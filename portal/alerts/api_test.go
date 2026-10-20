package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/incidents"
)

func TestAlerts(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 7, 32, 0, 0, time.UTC)
	crit := func(rule, obj, title string) incidents.Active {
		return incidents.Active{Rule: rule, Object: obj, Severity: protocol.SeverityCritical, Title: title}
	}
	drafts := []incidents.Draft{
		// Cluster a: an older resolved draft and a newer open one.
		{ID: "d-a-1", ClusterID: "a", ClusterName: "k8server", Status: incidents.StatusResolved, Started: t0.Add(-time.Hour),
			Active: map[string]incidents.Active{"x|y": crit("x", "y", "old")}},
		{ID: "d-a-2", ClusterID: "a", ClusterName: "k8server", Status: incidents.StatusOpen, Started: t0,
			Active: map[string]incidents.Active{
				"node.disk.eviction|Node/k8server": crit("node.disk.eviction", "Node/k8server", "DiskPressure"),
				"pod.evicted|Cluster":              {Rule: "pod.evicted", Object: "Cluster", Severity: protocol.SeverityWarning, Title: "41 pods were evicted"},
			}},
		// Cluster b: open, but only a warning is active now: no alert.
		{ID: "d-b", ClusterID: "b", ClusterName: "lab", Status: incidents.StatusOpen, Started: t0,
			Active: map[string]incidents.Active{"pod.evicted|Cluster": {Rule: "pod.evicted", Severity: protocol.SeverityWarning}}},
		// Cluster c: the newest draft is resolved: no alert, even if an older one says open.
		{ID: "d-c-2", ClusterID: "c", ClusterName: "c", Status: incidents.StatusResolved, Started: t0},
		{ID: "d-c-1", ClusterID: "c", ClusterName: "c", Status: incidents.StatusOpen, Started: t0.Add(-time.Hour),
			Active: map[string]incidents.Active{"k|o": crit("k", "o", "stale")}},
	}
	got := Alerts(drafts)
	if len(got) != 1 {
		t.Fatalf("%d alerts: %+v", len(got), got)
	}
	a := got[0]
	if a.ClusterID != "a" || a.DraftID != "d-a-2" || !a.Since.Equal(t0) || len(a.Findings) != 1 || a.Findings[0].Title != "DiskPressure" {
		t.Fatalf("alert %+v", a)
	}
	if a.Key != "d-a-2|node.disk.eviction|Node/k8server;" {
		t.Fatalf("key %q", a.Key)
	}
}

func TestAPI(t *testing.T) {
	sink := &incidents.MemorySink{}
	ctx := context.Background()
	sink.Save(ctx, &incidents.Draft{ID: "d1", ClusterID: "a", ClusterName: "k8server", Status: incidents.StatusOpen, Started: time.Now(),
		Active: map[string]incidents.Active{"r|o": {Rule: "r", Object: "o", Severity: protocol.SeverityCritical, Title: "CrashLoopBackOff"}}})
	mux := http.NewServeMux()
	guarded := 0
	(&API{Sink: sink}).Routes(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; h.ServeHTTP(w, r) })
	})(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/alerts", nil))
	var body struct{ Alerts []Alert }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || guarded != 1 || len(body.Alerts) != 1 || body.Alerts[0].Findings[0].Title != "CrashLoopBackOff" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// No drafts: an empty list, not null.
	mux = http.NewServeMux()
	(&API{Sink: &incidents.MemorySink{}}).Routes(func(h http.Handler) http.Handler { return h })(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/alerts", nil))
	if rec.Body.String() == "" || !json.Valid(rec.Body.Bytes()) || string(rec.Body.Bytes()[:12]) != `{"alerts":[]` {
		t.Fatalf("empty: %s", rec.Body)
	}
}
