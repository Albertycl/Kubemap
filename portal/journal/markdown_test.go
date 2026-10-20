package journal

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The 28 September incident imports with its timeline, its 6 decisions and
// its STAR summary, and exports byte for byte (KM-89 acceptance 2).
func TestDiskPressureIncident(t *testing.T) {
	md := readDoc(t, "docs/incidents/2026-09-28-disk-pressure.md")
	r, err := Parse(md, "2026-09-28-disk-pressure.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindIncident || r.Title != "disk pressure evicted every app on k8server" || r.Date != "2026-09-28" || r.Cluster != "k8server" {
		t.Fatalf("metadata: %q %q %q %q", r.Kind, r.Title, r.Date, r.Cluster)
	}
	var kinds []string
	for _, s := range r.Sections {
		kinds = append(kinds, s.Kind)
	}
	want := []string{SecIntro, SecSummary, SecTimeline, SecRootCause, SecDetection, SecFollowUps, SecDecisions, SecSTAR, SecLessons}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("sections %v, want %v", kinds, want)
	}
	tl := r.Find(SecTimeline).Timeline
	if len(tl) != 9 || tl[0] != (TimelineRow{"07:32", "DiskPressure=True. The eviction manager starts to reclaim ephemeral-storage."}) || tl[8].Time != "08:12" {
		t.Fatalf("timeline: %d rows %+v", len(tl), tl)
	}
	ds := r.Find(SecDecisions).Decisions
	if len(ds) != 6 || ds[0].Decision != "Stop all build jobs first." || !strings.HasPrefix(ds[3].TradeOff, "Recovery waited") {
		t.Fatalf("decisions: %d %+v", len(ds), ds)
	}
	st := r.Find(SecSTAR).STAR
	if !strings.HasPrefix(st.Situation, "On a single-node K3s cluster") || !strings.HasPrefix(st.Task, "Restore service") ||
		!strings.HasPrefix(st.Action, "I stopped the builds") || !strings.HasPrefix(st.Result, "All pods ran again") {
		t.Fatalf("STAR: %+v", st)
	}
	fu := r.Find(SecFollowUps).FollowUps
	if len(fu) != 5 || fu[0].Status != StatusDone || fu[2].Status != StatusTodo || fu[4].Owner != "Kubemap" || fu[3].Owner != "" {
		t.Fatalf("follow-ups: %+v", fu)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := Render(r); got != md {
		t.Fatalf("export differs from the file:\n%s", firstDiff(got, md))
	}
}

func TestChangeRecord(t *testing.T) {
	md := readDoc(t, "docs/changes/2026-09-28.md")
	r, err := Parse(md, "2026-09-28.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindChanges || r.Title != "Cluster changes on 28 September 2026 (GMT+8)" || r.Date != "2026-09-28" {
		t.Fatalf("metadata: %q %q %q", r.Kind, r.Title, r.Date)
	}
	if len(r.Sections) != 2 || r.Sections[0].Kind != SecChanges || r.Sections[1].Kind != SecText || r.Sections[1].Heading != "New finding (no change made)" {
		t.Fatalf("sections: %+v", r.Sections)
	}
	cs := r.Sections[0].Changes
	if len(cs) != 4 || cs[0].Time != "12:10" || !strings.HasPrefix(cs[2].Undo, "`kubectl apply -f docs/incidents/backups/") {
		t.Fatalf("changes: %+v", cs)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := Render(r); got != md {
		t.Fatalf("export differs from the file:\n%s", firstDiff(got, md))
	}
}

// A record made in the portal exports and imports again to the same record.
func TestRoundTrip(t *testing.T) {
	r := Record{Kind: KindIncident, Title: "API server slow", Date: "2026-10-01", Cluster: "k8server", Severity: "minor",
		Sections: []Section{
			{Kind: SecSummary, Heading: "Summary", Text: "etcd was slow.\n\nThe disk was busy."},
			{Kind: SecTimeline, Timeline: []TimelineRow{{"10:00", "Alert | p99 > 1s"}, {"10:05", "Two lines:\nhere"}}},
			{Kind: SecDecisions, Decisions: []Decision{{"Wait", "Safe", "Slow"}}},
			{Kind: SecFollowUps, FollowUps: []FollowUp{{StatusDoing, "sre team", "Move etcd"}, {StatusDone, "", "Alert added"}}},
			{Kind: SecSTAR, STAR: &STAR{Situation: "S", Task: "T", Action: "A", Result: "R"}},
			{Kind: SecText, Heading: "Notes", Text: "```\n## not a heading\n```"},
		}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	md := Render(r)
	back, err := Parse(md, "")
	if err != nil {
		t.Fatal(err)
	}
	if intro := back.Sections[0]; intro.Kind != SecIntro || intro.Text != "Date: 1 October 2026. Cluster `k8server`. Severity: minor." {
		t.Fatalf("intro: %+v", intro)
	}
	back.Sections = back.Sections[1:]
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip:\n got %+v\nwant %+v\n%s", back, r, md)
	}
}

// Text that does not fit a structured layout stays text, unchanged.
func TestParseKeepsHandWrittenText(t *testing.T) {
	md := "# Incident: x\n\n## Timeline\n\n- 10:00 it broke\n- 10:05 fixed\n\n## Actions and decisions\n\n| # | Decision | Why | Trade-off |\n|---|---|---|---|\n| 3 | skipped numbers | a | b |\n"
	r, err := Parse(md, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Sections[0].Kind != SecText || r.Sections[1].Kind != SecText {
		t.Fatalf("kinds: %s %s", r.Sections[0].Kind, r.Sections[1].Kind)
	}
	if got := Render(r); got != md {
		t.Fatalf("export differs:\n%s", firstDiff(got, md))
	}
	if _, err := Parse("no title here", ""); err == nil {
		t.Fatal("a file without a title parsed")
	}
}

func TestValidate(t *testing.T) {
	ok := func() Record {
		return Record{Kind: KindIncident, Title: "t", Sections: []Section{{Kind: SecSummary, Text: "x"}}}
	}
	for name, mut := range map[string]func(*Record){
		"kind":          func(r *Record) { r.Kind = "other" },
		"empty title":   func(r *Record) { r.Title = " " },
		"title newline": func(r *Record) { r.Title = "a\nb" },
		"title escape":  func(r *Record) { r.Title = "a\x1b[31mb" },
		"date":          func(r *Record) { r.Date = "28/09/2026" },
		"severity":      func(r *Record) { r.Severity = "sev1" },
		"section kind":  func(r *Record) { r.Sections[0].Kind = "x" },
		"intro not first": func(r *Record) {
			r.Sections = append(r.Sections, Section{Kind: SecIntro, Text: "x"})
		},
		"heading in text": func(r *Record) { r.Sections[0].Text = "a\n## b" },
		"status":          func(r *Record) { r.Sections[0] = Section{Kind: SecFollowUps, FollowUps: []FollowUp{{Status: "x"}}} },
		"owner paren": func(r *Record) {
			r.Sections[0] = Section{Kind: SecFollowUps, FollowUps: []FollowUp{{Status: StatusTodo, Owner: "a)"}}}
		},
		"star newline": func(r *Record) { r.Sections[0] = Section{Kind: SecSTAR, STAR: &STAR{Task: "a\nb"}} },
		"too many rows": func(r *Record) {
			r.Sections[0] = Section{Kind: SecTimeline, Timeline: make([]TimelineRow, maxRows+1)}
		},
	} {
		r := ok()
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	r := ok()
	r.Sections = append(r.Sections, Section{Kind: SecTimeline})
	if err := r.Validate(); err != nil || r.Sections[1].Heading != "Timeline" {
		t.Fatalf("default heading: %v %q", err, r.Sections[1].Heading)
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + ":\n got: " + x + "\nwant: " + y
		}
	}
	return "(same lines)"
}
