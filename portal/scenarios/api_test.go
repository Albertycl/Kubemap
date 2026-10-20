package scenarios

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

// golden reads the object of a protocol golden response.
func golden(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "protocol", "testdata", "response-"+name+".json"))
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

type fakeAgent struct {
	got  []*protocol.Request
	obj  map[protocol.Op]json.RawMessage
	down bool
}

func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	f.got = append(f.got, req)
	if f.down {
		return nil, tunnel.ErrNotConnected
	}
	if o, ok := f.obj[req.Op]; ok {
		return &protocol.Response{Object: o}, nil
	}
	return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeUnsupported, Message: "request: unknown op"}}, nil
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
	e := &env{agent: &fakeAgent{obj: map[protocol.Op]json.RawMessage{
		protocol.OpServiceCheck: golden(t, "service.check"),
		protocol.OpPodDebug:     golden(t, "pod.debug"),
		protocol.OpNodePressure: golden(t, "node.pressure"),
		protocol.OpAppMap:       golden(t, "appmap"),
		protocol.OpNetpolCheck:  golden(t, "netpol.check"),
		protocol.OpHealthCheck:  golden(t, "health.check"),
	}}, mux: http.NewServeMux(), active: active, pending: pending}
	api, err := NewAPI(st, e.agent)
	if err != nil {
		t.Fatal(err)
	}
	api.Reports = func(string) []protocol.Metrics { return []protocol.Metrics{{NodeName: "k8server"}} }
	api.Routes(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.guarded++
			if r.Header.Get("X-Test-Login") != "yes" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	})(e.mux)
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

// The scenario itself needs no cluster (KM-82 criterion 3).
func TestListAndShow(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/api/scenarios", true)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	list := body["scenarios"].([]any)
	var s map[string]any
	for _, x := range list {
		if x.(map[string]any)["id"] == "traffic-503" {
			s = x.(map[string]any)
		}
	}
	if s == nil || s["checks"].(float64) < 4 || s["area"] != "traffic" || body["placeholders"].(map[string]any)["svc"] != "Service" {
		t.Fatalf("list %v", body)
	}
	code, body = e.get(t, "/api/scenarios/traffic-503", true)
	sc := body["scenario"].(map[string]any)
	if code != 200 || len(sc["steps"].([]any)) < 5 || len(sc["senior"].([]any)) == 0 {
		t.Fatalf("show %d %v", code, body)
	}
	if code, _ := e.get(t, "/api/scenarios/nope", true); code != 404 {
		t.Fatalf("unknown scenario: %d", code)
	}
	if len(e.agent.got) != 0 {
		t.Fatal("reading a scenario asked an agent")
	}
}

func step(t *testing.T, e *env, n int, query string) (int, map[string]any) {
	t.Helper()
	return e.get(t, "/api/clusters/"+e.active.ID+"/scenarios/traffic-503/steps/"+string(rune('0'+n))+"/check?"+query, true)
}

func TestStepChecks(t *testing.T) {
	e := newEnv(t)
	// Step 2 (service.check) without a Service: it says what it needs, and
	// asks no agent.
	code, body := step(t, e, 2, "ns=kubemap-demo")
	if code != 200 || body["verdict"] != NeedsInput || body["missing"].([]any)[0] != "svc" || !strings.Contains(body["summary"].(string), "Service") {
		t.Fatalf("needs input: %d %v", code, body)
	}
	if len(e.agent.got) != 0 {
		t.Fatal("asked the agent with a missing value")
	}
	// With it: the golden answer is the demo's selector typo → bad.
	code, body = step(t, e, 2, "ns=kubemap-demo&svc=catalog")
	if code != 200 || body["verdict"] != Bad || body["op"] != "service.check" || !strings.Contains(strings.Join(toStrings(body["details"]), " "), "selector matches no pods") {
		t.Fatalf("service.check: %d %v", code, body)
	}
	if r := e.agent.got[0]; r.Op != protocol.OpServiceCheck || r.Namespace != "kubemap-demo" || r.Name != "catalog" {
		t.Fatalf("request %+v", r)
	}
	// Step 4 (pod.debug): the golden walk names the NetworkPolicy.
	_, body = step(t, e, 4, "ns=kubemap-demo&pod=web-59f856967c-lj7fl")
	if body["verdict"] != Bad || !strings.Contains(body["summary"].(string), "api-allow-monitoring-only") {
		t.Fatalf("pod.debug: %v", body)
	}
	// Step 6 (node.pressure) needs nothing.
	_, body = step(t, e, 6, "")
	if body["verdict"] == nil || body["op"] != "node.pressure" {
		t.Fatalf("node.pressure: %v", body)
	}
	// Step 3 has no live check; a step past the end does not exist.
	if code, _ := step(t, e, 3, "ns=a"); code != 404 {
		t.Fatalf("step without check: %d", code)
	}
	if code, _ := step(t, e, 9, ""); code != 404 {
		t.Fatalf("step 9: %d", code)
	}
	// Bad values from the browser are refused before the agent is asked,
	// also for placeholders this step does not use (B1).
	n := len(e.agent.got)
	for _, q := range []string{"ns=a/b&svc=x", "ns=a&svc=x&pod=p%3Bcurl%20x%7Csh", "ns=a&svc=x&port=0", "ns=a&svc=x&app=a%24(id)/b"} {
		if code, body := step(t, e, 2, q); code != 400 || len(e.agent.got) != n {
			t.Fatalf("%s: %d %v", q, code, body)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestCheckLoginUnknownPendingOffline(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/scenarios", "/api/scenarios/traffic-503", "/api/clusters/" + e.active.ID + "/scenarios/traffic-503/steps/6/check"} {
		if code, _ := e.get(t, p, false); code != 401 {
			t.Errorf("%s without login: %d", p, code)
		}
	}
	if code, _ := e.get(t, "/api/clusters/c-0000000000000000/scenarios/traffic-503/steps/6/check", true); code != 404 {
		t.Errorf("unknown cluster: %d", code)
	}
	e.agent.down = true
	if code, body := step(t, e, 6, ""); code != 503 || body["error"] != "agent_offline" {
		t.Errorf("offline: %d %v", code, body)
	}
}

// Each interpreter on the protocol's golden answers.
func TestVerdictsOnGoldenAnswers(t *testing.T) {
	cases := []struct {
		op      protocol.Op
		file    string
		verdict string
		want    string
	}{
		{protocol.OpServiceCheck, "service.check", Bad, "catalog"},
		{protocol.OpPodDebug, "pod.debug", Bad, "api-allow-monitoring-only"},
		{protocol.OpPodDiagnose, "pod.diagnose", Bad, "Image not on the node"},
		{protocol.OpNetpolCheck, "netpol.check", Bad, "deny-egress-to-argo"},
		{protocol.OpNodePressure, "node.pressure", "", ""}, // verdict depends on the example; checked below
		{protocol.OpHealthCheck, "health.check", Bad, "DiskPressure"},
		{protocol.OpAppMap, "appmap", "", ""},
	}
	for _, c := range cases {
		res, err := interpret(&protocol.Request{Op: c.op}, golden(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		if c.verdict != "" && res.Verdict != c.verdict {
			t.Errorf("%s: verdict %s, want %s (%s)", c.op, res.Verdict, c.verdict, res.Summary)
		}
		if !strings.Contains(res.Summary+strings.Join(res.Details, " "), c.want) {
			t.Errorf("%s: %q does not mention %q", c.op, res.Summary+strings.Join(res.Details, " "), c.want)
		}
		if res.Verdict == "" || res.Summary == "" {
			t.Errorf("%s: empty result %+v", c.op, res)
		}
	}
	if _, err := interpret(&protocol.Request{Op: protocol.OpServiceCheck}, json.RawMessage(`[`)); err == nil {
		t.Error("an unreadable answer was accepted")
	}
}

func TestVerdictEdges(t *testing.T) {
	ok := serviceVerdict(protocol.ServiceCheckResult{Services: []protocol.ServiceStatus{{Name: "api", Healthy: true, ReadyEndpoints: 2, PodsReady: 2, PodsMatched: 2}}})
	if ok.Verdict != Good || !strings.Contains(ok.Summary, "2 ready endpoint") {
		t.Fatalf("healthy service: %+v", ok)
	}
	if r := netpolVerdict(protocol.NetpolResult{Allowed: false, Summary: "no destination to check"}); r.Verdict != Warn {
		t.Fatalf("no paths must not read as blocked: %+v", r)
	}
	if r := netpolVerdict(protocol.NetpolResult{Allowed: true, Summary: "allowed", Paths: []protocol.NetpolPath{{Allowed: true}}}); r.Verdict != Good {
		t.Fatalf("allowed: %+v", r)
	}
	p := pressureVerdict(protocol.NodePressureReport{Nodes: []protocol.NodePressure{{Node: "a", Ready: true, Verdict: "ok"}, {Node: "b", Ready: false}}})
	if p.Verdict != Bad || !strings.Contains(strings.Join(p.Details, ""), "b: NotReady") {
		t.Fatalf("NotReady node: %+v", p)
	}
	if p := pressureVerdict(protocol.NodePressureReport{Nodes: []protocol.NodePressure{{Node: "a", Ready: true, Verdict: "elevated", Summary: "elevated: [cpu]"}}}); p.Verdict != Warn {
		t.Fatalf("elevated: %+v", p)
	}
	if r := appMapVerdict(protocol.AppMap{}); r.Verdict != Unknown {
		t.Fatalf("no routes: %+v", r)
	}
	broken := protocol.AppMap{Apps: []protocol.MapApp{{Components: []protocol.MapComponent{{Routes: []protocol.MapRoute{
		{Host: "shop.example.com", Path: "/api", Service: "Service/shop/api", Problems: []string{"no ready endpoints (requests get 503)"}},
		{Host: "shop.example.com", Service: "Service/shop/web"},
	}}}}}}
	if r := appMapVerdict(broken); r.Verdict != Bad || !strings.Contains(r.Summary, "shop.example.com/api") {
		t.Fatalf("broken route: %+v", r)
	}
	// A plain-HTTP finding is listed but is not a 503 cause.
	plain := protocol.AppMap{Apps: []protocol.MapApp{{Components: []protocol.MapComponent{{Routes: []protocol.MapRoute{
		{Host: "sre-lab.example.com", Path: "/whoami", Service: "Service/sre-lab-app/whoami", Problems: []string{"also served over plain HTTP without a redirect to HTTPS"}},
	}}}}}}
	if r := appMapVerdict(plain); r.Verdict != Good || !strings.Contains(strings.Join(r.Details, ""), "Not a 503 cause") {
		t.Fatalf("plain HTTP: %+v", r)
	}
	if r := podDebugVerdict(protocol.PodDebug{Name: "p", Status: "Running", Steps: []protocol.DebugStep{{Verdict: "ok"}}}); r.Verdict != Good {
		t.Fatalf("healthy pod: %+v", r)
	}
	long := make([]string, 20)
	if got := capped(long); len(got) != maxDetails+1 || !strings.HasPrefix(got[maxDetails], "and 12 more") {
		t.Fatalf("capped: %d %q", len(got), got[len(got)-1])
	}
}

// Review KM-82 N-1: node.pressure (and health.check) get the node agents'
// reports; other ops never carry them.
func TestNodeReportsSent(t *testing.T) {
	e := newEnv(t)
	step(t, e, 6, "")
	last := e.agent.got[len(e.agent.got)-1]
	if last.Op != protocol.OpNodePressure || len(last.NodeMetrics) != 1 || last.NodeMetrics[0].NodeName != "k8server" {
		t.Fatalf("node.pressure request: %+v", last)
	}
	step(t, e, 2, "ns=a&svc=b")
	if last := e.agent.got[len(e.agent.got)-1]; last.Op != protocol.OpServiceCheck || last.NodeMetrics != nil {
		t.Fatalf("service.check carried node reports: %+v", last)
	}
}
