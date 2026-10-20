// Package journal is the incident journal (KM-89, the interview view KM-91):
// a record of real problems and changes, what was done and why, with
// evidence snapshots from Kubemap. Records import from and export to the
// Markdown files in docs/incidents/ and docs/changes/.
//
// A record is a title, some metadata and an ordered list of sections. A
// section is either Markdown text or one of the structured kinds (timeline,
// decisions, follow-ups, STAR, change rows). Keeping the order and the
// headings makes a Markdown import export again byte for byte.
package journal

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Record kinds.
const (
	KindIncident = "incident"
	KindChanges  = "changes" // one day's cluster changes
)

// Section kinds. The text kinds hold Markdown in Section.Text; the others
// hold rows.
const (
	SecIntro     = "intro" // the text under the title (no heading)
	SecSummary   = "summary"
	SecDetection = "detection" // how it was found
	SecRootCause = "rootcause"
	SecActions   = "actions"
	SecResult    = "result"
	SecLessons   = "lessons"
	SecText      = "text" // any other heading

	SecTimeline  = "timeline"
	SecDecisions = "decisions"
	SecFollowUps = "followups"
	SecSTAR      = "star"
	SecChanges   = "changes"
)

// textKinds are the section kinds that hold Markdown text.
var textKinds = map[string]bool{
	SecIntro: true, SecSummary: true, SecDetection: true, SecRootCause: true,
	SecActions: true, SecResult: true, SecLessons: true, SecText: true,
}

// DefaultHeadings are the headings of new sections, by kind.
var DefaultHeadings = map[string]string{
	SecSummary:   "Summary",
	SecTimeline:  "Timeline",
	SecRootCause: "Root cause",
	SecDetection: "How it was found",
	SecActions:   "Actions",
	SecFollowUps: "Fixes and follow-ups",
	SecDecisions: "Actions and decisions",
	SecSTAR:      "Interview summary (STAR)",
	SecResult:    "Result",
	SecLessons:   "Lessons",
	SecChanges:   "Changes",
}

// Severities an incident may have; empty means not set.
var Severities = []string{"critical", "major", "minor"}

// Follow-up statuses.
const (
	StatusTodo  = "todo"
	StatusDoing = "doing"
	StatusDone  = "done"
)

// Record is an incident or a change record, without its store metadata.
type Record struct {
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Date     string    `json:"date"` // YYYY-MM-DD or empty
	Cluster  string    `json:"cluster"`
	Severity string    `json:"severity"`
	Sections []Section `json:"sections"`
}

// Section is one part of a record. Only the field of its kind is set.
type Section struct {
	Kind    string `json:"kind"`
	Heading string `json:"heading"` // empty: under the title (a first intro or change table)
	Text    string `json:"text,omitempty"`

	Timeline  []TimelineRow `json:"timeline,omitempty"`
	Decisions []Decision    `json:"decisions,omitempty"`
	FollowUps []FollowUp    `json:"followUps,omitempty"`
	STAR      *STAR         `json:"star,omitempty"`
	Changes   []Change      `json:"changes,omitempty"`
}

type TimelineRow struct {
	Time  string `json:"time"`
	Event string `json:"event"`
}

// Decision is one decision with its reason and what it cost.
type Decision struct {
	Decision string `json:"decision"`
	Why      string `json:"why"`
	TradeOff string `json:"tradeOff"`
}

type FollowUp struct {
	Status string `json:"status"` // todo, doing, done
	Owner  string `json:"owner"`
	Text   string `json:"text"`
}

// STAR is the interview summary: Situation, Task, Action, Result.
type STAR struct {
	Situation string `json:"situation"`
	Task      string `json:"task"`
	Action    string `json:"action"`
	Result    string `json:"result"`
}

// Change is one row of a change record.
type Change struct {
	Time    string `json:"time"`
	Change  string `json:"change"`
	Why     string `json:"why"`
	Checked string `json:"checked"` // how it was checked
	Undo    string `json:"undo"`
}

// Limits of one record.
const (
	maxTitle    = 200
	maxShort    = 100 // cluster, time, owner
	maxSections = 50
	maxRows     = 200
	maxText     = 64 << 10
	maxCell     = 8 << 10
)

// Validate checks a record before it is stored. It also fills a missing
// heading with the kind's default and trims the metadata.
func (r *Record) Validate() error {
	r.Title = strings.TrimSpace(r.Title)
	r.Date = strings.TrimSpace(r.Date)
	r.Cluster = strings.TrimSpace(r.Cluster)
	r.Severity = strings.TrimSpace(r.Severity)
	switch r.Kind {
	case KindIncident, KindChanges:
	default:
		return fmt.Errorf("kind must be %q or %q", KindIncident, KindChanges)
	}
	if r.Title == "" {
		return fmt.Errorf("the title is empty")
	}
	if err := line("title", r.Title, maxTitle); err != nil {
		return err
	}
	if r.Date != "" {
		if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
			return fmt.Errorf("the date must be YYYY-MM-DD")
		}
	}
	if err := line("cluster", r.Cluster, maxShort); err != nil {
		return err
	}
	if r.Severity != "" && !contains(Severities, r.Severity) {
		return fmt.Errorf("severity must be empty or one of %s", strings.Join(Severities, ", "))
	}
	if len(r.Sections) > maxSections {
		return fmt.Errorf("at most %d sections", maxSections)
	}
	if r.Sections == nil {
		r.Sections = []Section{}
	}
	for i := range r.Sections {
		if err := r.Sections[i].validate(i); err != nil {
			return fmt.Errorf("section %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Section) validate(i int) error {
	s.Heading = strings.TrimSpace(s.Heading)
	if s.Kind == SecIntro {
		if i != 0 {
			return fmt.Errorf("the intro (text under the title) must come first")
		}
		if s.Heading != "" {
			return fmt.Errorf("the intro has no heading")
		}
	} else if s.Heading == "" && (i > 0 || s.Kind != SecChanges) { // a change table may stand under the title
		s.Heading = DefaultHeadings[s.Kind]
		if s.Heading == "" {
			return fmt.Errorf("the heading is empty")
		}
	}
	if err := line("heading", s.Heading, maxTitle); err != nil {
		return err
	}
	rows := 0
	var cells []string
	switch {
	case textKinds[s.Kind]:
		if len(s.Text) > maxText {
			return fmt.Errorf("the text is longer than %d KiB", maxText>>10)
		}
		if !utf8.ValidString(s.Text) {
			return fmt.Errorf("the text is not valid UTF-8")
		}
		s.Text = strings.Trim(strings.ReplaceAll(s.Text, "\r\n", "\n"), "\n")
		if headingInText(s.Text) {
			return fmt.Errorf("the text has a \"## \" heading line; add a new section instead")
		}
	case s.Kind == SecTimeline:
		rows = len(s.Timeline)
		for _, r := range s.Timeline {
			cells = append(cells, r.Time, r.Event)
		}
	case s.Kind == SecDecisions:
		rows = len(s.Decisions)
		for _, r := range s.Decisions {
			cells = append(cells, r.Decision, r.Why, r.TradeOff)
		}
	case s.Kind == SecChanges:
		rows = len(s.Changes)
		for _, r := range s.Changes {
			cells = append(cells, r.Time, r.Change, r.Why, r.Checked, r.Undo)
		}
	case s.Kind == SecFollowUps:
		rows = len(s.FollowUps)
		for j, f := range s.FollowUps {
			switch f.Status {
			case StatusTodo, StatusDoing, StatusDone:
			default:
				return fmt.Errorf("follow-up %d: status must be todo, doing or done", j+1)
			}
			if err := line("owner", f.Owner, maxShort); err != nil {
				return err
			}
			if strings.Contains(f.Owner, ")") {
				return fmt.Errorf("follow-up %d: the owner cannot contain ')'", j+1)
			}
			if strings.Contains(f.Text, "\n") {
				return fmt.Errorf("follow-up %d: the text must be one line", j+1)
			}
			cells = append(cells, f.Text)
		}
	case s.Kind == SecSTAR:
		if s.STAR == nil {
			s.STAR = &STAR{}
		}
		for _, v := range []string{s.STAR.Situation, s.STAR.Task, s.STAR.Action, s.STAR.Result} {
			if strings.Contains(v, "\n") {
				return fmt.Errorf("each STAR part must be one paragraph (no line breaks)")
			}
			cells = append(cells, v)
		}
	default:
		return fmt.Errorf("unknown section kind %q", s.Kind)
	}
	if rows > maxRows {
		return fmt.Errorf("at most %d rows", maxRows)
	}
	for _, c := range cells {
		if len(c) > maxCell {
			return fmt.Errorf("a cell is longer than %d KiB", maxCell>>10)
		}
		if !utf8.ValidString(c) {
			return fmt.Errorf("a cell is not valid UTF-8")
		}
	}
	return nil
}

// headingInText reports a "## " line outside code fences: it would start a
// new section when the Markdown is imported again.
func headingInText(text string) bool {
	fence := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(l, "## ") {
			return true
		}
	}
	return false
}

// line checks a one-line field.
func line(name, v string, max int) error {
	if len(v) > max {
		return fmt.Errorf("the %s is longer than %d characters", name, max)
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 || !utf8.ValidString(v) {
		return fmt.Errorf("the %s must be one line of text", name)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Find returns the first section of a kind, or nil.
func (r *Record) Find(kind string) *Section {
	for i := range r.Sections {
		if r.Sections[i].Kind == kind {
			return &r.Sections[i]
		}
	}
	return nil
}
