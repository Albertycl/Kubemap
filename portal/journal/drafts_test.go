package journal

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/incidents"
	"kubemap/portal/store"
)

// The drafter works through the journal's sink, the draft survives in the
// database, and a person writes it up as an incident with its evidence.
func TestDraftWriteUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sink := DraftSink{Store: e.st}
	now := time.Date(2026, 9, 28, 7, 32, 0, 0, time.UTC) // 15:32 in Taipei
	critical := &protocol.HealthReport{Rules: []string{"node.disk.eviction"}, Findings: []protocol.HealthFinding{{
		Rule: "node.disk.eviction", Severity: protocol.SeverityCritical, Object: "Node/k8server", Title: "DiskPressure", Evidence: "4.2% free"}}}
	report := critical
	d := &incidents.Drafter{Sink: sink, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(tw{t}, nil)),
		Clusters: func(ctx context.Context) ([]store.Cluster, error) {
			c, err := e.st.GetCluster(ctx, e.active.ID)
			return []store.Cluster{c}, err
		},
		Check: func(context.Context, string) (*protocol.HealthReport, error) { return report, nil }}
	d.Tick(ctx) // Kubemap starts watching: the finding is already there
	now = now.Add(17 * time.Minute)
	report = &protocol.HealthReport{Rules: critical.Rules}
	d.Tick(ctx) // cleared

	drafts, err := sink.List(ctx)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("drafts %d %v", len(drafts), err)
	}
	dr := drafts[0]
	if dr.ClusterID != e.active.ID || len(dr.Timeline) < 2 || dr.Status != incidents.StatusResolved {
		t.Fatalf("draft %+v", dr)
	}
	if got, _ := sink.Latest(ctx, e.active.ID); got == nil || got.ID != dr.ID {
		t.Fatalf("latest %+v", got)
	}
	if got, _ := sink.Get(ctx, "none"); got != nil {
		t.Fatal("unknown draft found")
	}

	r := e.do(t, "POST", "/api/journal-drafts/"+dr.ID+"/write-up", "alice", map[string]string{"timeZone": "Asia/Taipei"})
	if r.code != 201 {
		t.Fatalf("write-up: %d %s", r.code, r.raw)
	}
	entry := decodeEntry(t, r)
	if entry.Source != DraftSource(dr.ID) || entry.Cluster != "k3s-home" || entry.Severity != "critical" || entry.Date != "2026-09-28" || entry.CreatedBy != "alice" {
		t.Fatalf("entry %+v", entry)
	}
	intro := entry.Sections[0].Text
	if !strings.Contains(intro, "Asia/Taipei") || !strings.Contains(intro, "real start is earlier") {
		t.Fatalf("intro %q", intro)
	}
	tl := entry.Find(SecTimeline).Timeline
	if tl[0].Time != "15:32" || tl[len(tl)-1].Time != "15:49" {
		t.Fatalf("timeline in Taipei time: %+v", tl)
	}
	if entry.Find(SecDetection).Text == "" || entry.Find(SecSTAR) == nil || entry.Find(SecDecisions) == nil {
		t.Fatalf("sections %+v", entry.Sections)
	}
	g := e.do(t, "GET", "/api/journal/"+entry.ID, "alice", nil)
	evs := g.body["evidence"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["kind"] != "findings" || evs[0].(map[string]any)["capturedBy"] != "kubemap" {
		t.Fatalf("evidence %v", evs)
	}
	var data struct{ Findings []map[string]any }
	b, _ := json.Marshal(evs[0].(map[string]any)["data"])
	json.Unmarshal(b, &data)
	if len(data.Findings) == 0 || data.Findings[0]["title"] != "DiskPressure" {
		t.Fatalf("evidence data %s", b)
	}
	// The export is a normal file name, not the draft source.
	x := e.do(t, "GET", "/api/journal/"+entry.ID+"/markdown", "alice", nil)
	if cd := x.hdr.Get("Content-Disposition"); !strings.Contains(cd, `filename="2026-09-28-`) {
		t.Fatalf("export name %q", cd)
	}

	// Once per draft; unknown drafts and zones are refused.
	if r := e.do(t, "POST", "/api/journal-drafts/"+dr.ID+"/write-up", "alice", map[string]string{}); r.code != 409 || r.body["id"] != entry.ID {
		t.Fatalf("second write-up: %d %s", r.code, r.raw)
	}
	if r := e.do(t, "POST", "/api/journal-drafts/none/write-up", "alice", map[string]string{}); r.code != 404 {
		t.Fatalf("unknown draft: %d", r.code)
	}
	if r := e.do(t, "POST", "/api/journal-drafts/"+dr.ID+"/write-up", "alice", map[string]string{"timeZone": "Mars/Base"}); r.code != 400 {
		t.Fatalf("bad zone: %d", r.code)
	}
	if a := e.audit(t); a[0].Action != "journal.from-draft" || a[0].ClusterID != e.active.ID {
		t.Fatalf("audit %+v", a[0])
	}
}

func TestFromDraftLongTimeline(t *testing.T) {
	d := incidents.Draft{ID: "draft-x", Title: "Pods crash", Severity: "warning", Started: time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC)}
	for i := 0; i < 450; i++ {
		d.Timeline = append(d.Timeline, incidents.Entry{Time: d.Started.Add(time.Duration(i) * time.Minute), Event: "line"})
	}
	r := FromDraft(d, time.UTC)
	tl := r.Find(SecTimeline).Timeline
	if len(tl) != maxRows || !strings.Contains(tl[maxRows/2].Event, "251 more lines") || r.Severity != "major" {
		t.Fatalf("%d rows, middle %+v, severity %s", len(tl), tl[maxRows/2], r.Severity)
	}
	if tl[len(tl)-1].Time != "09-29 06:29" {
		t.Fatalf("a later day shows its date: %q", tl[len(tl)-1].Time)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

// tw sends the drafter's log to the test log.
type tw struct{ t *testing.T }

func (w tw) Write(p []byte) (int, error) { w.t.Log(strings.TrimSpace(string(p))); return len(p), nil }
