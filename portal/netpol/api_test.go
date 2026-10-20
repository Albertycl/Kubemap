package netpol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

type fakeAgent struct {
	mu     sync.Mutex
	checks []protocol.NetpolCheckRequest
	down   bool
}

func pod(name, owner, phase string) json.RawMessage {
	ref := ""
	if owner != "" {
		ref = fmt.Sprintf(`,"ownerReferences":[{"kind":"ReplicaSet","name":%q,"controller":true}]`, owner)
	}
	return json.RawMessage(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"shop"%s},"spec":{"containers":[{"name":"c"}]},"status":{"phase":%q}}`, name, ref, phase))
}

func svc(name string, selector bool, ports string) json.RawMessage {
	sel := ""
	if selector {
		sel = `"selector":{"app":"` + name + `"},`
	}
	return json.RawMessage(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"shop"},"spec":{"type":"ClusterIP",%s"ports":%s}}`, name, sel, ports))
}

// The demo's rule: only monitoring may reach api; everything else is open.
func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	if f.down {
		return nil, tunnel.ErrNotConnected
	}
	switch {
	case req.Op == protocol.OpList && req.Resource == "pods":
		return &protocol.Response{Items: []json.RawMessage{
			pod("web-1", "web-7d9", "Running"), pod("web-2", "web-7d9", "Running"),
			pod("monitoring-1", "mon-55f", "Running"), pod("job-1", "", "Succeeded"), pod("debug", "", "Running"),
		}}, nil
	case req.Op == protocol.OpList && req.Resource == "services":
		return &protocol.Response{Items: []json.RawMessage{
			svc("api", true, `[{"name":"http","port":80,"targetPort":8080},{"name":"metrics","port":9090}]`),
			svc("web", true, `[{"port":80}]`),
			svc("external", false, `[{"port":5432}]`),
		}}, nil
	case req.Op == protocol.OpNetpolCheck:
		q := *req.NetpolCheck
		f.mu.Lock()
		f.checks = append(f.checks, q)
		f.mu.Unlock()
		if q.To.Service == "missing" {
			return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "no Service shop/missing"}}, nil
		}
		allowed := q.To.Service != "api" || strings.HasPrefix(q.From.Pod, "monitoring")
		res := protocol.NetpolResult{Allowed: allowed, Port: q.Port, Protocol: "TCP", From: "Pod/shop/" + q.From.Pod,
			Summary: map[bool]string{true: "allowed", false: "blocked by shop/api-allow-monitoring-only"}[allowed],
			Paths:   []protocol.NetpolPath{{To: "Pod/shop/" + q.To.Service + "-1", Port: q.Port, Allowed: allowed}}}
		b, _ := json.Marshal(res)
		return &protocol.Response{Object: b}, nil
	}
	return nil, fmt.Errorf("unexpected request %+v", req)
}

type env struct {
	agent   *fakeAgent
	mux     *http.ServeMux
	guarded int
	active  store.Cluster
	pending store.Cluster
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	active, _ := st.EnsureCluster(ctx, "c-k3s-home", "k3s-home")
	pending, _ := st.EnsureCluster(ctx, "c-new", "new")
	e := &env{agent: &fakeAgent{}, mux: http.NewServeMux(), active: active, pending: pending}
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

func TestCheck(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/netpol/check?fromNamespace=shop&fromPod=web-1&toNamespace=shop&toService=api&port=80&protocol=tcp", true)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	res := body["result"].(map[string]any)
	if res["allowed"] != false || !strings.Contains(res["summary"].(string), "api-allow-monitoring-only") {
		t.Fatalf("result %v", res)
	}
	q := e.agent.checks[0]
	if q.From != (protocol.NetpolEndpoint{Namespace: "shop", Pod: "web-1"}) || q.To != (protocol.NetpolEndpoint{Namespace: "shop", Service: "api"}) ||
		q.Port != 80 || q.Protocol != "TCP" {
		t.Fatalf("request %+v", q)
	}
	// An IP source drops a namespace the form may still send.
	e.get(t, "/api/clusters/"+e.active.ID+"/netpol/check?fromNamespace=shop&fromIp=10.0.0.9&toNamespace=shop&toPod=web-1&port=8080", true)
	if q := e.agent.checks[1]; q.From != (protocol.NetpolEndpoint{IP: "10.0.0.9"}) || q.To.Pod != "web-1" {
		t.Fatalf("ip request %+v", q)
	}
	for _, bad := range []string{
		"port=0&fromNamespace=shop&fromPod=a&toNamespace=shop&toPod=b",
		"port=x&fromNamespace=shop&fromPod=a&toNamespace=shop&toPod=b",
		"port=80&fromNamespace=shop&toNamespace=shop&toPod=b",                         // no source
		"port=80&fromNamespace=shop&fromPod=a&toNamespace=shop&toPod=b&toService=c",   // two destinations
		"port=80&fromNamespace=shop&fromPod=a&toNamespace=shop&toPod=b&protocol=HTTP", // bad protocol
	} {
		if code, _ := e.get(t, "/api/clusters/"+e.active.ID+"/netpol/check?"+bad, true); code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
	if code, body := e.get(t, "/api/clusters/"+e.active.ID+"/netpol/check?fromNamespace=shop&fromPod=a&toNamespace=shop&toService=missing&port=80", true); code != 404 {
		t.Errorf("unknown Service: %d %v", code, body)
	}
}

func TestMatrix(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/netpol/matrix?namespace=shop", true)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	var m Matrix
	b, _ := json.Marshal(body)
	json.Unmarshal(b, &m)
	// Rows: one pod per workload, running only (job-1 has finished).
	if got := fmt.Sprint(m.Sources); got != "[{Pod/debug debug} {ReplicaSet/mon-55f monitoring-1} {ReplicaSet/web-7d9 web-1}]" {
		t.Fatalf("sources %s", got)
	}
	// Columns: every port of every Service with a selector.
	if len(m.Targets) != 3 || m.Targets[0] != (Target{Service: "api", Port: 80, Protocol: "TCP", PortName: "http"}) || m.Targets[2].Service != "web" {
		t.Fatalf("targets %+v", m.Targets)
	}
	if len(m.Cells) != 3 || len(m.Cells[0]) != 3 {
		t.Fatalf("cells %+v", m.Cells)
	}
	// Only monitoring reaches api; web reaches web.
	if m.Cells[0][0].Allowed || !m.Cells[1][0].Allowed || m.Cells[2][0].Allowed || !m.Cells[2][2].Allowed {
		t.Fatalf("verdicts %+v", m.Cells)
	}
	if len(e.agent.checks) != 9 || m.Truncated {
		t.Fatalf("%d checks, truncated %v", len(e.agent.checks), m.Truncated)
	}
	if code, _ := e.get(t, "/api/clusters/"+e.active.ID+"/netpol/matrix", true); code != 400 {
		t.Errorf("no namespace: %d", code)
	}
}

func TestLoginUnknownPendingOffline(t *testing.T) {
	e := newEnv(t)
	paths := func(id string) []string {
		return []string{
			"/api/clusters/" + id + "/netpol/check?fromNamespace=shop&fromPod=a&toNamespace=shop&toPod=b&port=80",
			"/api/clusters/" + id + "/netpol/matrix?namespace=shop",
		}
	}
	for _, p := range paths(e.active.ID) {
		if code, _ := e.get(t, p, false); code != 401 {
			t.Errorf("%s without login: %d", p, code)
		}
	}
	for _, p := range paths("c-0000000000000000") {
		if code, _ := e.get(t, p, true); code != 404 {
			t.Errorf("%s unknown: %d", p, code)
		}
	}
	if len(e.agent.checks) != 0 {
		t.Fatal("the agent was asked")
	}
	e.agent.down = true
	for _, p := range paths(e.active.ID) {
		if code, body := e.get(t, p, true); code != 503 || body["error"] != "agent_offline" {
			t.Errorf("%s offline: %d %v", p, code, body)
		}
	}
	if e.guarded != 6 {
		t.Fatalf("login check ran on %d of 6 requests", e.guarded)
	}
}

func TestMatrixCaps(t *testing.T) {
	var items []json.RawMessage
	for i := 0; i < maxSources+5; i++ {
		items = append(items, pod(fmt.Sprintf("p-%02d", i), fmt.Sprintf("rs-%02d", i), "Running"))
	}
	s, cut := sources(items, false)
	if len(s) != maxSources || !cut {
		t.Fatalf("sources %d cut %v", len(s), cut)
	}
	var svcs []json.RawMessage
	for i := 0; i < maxTargets; i++ {
		svcs = append(svcs, svc(fmt.Sprintf("s-%02d", i), true, `[{"port":80},{"port":81}]`))
	}
	tg, cut := targets(svcs, false)
	if len(tg) != maxTargets || !cut {
		t.Fatalf("targets %d cut %v", len(tg), cut)
	}
}

func TestCellOf(t *testing.T) {
	none := cellOf(&protocol.NetpolResult{Allowed: false, Summary: "no destination to check"})
	if !none.NoDestination || none.Allowed {
		t.Fatalf("no paths: %+v", none)
	}
	blocked := cellOf(&protocol.NetpolResult{Allowed: false, Paths: []protocol.NetpolPath{{To: "Pod/a/b"}}})
	if blocked.NoDestination || blocked.Allowed {
		t.Fatalf("blocked: %+v", blocked)
	}
	if ok := cellOf(&protocol.NetpolResult{Allowed: true, Paths: []protocol.NetpolPath{{To: "Pod/a/b", Allowed: true}}}); !ok.Allowed {
		t.Fatalf("allowed: %+v", ok)
	}
}
