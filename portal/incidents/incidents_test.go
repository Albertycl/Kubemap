package incidents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

var (
	t0      = time.Date(2026, 9, 27, 23, 30, 0, 0, time.UTC) // 07:30 GMT+8
	cluster = store.Cluster{ID: "c-lab", Name: "k8server", Status: store.StatusActive}
)

func disk(freePct float64) protocol.HealthFinding {
	return protocol.HealthFinding{Rule: "node.disk.eviction", Severity: protocol.SeverityCritical, Object: "Node/k8server",
		Title:    fmt.Sprintf("DiskPressure: the kubelet is evicting pods (%.1f%% free)", freePct),
		Evidence: "root filesystem (nodefs, /): … keeps evicting until 15% free"}
}

func evicted(items ...string) protocol.HealthFinding {
	return protocol.HealthFinding{Rule: "pod.evicted", Severity: protocol.SeverityWarning, Object: "Cluster",
		Title: fmt.Sprintf("%d pods were evicted", len(items)), Evidence: "Stopped by the kubelet", Items: items, Count: len(items)}
}

func crash(ns, pod string) protocol.HealthFinding {
	return protocol.HealthFinding{Rule: "pod.crashloop", Severity: protocol.SeverityCritical, Object: "Pod/" + ns + "/" + pod,
		Title: "CrashLoopBackOff: app", Evidence: "restarts 4"}
}

func pods(n int, ns string) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("%s/p-%02d", ns, i))
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newDrafter() (*Drafter, *MemorySink, *clock) {
	s := &MemorySink{}
	c := &clock{t: t0}
	return &Drafter{Sink: s, Now: c.now, IgnoreNamespaces: DefaultIgnoreNamespaces,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, s, c
}

func observe(t *testing.T, d *Drafter, fs ...protocol.HealthFinding) {
	t.Helper()
	if err := d.Observe(context.Background(), cluster, &protocol.HealthReport{Findings: fs}); err != nil {
		t.Fatal(err)
	}
}

func events(dr *Draft) string {
	var b strings.Builder
	for _, e := range dr.Timeline {
		b.WriteString(e.Time.Format("15:04") + " " + e.Event + "\n")
	}
	return b.String()
}

// The 28 Sep 2026 incident, replayed: disk pressure, evictions growing,
// recovery, then a crash loop in the same episode, then a new episode.
func TestEpisode(t *testing.T) {
	d, s, c := newDrafter()
	ctx := context.Background()
	lab := crash("kubemap-demo", "checkout-1") // a practice lab: never a trigger

	observe(t, d, lab, protocol.HealthFinding{Rule: "pod.no-memory-limit", Severity: protocol.SeverityWarning, Object: "Cluster", Title: "33 pods …"})
	if all, _ := s.List(ctx); len(all) != 0 {
		t.Fatalf("healthy cluster (only a lab and a warning) opened a draft: %+v", all)
	}

	c.t = t0.Add(2 * time.Minute) // 07:32
	observe(t, d, lab, disk(4.2), evicted(pods(5, "argocd")...))
	dr, _ := s.Latest(ctx, cluster.ID)
	if dr == nil || dr.Status != StatusOpen || dr.PresentAtStart || dr.Severity != protocol.SeverityCritical || len(dr.Timeline) != 2 || len(dr.Evidence) != 2 {
		t.Fatalf("draft after the first trigger: %+v\n%s", dr, events(dr))
	}
	if dr.Title != "DiskPressure: the kubelet is evicting pods (4.2% free) (Node/k8server), and 1 more problem(s)" {
		t.Errorf("title %q", dr.Title)
	}
	if !strings.Contains(dr.Detection, "rule node.disk.eviction on Node/k8server, 2026-09-27 23:32 UTC") {
		t.Errorf("detection %q", dr.Detection)
	}
	if dr.ID != fmt.Sprintf("draft-c-lab-%d", t0.Add(2*time.Minute).Unix()) || dr.Evidence[0].Link != "#/clusters/c-lab/problems" {
		t.Errorf("id %s link %s", dr.ID, dr.Evidence[0].Link)
	}

	c.t = t0.Add(5 * time.Minute) // live numbers change, nothing else: no new line
	observe(t, d, lab, disk(6.0), evicted(pods(5, "argocd")...))
	if dr2, _ := s.Latest(ctx, cluster.ID); len(dr2.Timeline) != 2 || dr2.Active["node.disk.eviction|Node/k8server"].Title == dr.Active["node.disk.eviction|Node/k8server"].Title {
		t.Errorf("a title change alone must not add a line: %s", events(dr2))
	}

	c.t = t0.Add(10 * time.Minute) // evictions grow (lab pods in the list are dropped)
	observe(t, d, disk(13.7), evicted(append(pods(40, "argocd"), "kubemap-demo/web-1")...))
	dr, _ = s.Latest(ctx, cluster.ID)
	if !strings.Contains(events(dr), "Changed: 5 → 40, 40 pods were evicted") {
		t.Errorf("growth not recorded:\n%s", events(dr))
	}

	c.t = t0.Add(19 * time.Minute) // 07:49 DiskPressure clears
	observe(t, d, evicted(pods(40, "argocd")...))
	c.t = t0.Add(40 * time.Minute) // the evicted pods are cleaned up
	observe(t, d, lab)
	dr, _ = s.Latest(ctx, cluster.ID)
	if dr.Status != StatusResolved || !dr.Resolved.Equal(t0.Add(40*time.Minute)) || len(dr.Active) != 0 {
		t.Fatalf("not resolved: %+v\n%s", dr, events(dr))
	}
	// Times are UTC in the draft: 23:32 UTC is 07:32 GMT+8.
	want := []string{"23:32 First seen: DiskPressure", "23:32 First seen: 5 pods were evicted", "23:40 Changed: 5 → 40",
		"23:49 Cleared: DiskPressure", "00:10 Cleared: 40 pods were evicted", "00:10 Every triggering finding has cleared"}
	got := events(dr)
	if n := strings.Count(got, "\n"); n != len(want) {
		t.Errorf("%d lines, want %d:\n%s", n, len(want), got)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}

	c.t = t0.Add(50 * time.Minute) // within 30 min of resolving: same episode
	observe(t, d, crash("sre-lab-app", "url-shortener-1"))
	dr2, _ := s.Latest(ctx, cluster.ID)
	if dr2.ID != dr.ID || dr2.Status != StatusOpen || !strings.Contains(events(dr2), "Findings are back within 30m0s") {
		t.Errorf("reopen: %+v\n%s", dr2, events(dr2))
	}

	c.t = t0.Add(55 * time.Minute)
	observe(t, d)
	c.t = t0.Add(90 * time.Minute) // 35 min after resolving: a new episode
	observe(t, d, crash("sre-lab-app", "url-shortener-1"))
	all, _ := s.List(ctx)
	if len(all) != 2 || all[0].ID == dr.ID || all[0].PresentAtStart {
		t.Errorf("new episode: %d drafts, newest %+v", len(all), all[0])
	}
}

// Findings present at the first check are marked: the real start is earlier.
func TestPresentAtStart(t *testing.T) {
	d, s, _ := newDrafter()
	observe(t, d, disk(4), crash("sre-lab-app", "a"))
	dr, _ := s.Latest(context.Background(), cluster.ID)
	if !dr.PresentAtStart || strings.Count(events(dr), "already present when Kubemap started watching") != 2 ||
		!strings.Contains(dr.Detection, "it began earlier") || strings.Contains(dr.Title, "already present") {
		t.Errorf("%+v\n%s", dr, events(dr))
	}
}

func TestIgnoredNamespaces(t *testing.T) {
	d, s, _ := newDrafter()
	observe(t, d, crash("kubemap-demo", "oom-1"), evicted("kubemap-demo/x", "kubemap-demo/y"),
		protocol.HealthFinding{Rule: "pod.pending", Severity: protocol.SeverityWarning, Object: "Pod/sre-lab-app/p"})
	if all, _ := s.List(context.Background()); len(all) != 0 {
		t.Fatalf("labs and warnings opened a draft: %+v", all)
	}
	d.IgnoreNamespaces = nil // e2e: the labs are the triggers
	observe(t, d, crash("kubemap-demo", "oom-1"))
	if all, _ := s.List(context.Background()); len(all) != 1 {
		t.Fatalf("without ignored namespaces: %d drafts", len(all))
	}
}

func TestCaps(t *testing.T) {
	d, s, c := newDrafter()
	for i := range MaxTimeline + 50 {
		c.t = t0.Add(time.Duration(i) * time.Minute)
		observe(t, d, evicted(pods(i%7+1, "argocd")...))
	}
	dr, _ := s.Latest(context.Background(), cluster.ID)
	if len(dr.Timeline) != MaxTimeline || len(dr.Evidence) != MaxEvidence {
		t.Errorf("timeline %d, evidence %d", len(dr.Timeline), len(dr.Evidence))
	}
}

// An agent that cannot be asked is skipped: its findings are not "cleared".
func TestTickSkipsFailedChecks(t *testing.T) {
	d, s, _ := newDrafter()
	fail := false
	d.Check = func(context.Context, string) (*protocol.HealthReport, error) {
		if fail {
			return nil, errors.New("agent_offline")
		}
		return &protocol.HealthReport{Findings: []protocol.HealthFinding{disk(4)}}, nil
	}
	d.Clusters = func(context.Context) ([]store.Cluster, error) { return []store.Cluster{cluster}, nil }
	d.Tick(context.Background())
	fail = true
	d.Tick(context.Background())
	dr, _ := s.Latest(context.Background(), cluster.ID)
	if dr == nil || dr.Status != StatusOpen || len(dr.Active) != 1 {
		t.Fatalf("after a failed check: %+v", dr)
	}
}

func TestAPI(t *testing.T) {
	d, s, _ := newDrafter()
	observe(t, d, disk(4))
	mux := http.NewServeMux()
	guarded := 0
	(&API{Sink: s}).Routes(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; h.ServeHTTP(w, r) })
	})(mux)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	var list struct{ Drafts []Draft }
	w := get("/api/incident-drafts")
	if json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || len(list.Drafts) != 1 || list.Drafts[0].Evidence != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var one struct{ Draft Draft }
	w = get("/api/incident-drafts/" + list.Drafts[0].ID)
	if json.Unmarshal(w.Body.Bytes(), &one); w.Code != 200 || len(one.Draft.Evidence) != 1 || one.Draft.Evidence[0].Finding.Rule != "node.disk.eviction" {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w = get("/api/incident-drafts/nope"); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	if guarded != 3 {
		t.Errorf("require wrapped %d of 3", guarded)
	}
}
