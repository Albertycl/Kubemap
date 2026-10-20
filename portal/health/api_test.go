package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

type fakeAgent struct {
	mu     sync.Mutex
	asked  map[string]*protocol.Request
	answer func(clusterID string) (*protocol.Response, error)
}

func (f *fakeAgent) Request(_ context.Context, id string, req *protocol.Request) (*protocol.Response, error) {
	f.mu.Lock()
	f.asked[id] = req
	f.mu.Unlock()
	return f.answer(id)
}

type env struct {
	st      *store.SQLite
	agent   *fakeAgent
	mux     *http.ServeMux
	guarded int
	active  store.Cluster
	other   store.Cluster
	pending store.Cluster
}

func accepted(t *testing.T, st *store.SQLite, name string) store.Cluster {
	t.Helper()
	ctx := context.Background()
	c, _ := st.EnsureCluster(ctx, "c-"+name, name)
	c, _ = st.GetCluster(ctx, c.ID)
	return c
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, agent: &fakeAgent{asked: map[string]*protocol.Request{}}, mux: http.NewServeMux()}
	e.active = accepted(t, st, "k3s-home")
	e.other = accepted(t, st, "lab-2")
	e.pending, _ = st.EnsureCluster(context.Background(), "c-new", "new")
	require := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.guarded++
			if r.Header.Get("X-Test-Login") != "yes" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	NewAPI(st, e.agent).Routes(require)(e.mux)
	return e
}

func (e *env) get(t *testing.T, path string, login bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if login {
		req.Header.Set("X-Test-Login", "yes")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// golden is the protocol's own example report.
func golden(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "protocol", "testdata", "response-health.check.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload struct {
			Object json.RawMessage `json:"object"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Payload.Object
}

func TestOneCluster(t *testing.T) {
	e := newEnv(t)
	obj := golden(t)
	e.agent.answer = func(string) (*protocol.Response, error) { return &protocol.Response{Object: obj}, nil }

	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/health?namespace=kubemap-demo", true)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if r := e.agent.asked[e.active.ID]; r.Op != protocol.OpHealthCheck || r.Namespace != "kubemap-demo" {
		t.Fatalf("request %+v", r)
	}
	rep := body["report"].(map[string]any)
	f := rep["findings"].([]any)
	first := f[0].(map[string]any)
	if len(f) != 2 || first["severity"] != "critical" || first["verify"] == "" || first["fix"] == "" || body["at"] == nil {
		t.Fatalf("report %v", rep)
	}
	if len(rep["rules"].([]any)) != 3 || rep["thresholds"].(map[string]any)["source"] != "agent settings" {
		t.Fatalf("rules/thresholds %v", rep)
	}
}

func TestLoginUnknownPendingOffline(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(string) (*protocol.Response, error) { return nil, tunnel.ErrNotConnected }
	for _, p := range []string{"/api/health", "/api/clusters/" + e.active.ID + "/health"} {
		if code, _ := e.get(t, p, false); code != http.StatusUnauthorized {
			t.Errorf("%s without login: %d", p, code)
		}
	}
	if code, body := e.get(t, "/api/clusters/c-0000000000000000/health", true); code != 404 || body["error"] != "not_found" {
		t.Errorf("unknown: %d %v", code, body)
	}
	if code, body := e.get(t, "/api/clusters/"+e.active.ID+"/health", true); code != 503 || body["error"] != "agent_offline" {
		t.Errorf("offline: %d %v", code, body)
	}
	if e.guarded != 4 {
		t.Fatalf("login check ran on %d of 4 requests", e.guarded)
	}
}

// All clusters: a report for each one that answers, the reason for each
// one that does not.
func TestAllClusters(t *testing.T) {
	e := newEnv(t)
	obj := golden(t)
	e.agent.answer = func(id string) (*protocol.Response, error) {
		switch id {
		case e.active.ID:
			return &protocol.Response{Object: obj}, nil
		case e.other.ID:
			return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeUnsupported, Message: "request: unknown op"}}, nil
		}
		return nil, tunnel.ErrNotConnected
	}
	code, body := e.get(t, "/api/health", true)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	byName := map[string]map[string]any{}
	for _, c := range body["clusters"].([]any) {
		m := c.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if len(byName) != 3 {
		t.Fatalf("clusters %v", body["clusters"])
	}
	if r := byName["k3s-home"]; r["report"] == nil || r["error"] != nil {
		t.Errorf("k3s-home: %v", r)
	}
	if r := byName["lab-2"]; r["report"] != nil || r["error"].(map[string]any)["error"] != "unsupported" {
		t.Errorf("lab-2 (old agent): %v", r)
	}
}

func TestNormalizeWorstFirst(t *testing.T) {
	r := &protocol.HealthReport{Findings: []protocol.HealthFinding{
		{Rule: "a", Severity: "info"}, {Rule: "b", Severity: "critical"}, {Rule: "c", Severity: "warning"}, {Rule: "d", Severity: "critical"},
	}}
	normalize(r)
	got := ""
	for _, f := range r.Findings {
		got += f.Rule
	}
	if got != "bdca" || r.Rules == nil {
		t.Fatalf("order %s, rules %v", got, r.Rules)
	}
	empty := &protocol.HealthReport{}
	normalize(empty)
	if empty.Findings == nil {
		t.Fatal("nil findings")
	}
}

// The node agents' reports go along with health.check (the node rules judge
// PSI from them; the cluster agent makes no kubelet calls).
func TestReportsSent(t *testing.T) {
	e := newEnv(t)
	obj := golden(t)
	e.agent.answer = func(string) (*protocol.Response, error) { return &protocol.Response{Object: obj}, nil }
	h := NewAPI(e.st, e.agent)
	var askedFor string
	h.Reports = func(id string) []protocol.Metrics {
		askedFor = id
		return []protocol.Metrics{{NodeName: "k8server"}}
	}
	if rep, status, _, msg := h.ask(context.Background(), e.active.ID, ""); rep == nil {
		t.Fatalf("%d %s", status, msg)
	}
	if got := e.agent.asked[e.active.ID].NodeMetrics; askedFor != e.active.ID || len(got) != 1 || got[0].NodeName != "k8server" {
		t.Fatalf("node reports sent: %+v (for %q)", got, askedFor)
	}
}
