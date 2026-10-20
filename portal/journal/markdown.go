package journal

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The Markdown form of a record:
//
//	# Incident: <title>          (a change record: # <title>)
//
//	<intro text>
//
//	## <heading>
//
//	<body>
//
// Blocks are separated by one empty line and the file ends with one
// newline. A structured section (timeline, decisions, ...) is a table or a
// list in a fixed layout. Parse only takes a section as structured when
// rendering it again gives the same text; otherwise it stays Markdown text,
// so nothing written by hand is lost.

const incidentPrefix = "Incident: "

// Parse reads a Markdown incident or change record. source is the file
// name; a date at its start (2026-09-28-...) is used when the text has none.
func Parse(md, source string) (Record, error) {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || !strings.HasPrefix(lines[i], "# ") {
		return Record{}, fmt.Errorf("the first line must be a title (\"# Incident: ...\")")
	}
	title := strings.TrimSpace(strings.TrimPrefix(lines[i], "# "))
	r := Record{Kind: KindIncident, Title: title}
	if t, ok := strings.CutPrefix(title, incidentPrefix); ok {
		r.Title = strings.TrimSpace(t)
	}

	// Cut into blocks at "## " lines, outside code fences.
	type block struct {
		heading string
		body    []string
		top     bool
	}
	blocks := []block{{top: true}}
	fence := false
	for _, l := range lines[i+1:] {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(l, "## ") {
			blocks = append(blocks, block{heading: strings.TrimSpace(strings.TrimPrefix(l, "## "))})
			continue
		}
		blocks[len(blocks)-1].body = append(blocks[len(blocks)-1].body, l)
	}
	for _, b := range blocks {
		body := strings.Trim(strings.Join(b.body, "\n"), "\n")
		if b.top && body == "" {
			continue
		}
		s := parseSection(b.heading, body, b.top)
		if s.Kind == SecChanges {
			r.Kind = KindChanges
		}
		r.Sections = append(r.Sections, s)
	}
	if r.Kind == KindChanges {
		r.Title = title // change records have no "Incident: " prefix
	}
	meta := title
	if len(r.Sections) > 0 && r.Sections[0].Kind == SecIntro {
		meta += "\n" + r.Sections[0].Text
	}
	r.Date = findDate(meta)
	if r.Date == "" {
		r.Date = findDate(strings.SplitN(source, "_", 2)[0])
	}
	if m := clusterRE.FindStringSubmatch(meta); m != nil {
		r.Cluster = m[1]
	}
	if m := severityRE.FindStringSubmatch(meta); m != nil {
		if s := strings.ToLower(m[1]); contains(Severities, s) {
			r.Severity = s
		}
	}
	if r.Sections == nil {
		r.Sections = []Section{}
	}
	return r, nil
}

var (
	clusterRE  = regexp.MustCompile("(?i)cluster `([^`\n]+)`")
	severityRE = regexp.MustCompile(`(?i)severity: *([a-z]+)`)
	isoDateRE  = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	longDateRE = regexp.MustCompile(`\b(\d{1,2} (?:January|February|March|April|May|June|July|August|September|October|November|December) \d{4})\b`)
)

// findDate returns the first date in s as YYYY-MM-DD.
func findDate(s string) string {
	if m := longDateRE.FindStringSubmatch(s); m != nil {
		if t, err := time.Parse("2 January 2006", m[1]); err == nil {
			return t.Format(time.DateOnly)
		}
	}
	if m := isoDateRE.FindStringSubmatch(s); m != nil {
		if _, err := time.Parse(time.DateOnly, m[1]); err == nil {
			return m[1]
		}
	}
	return ""
}

// headingKinds maps a heading (lower case) to its section kind.
var headingKinds = []struct {
	prefix, kind string
}{
	{"timeline", SecTimeline},
	{"actions and decisions", SecDecisions},
	{"decisions", SecDecisions},
	{"interview summary", SecSTAR},
	{"star", SecSTAR},
	{"fixes and follow-ups", SecFollowUps},
	{"follow-ups", SecFollowUps},
	{"follow ups", SecFollowUps},
	{"summary", SecSummary},
	{"root cause", SecRootCause},
	{"how it was found", SecDetection},
	{"detection", SecDetection},
	{"actions", SecActions},
	{"what was done", SecActions},
	{"result", SecResult},
	{"lessons", SecLessons},
	{"what this shows", SecLessons},
	{"what we learned", SecLessons},
	{"changes", SecChanges},
}

func kindOf(heading string) string {
	h := strings.ToLower(heading)
	for _, k := range headingKinds {
		if strings.HasPrefix(h, k.prefix) {
			return k.kind
		}
	}
	return SecText
}

// parseSection turns one block into a section: structured if its body has
// the layout of its kind and renders back unchanged, text otherwise.
func parseSection(heading, body string, top bool) Section {
	if s, ok := parseChanges(body); ok { // a change table, under any heading
		s.Heading = heading
		return s
	}
	kind := SecIntro
	if !top {
		kind = kindOf(heading)
	}
	var s Section
	ok := false
	switch kind {
	case SecTimeline:
		s, ok = parseTimeline(body)
	case SecDecisions:
		s, ok = parseDecisions(body)
	case SecFollowUps:
		s, ok = parseFollowUps(body)
	case SecSTAR:
		s, ok = parseSTAR(body)
	case SecChanges:
		kind = SecText // a "Changes" heading without the change table
	}
	if ok {
		s.Heading = heading
		if renderBody(s) == body {
			return s
		}
	}
	if !textKinds[kind] {
		kind = SecText
	}
	return Section{Kind: kind, Heading: heading, Text: body}
}

// table reads a Markdown table with the given header. It returns the rows'
// cells, or false if body is not exactly that table.
func table(body string, header []string) ([][]string, bool) {
	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		return nil, false
	}
	h := cells(lines[0])
	if len(h) != len(header) {
		return nil, false
	}
	for i := range h {
		if !strings.EqualFold(h[i], header[i]) {
			return nil, false
		}
	}
	if strings.Trim(strings.ReplaceAll(lines[1], " ", ""), "|-:") != "" {
		return nil, false
	}
	var rows [][]string
	for _, l := range lines[2:] {
		c := cells(l)
		if len(c) != len(header) {
			return nil, false
		}
		rows = append(rows, c)
	}
	return rows, true
}

// cells splits "| a | b |" into its unescaped cells; nil if l is no row.
func cells(l string) []string {
	l = strings.TrimSpace(l)
	if len(l) < 2 || l[0] != '|' || l[len(l)-1] != '|' {
		return nil
	}
	l = l[1 : len(l)-1]
	var out []string
	var cur strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l) && l[i+1] == '|':
			cur.WriteByte('|')
			i++
		case l[i] == '|':
			out = append(out, unescapeCell(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(out, unescapeCell(cur.String()))
}

func unescapeCell(c string) string { return strings.ReplaceAll(strings.TrimSpace(c), "<br>", "\n") }

func escapeCell(c string) string {
	c = strings.ReplaceAll(c, "|", `\|`)
	return strings.ReplaceAll(c, "\n", "<br>")
}

func row(cs ...string) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = escapeCell(c)
	}
	return "| " + strings.Join(out, " | ") + " |"
}

var (
	timelineHeader = []string{"Time", "Event"}
	decisionHeader = []string{"#", "Decision", "Why", "Trade-off"}
	changeHeader   = []string{"Time", "Change", "Why", "How it was checked", "Undo"}
)

func parseTimeline(body string) (Section, bool) {
	rows, ok := table(body, timelineHeader)
	s := Section{Kind: SecTimeline, Timeline: []TimelineRow{}}
	for _, r := range rows {
		s.Timeline = append(s.Timeline, TimelineRow{Time: r[0], Event: r[1]})
	}
	return s, ok
}

func parseDecisions(body string) (Section, bool) {
	rows, ok := table(body, decisionHeader)
	s := Section{Kind: SecDecisions, Decisions: []Decision{}}
	for _, r := range rows {
		s.Decisions = append(s.Decisions, Decision{Decision: r[1], Why: r[2], TradeOff: r[3]})
	}
	return s, ok
}

func parseChanges(body string) (Section, bool) {
	rows, ok := table(body, changeHeader)
	if !ok {
		return Section{}, false
	}
	s := Section{Kind: SecChanges, Changes: []Change{}}
	for _, r := range rows {
		s.Changes = append(s.Changes, Change{Time: r[0], Change: r[1], Why: r[2], Checked: r[3], Undo: r[4]})
	}
	return s, renderBody(s) == body
}

var (
	followUpRE = regexp.MustCompile(`^- (Done|To do|In progress)(?: \(([^)\n]+)\))?: (.*)$`)
	starRE     = regexp.MustCompile(`^- \*\*(Situation|Task|Action|Result):\*\* (.*)$`)
)

var statusLabel = map[string]string{StatusDone: "Done", StatusTodo: "To do", StatusDoing: "In progress"}

func parseFollowUps(body string) (Section, bool) {
	s := Section{Kind: SecFollowUps, FollowUps: []FollowUp{}}
	if body == "" {
		return s, true
	}
	for _, l := range strings.Split(body, "\n") {
		m := followUpRE.FindStringSubmatch(l)
		if m == nil {
			return s, false
		}
		status := StatusTodo
		for k, v := range statusLabel {
			if v == m[1] {
				status = k
			}
		}
		s.FollowUps = append(s.FollowUps, FollowUp{Status: status, Owner: m[2], Text: m[3]})
	}
	return s, true
}

func parseSTAR(body string) (Section, bool) {
	st := &STAR{}
	s := Section{Kind: SecSTAR, STAR: st}
	if body == "" {
		return s, true
	}
	for _, l := range strings.Split(body, "\n") {
		m := starRE.FindStringSubmatch(l)
		if m == nil {
			return s, false
		}
		switch m[1] {
		case "Situation":
			st.Situation = m[2]
		case "Task":
			st.Task = m[2]
		case "Action":
			st.Action = m[2]
		case "Result":
			st.Result = m[2]
		}
	}
	return s, true
}

// Render writes a record as Markdown.
func Render(r Record) string {
	title := r.Title
	if r.Kind == KindIncident {
		title = incidentPrefix + title
	}
	parts := []string{"# " + title}
	hasIntro := len(r.Sections) > 0 && r.Sections[0].Kind == SecIntro
	if !hasIntro {
		if m := metaLine(r); m != "" {
			parts = append(parts, m)
		}
	}
	for _, s := range r.Sections {
		if s.Heading != "" {
			parts = append(parts, "## "+s.Heading)
		}
		if b := renderBody(s); b != "" {
			parts = append(parts, b)
		}
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// metaLine is the intro written for a record that has none, so the
// metadata survives an export and a new import. A date the title already
// holds is not repeated.
func metaLine(r Record) string {
	var p []string
	if r.Date != "" && findDate(r.Title) != r.Date {
		if t, err := time.Parse(time.DateOnly, r.Date); err == nil {
			p = append(p, "Date: "+t.Format("2 January 2006")+".")
		}
	}
	if r.Cluster != "" && !strings.Contains(r.Cluster, "`") {
		p = append(p, "Cluster `"+r.Cluster+"`.")
	}
	if r.Severity != "" {
		p = append(p, "Severity: "+r.Severity+".")
	}
	return strings.Join(p, " ")
}

func renderBody(s Section) string {
	var out []string
	switch s.Kind {
	case SecTimeline:
		out = append(out, row(timelineHeader...), "|---|---|")
		for _, t := range s.Timeline {
			out = append(out, row(t.Time, t.Event))
		}
	case SecDecisions:
		out = append(out, row(decisionHeader...), "|---|---|---|---|")
		for i, d := range s.Decisions {
			out = append(out, row(fmt.Sprint(i+1), d.Decision, d.Why, d.TradeOff))
		}
	case SecChanges:
		out = append(out, row(changeHeader...), "|---|---|---|---|---|")
		for _, c := range s.Changes {
			out = append(out, row(c.Time, c.Change, c.Why, c.Checked, c.Undo))
		}
	case SecFollowUps:
		for _, f := range s.FollowUps {
			l := "- " + statusLabel[f.Status]
			if f.Owner != "" {
				l += " (" + f.Owner + ")"
			}
			out = append(out, l+": "+f.Text)
		}
	case SecSTAR:
		if s.STAR != nil {
			for _, p := range [][2]string{{"Situation", s.STAR.Situation}, {"Task", s.STAR.Task}, {"Action", s.STAR.Action}, {"Result", s.STAR.Result}} {
				if p[1] != "" {
					out = append(out, "- **"+p[0]+":** "+p[1])
				}
			}
		}
	default:
		return s.Text
	}
	return strings.Join(out, "\n")
}
