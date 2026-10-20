package resources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

// fakeAgent answers like a cluster agent behind the tunnel.
type fakeAgent struct {
	mu      sync.Mutex
	reqs    []protocol.Request
	answer  func(req *protocol.Request) (*protocol.Response, error)
	stream  *fakeStream
	opened  *protocol.StreamOpen
	openErr error
}

func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, *req)
	f.mu.Unlock()
	return f.answer(req)
}

func (f *fakeAgent) OpenStream(_ context.Context, _ string, o *protocol.StreamOpen) (LogStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = o
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.stream, nil
}

// fakeStream plays a list of events; a nil chunk with a nil end blocks
// until the reader's context ends (a quiet log).
type fakeStream struct {
	events []streamLine
	i      int
	closed chan struct{}
}

func newStream(events ...streamLine) *fakeStream {
	return &fakeStream{events: events, closed: make(chan struct{})}
}

func (s *fakeStream) Next(ctx context.Context) ([]byte, *protocol.StreamClose, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		if e.End != nil {
			return nil, &protocol.StreamClose{Reason: e.End.Reason, Error: e.End.Error}, nil
		}
		return e.Data, nil, nil
	}
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func (s *fakeStream) Close() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}

type env struct {
	st      *store.SQLite
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
	e := &env{st: st, agent: &fakeAgent{}, mux: http.NewServeMux(), active: active, pending: pending}
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

func (e *env) do(t *testing.T, path string, login bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if login {
		req.Header.Set("X-Test-Login", "yes")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) json(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	rec := e.do(t, path, true)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: not JSON (%d): %q", path, rec.Code, rec.Body.String())
	}
	return rec.Code, body
}

func routes(id string) []string {
	return []string{
		"/api/clusters/" + id + "/namespaces",
		"/api/clusters/" + id + "/pods",
		"/api/clusters/" + id + "/services",
		"/api/clusters/" + id + "/namespaces/argocd/pods/controller-0/logs",
		"/api/clusters/" + id + "/namespaces/argocd/pods/controller-0/logs?follow=true",
		"/api/clusters/" + id + "/namespaces/argocd/pods/controller-0/debug",
		"/api/clusters/" + id + "/log-search?app=argocd/url-shortener",
	}
}

// Every route needs the login, knows unknown clusters and refuses a pending
// one, and never asks the agent in those cases (KM-80 criterion 3).
func TestLoginUnknownAndPending(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(*protocol.Request) (*protocol.Response, error) {
		t.Fatal("the agent was asked")
		return nil, nil
	}
	for _, p := range routes(e.active.ID) {
		if rec := e.do(t, p, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without login: %d", p, rec.Code)
		}
	}
	for _, p := range routes("c-0000000000000000") {
		if code, body := e.json(t, p); code != http.StatusNotFound || body["error"] != "not_found" {
			t.Errorf("%s unknown cluster: %d %v", p, code, body)
		}
	}
	if want := 2 * len(routes("")); e.guarded != want {
		t.Fatalf("login check ran on %d of %d requests", e.guarded, want)
	}
	if e.agent.opened != nil {
		t.Fatal("a stream was opened")
	}
}

func TestAgentOfflineAndErrors(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(*protocol.Request) (*protocol.Response, error) { return nil, tunnel.ErrNotConnected }
	e.agent.openErr = tunnel.ErrNotConnected
	for _, p := range routes(e.active.ID) {
		if code, body := e.json(t, p); code != http.StatusServiceUnavailable || body["error"] != "agent_offline" {
			t.Errorf("%s offline: %d %v", p, code, body)
		}
	}
	e.agent.answer = func(*protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: `pods "x" not found`}}, nil
	}
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/x/logs")
	if code != http.StatusNotFound || !strings.Contains(body["message"].(string), "not found") {
		t.Fatalf("missing pod: %d %v", code, body)
	}
	for _, bad := range []string{"?tailLines=0", "?tailLines=20000", "?previous=maybe", "?follow=x", "?container=a/b"} {
		if code, _ := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/x/logs"+bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}

func podJSON(ns, name string, ready bool, restarts int32, waiting string) json.RawMessage {
	state := `{"running":{}}`
	if waiting != "" {
		state = fmt.Sprintf(`{"waiting":{"reason":%q}}`, waiting)
	}
	return json.RawMessage(fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":%q,
		"creationTimestamp":"2026-09-28T01:00:00Z","ownerReferences":[{"kind":"ReplicaSet","name":"web-1","controller":true}]},
		"spec":{"nodeName":"k8server","containers":[{"name":"app"}]},
		"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":%v,"restartCount":%d,"state":%s,"lastState":{"terminated":{"reason":"Error","exitCode":1}}}]}}`,
		name, ns, ready, restarts, state))
}

func TestPodsPagesAndStatus(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		if req.Op != protocol.OpList || req.Version != "v1" || req.Resource != "pods" || req.Namespace != "shop" {
			return nil, fmt.Errorf("unexpected request %+v", req)
		}
		if req.Continue == "" {
			return &protocol.Response{Items: []json.RawMessage{podJSON("shop", "web-1", true, 0, "")}, Continue: "p2"}, nil
		}
		return &protocol.Response{Items: []json.RawMessage{podJSON("shop", "web-2", false, 7, "CrashLoopBackOff")}}, nil
	}
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/pods?namespace=shop")
	if code != http.StatusOK || body["truncated"] != false {
		t.Fatalf("%d %v", code, body)
	}
	pods := body["pods"].([]any)
	if len(pods) != 2 || len(e.agent.reqs) != 2 || e.agent.reqs[1].Continue != "p2" || e.agent.reqs[0].Limit != pageSize {
		t.Fatalf("pages: %v %+v", pods, e.agent.reqs)
	}
	p := pods[1].(map[string]any)
	if p["status"] != "CrashLoopBackOff" || p["restarts"].(float64) != 7 || p["ready"].(float64) != 0 || p["total"].(float64) != 1 ||
		p["node"] != "k8server" || p["owner"] != "ReplicaSet/web-1" {
		t.Fatalf("pod view: %v", p)
	}
	c := p["containers"].([]any)[0].(map[string]any)
	if c["name"] != "app" || c["previous"] != true || c["state"] != "waiting: CrashLoopBackOff" {
		t.Fatalf("container view: %v", c)
	}
}

func TestNamespaces(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Items: []json.RawMessage{
			json.RawMessage(`{"metadata":{"name":"kubemap-system","labels":{"pod-security.kubernetes.io/enforce":"restricted"}},"status":{"phase":"Active"}}`),
			json.RawMessage(`not json`),
		}}, nil
	}
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces")
	ns := body["namespaces"].([]any)
	if code != 200 || len(ns) != 1 || ns[0].(map[string]any)["podSecurity"] != "restricted" || e.agent.reqs[0].Resource != "namespaces" {
		t.Fatalf("%d %v %+v", code, body, e.agent.reqs)
	}
}

func TestListCapped(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		items := make([]json.RawMessage, pageSize)
		for i := range items {
			items[i] = podJSON("a", fmt.Sprintf("p-%d", i), true, 0, "")
		}
		return &protocol.Response{Items: items, Continue: "more"}, nil
	}
	_, body := e.json(t, "/api/clusters/"+e.active.ID+"/pods")
	if len(body["pods"].([]any)) != maxItems || body["truncated"] != true || len(e.agent.reqs) != maxItems/pageSize {
		t.Fatalf("pods %d, truncated %v, requests %d", len(body["pods"].([]any)), body["truncated"], len(e.agent.reqs))
	}
}

func TestPodStatusWords(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"metadata":{"name":"a"},"status":{"phase":"Failed","reason":"Evicted"}}`, "Evicted"},
		{`{"metadata":{"name":"a"},"spec":{"initContainers":[{"name":"i"}]},"status":{"phase":"Pending","initContainerStatuses":[{"name":"i","state":{"running":{}}}]}}`, "Init:0/1"},
		{`{"metadata":{"name":"a"},"spec":{"initContainers":[{"name":"i"}]},"status":{"phase":"Pending","initContainerStatuses":[{"name":"i","state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`, "Init:CrashLoopBackOff"},
		{`{"metadata":{"name":"a"},"status":{"phase":"Succeeded","containerStatuses":[{"name":"c","state":{"terminated":{"reason":"Completed"}}}]}}`, "Completed"},
		{`{"metadata":{"name":"a"},"status":{"phase":"Running","containerStatuses":[{"name":"c","state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`, "OOMKilled"},
		{`{"metadata":{"name":"a","deletionTimestamp":"2026-09-28T01:00:00Z"},"status":{"phase":"Running"}}`, "Terminating"},
		{`{"metadata":{"name":"a"},"status":{"phase":"Pending"}}`, "Pending"},
	} {
		p, err := PodView(json.RawMessage(tc.raw))
		if err != nil || p.Status != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.raw, p.Status, err, tc.want)
		}
	}
}

func TestLogsPlain(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Logs: []byte("<script>alert(1)</script>\nline 2\n"), Truncated: true}, nil
	}
	rec := e.do(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/controller-0/logs?container=app&previous=true&tailLines=200&timestamps=true", true)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" || rec.Header().Get("X-Kubemap-Truncated") != "true" ||
		!strings.HasPrefix(rec.Body.String(), "<script>") {
		t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	r := e.agent.reqs[0]
	if r.Op != protocol.OpLogs || r.Namespace != "argocd" || r.Name != "controller-0" ||
		*r.Logs != (protocol.LogOptions{Container: "app", Previous: true, TailLines: 200, Timestamps: true}) {
		t.Fatalf("request %+v %+v", r, r.Logs)
	}
	// Defaults: 500 lines, current container.
	e.do(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/controller-0/logs", true)
	if l := e.agent.reqs[1].Logs; l.TailLines != defaultTailLines || l.Previous || l.Container != "" {
		t.Fatalf("defaults %+v", l)
	}
}

func readLines(t *testing.T, body string) []streamLine {
	t.Helper()
	var out []streamLine
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		var l streamLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("bad NDJSON line %q", sc.Text())
		}
		out = append(out, l)
	}
	return out
}

func TestLogsFollow(t *testing.T) {
	e := newEnv(t)
	e.agent.stream = newStream(
		streamLine{Data: []byte("first\n")},
		streamLine{Data: []byte("\x1b[32mINF\x1b[0m second\n")},
		streamLine{End: &streamEnd{Reason: "log ended (container stopped or pod deleted)"}},
	)
	rec := e.do(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/controller-0/logs?follow=true&container=app", true)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	lines := readLines(t, rec.Body.String())
	if len(lines) != 3 || string(lines[0].Data) != "first\n" || !strings.Contains(string(lines[1].Data), "\x1b[32m") ||
		lines[2].End == nil || !strings.HasPrefix(lines[2].End.Reason, "log ended") {
		t.Fatalf("lines %+v", lines)
	}
	if o := e.agent.opened; o.Kind != protocol.StreamLogs || o.Name != "controller-0" || o.Logs.Container != "app" {
		t.Fatalf("opened %+v", o)
	}
	select {
	case <-e.agent.stream.closed:
	default:
		t.Fatal("stream not closed at the end")
	}
}

// A stream that fails at once gets a real HTTP status, not a 200.
func TestLogsFollowFailsFirst(t *testing.T) {
	e := newEnv(t)
	e.agent.stream = newStream(streamLine{End: &streamEnd{Error: &protocol.Error{Code: protocol.CodeBadRequest, Message: "previous terminated container \"app\" not found"}}})
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/controller-0/logs?follow=true&previous=true")
	if code != http.StatusBadRequest || !strings.Contains(body["message"].(string), "previous terminated") {
		t.Fatalf("%d %v", code, body)
	}
}

// A quiet log answers 200 after firstEventWait; when the browser goes away
// the stream is closed, which tells the agent.
func TestLogsFollowQuietThenBrowserLeaves(t *testing.T) {
	e := newEnv(t)
	e.agent.stream = newStream() // no events: blocks until the request ends
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/clusters/"+e.active.ID+"/namespaces/argocd/pods/controller-0/logs?follow=true", nil).WithContext(ctx)
	req.Header.Set("X-Test-Login", "yes")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.mux.ServeHTTP(rec, req); close(done) }()
	time.AfterFunc(firstEventWait+500*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(firstEventWait + 5*time.Second):
		t.Fatal("handler did not return after the browser left")
	}
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	select {
	case <-e.agent.stream.closed:
	default:
		t.Fatal("stream not closed when the browser left")
	}
}

func TestDebug(t *testing.T) {
	e := newEnv(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "protocol", "testdata", "response-pod.debug.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Payload struct {
			Object json.RawMessage `json:"object"`
		} `json:"payload"`
	}
	json.Unmarshal(raw, &golden)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Object: golden.Payload.Object}, nil
	}
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces/kubemap-demo/pods/web-59f856967c-lj7fl/debug")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if r := e.agent.reqs[0]; r.Op != protocol.OpPodDebug || r.Namespace != "kubemap-demo" || r.Name != "web-59f856967c-lj7fl" {
		t.Fatalf("request %+v", r)
	}
	d := body["debug"].(map[string]any)
	steps := d["steps"].([]any)
	if d["causeStep"] != "netpol" || len(steps) != 8 || steps[7].(map[string]any)["verdict"] != "problem" ||
		len(steps[0].(map[string]any)["commands"].([]any)) == 0 {
		t.Fatalf("debug %v", d)
	}
	// Agent errors keep their meaning.
	e.agent.answer = func(*protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: `pod "x" not found`}}, nil
	}
	if code, _ := e.json(t, "/api/clusters/"+e.active.ID+"/namespaces/a/pods/x/debug"); code != 404 {
		t.Fatalf("missing pod: %d", code)
	}
}

func TestServices(t *testing.T) {
	e := newEnv(t)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		if req.Resource != "services" || req.Namespace != "kubemap-demo" || req.Version != "v1" {
			return nil, fmt.Errorf("unexpected %+v", req)
		}
		return &protocol.Response{Items: []json.RawMessage{json.RawMessage(`{"metadata":{"name":"api","namespace":"kubemap-demo"},
			"spec":{"type":"ClusterIP","clusterIP":"10.43.0.9","selector":{"app":"api"},
			"ports":[{"name":"http","port":80,"targetPort":8080},{"port":53,"protocol":"UDP","targetPort":"dns"}]}}`)}}, nil
	}
	code, body := e.json(t, "/api/clusters/"+e.active.ID+"/services?namespace=kubemap-demo")
	svcs := body["services"].([]any)
	if code != 200 || len(svcs) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	ports := svcs[0].(map[string]any)["ports"].([]any)
	p0, p1 := ports[0].(map[string]any), ports[1].(map[string]any)
	if p0["protocol"] != "TCP" || p0["targetPort"] != "8080" || p1["protocol"] != "UDP" || p1["targetPort"] != "dns" {
		t.Fatalf("ports %v", ports)
	}
}

func TestLogSearch(t *testing.T) {
	e := newEnv(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "protocol", "testdata", "response-logs.search.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Payload struct {
			Object json.RawMessage `json:"object"`
		} `json:"payload"`
	}
	json.Unmarshal(raw, &golden)
	e.agent.answer = func(req *protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Object: golden.Payload.Object}, nil
	}
	base := "/api/clusters/" + e.active.ID + "/log-search?app=argocd/url-shortener"
	code, body := e.json(t, base+"&component=Deployment/sre-lab-app/url-shortener&q=pool&levels=error,warn&since=2h&until=2026-09-28T08:00:00Z&previous=1&max=300")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	q := e.agent.reqs[0].LogSearch
	until := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	if e.agent.reqs[0].Op != protocol.OpLogSearch || q.App != "argocd/url-shortener" || q.Component != "Deployment/sre-lab-app/url-shortener" ||
		q.Text != "pool" || strings.Join(q.Levels, ",") != "error,warn" || !q.Until.Equal(until) || !q.Since.Equal(until.Add(-2*time.Hour)) ||
		!q.Previous || q.MaxLines != 300 {
		t.Fatalf("request %+v", q)
	}
	res := body["search"].(map[string]any)
	entries := res["entries"].([]any)
	if len(entries) != 2 || len(entries[0].(map[string]any)["stack"].([]any)) != 3 || res["levels"].(map[string]any)["info"] != 214.0 {
		t.Fatalf("search %v", res)
	}
	// since as a time; defaults.
	if code, _ := e.json(t, base+"&since=2026-09-28T07:00:00Z"); code != 200 || !e.agent.reqs[1].LogSearch.Since.Equal(until.Add(-time.Hour)) {
		t.Fatalf("since as a time: %d %+v", code, e.agent.reqs[1].LogSearch)
	}
	// Bad queries never reach the agent.
	n := len(e.agent.reqs)
	for _, bad := range []string{"", "&since=yesterday", "&since=-1h", "&until=soon", "&max=many", "&levels=loud", "&max=99999"} {
		path := base + bad
		if bad == "" {
			path = "/api/clusters/" + e.active.ID + "/log-search"
		}
		if code, body := e.json(t, path); code != 400 {
			t.Errorf("%q: %d %v", bad, code, body)
		}
	}
	if len(e.agent.reqs) != n {
		t.Errorf("bad queries reached the agent")
	}
	// Agent errors keep their meaning.
	e.agent.answer = func(*protocol.Request) (*protocol.Response, error) {
		return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "no app argocd/x"}}, nil
	}
	if code, _ := e.json(t, "/api/clusters/"+e.active.ID+"/log-search?app=argocd/x"); code != 404 {
		t.Fatalf("unknown app: %d", code)
	}
}
