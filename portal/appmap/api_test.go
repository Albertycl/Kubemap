package appmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeAgent answers requests like a cluster agent behind the tunnel.
type fakeAgent struct {
	got  *protocol.Request
	resp *protocol.Response
	err  error
}

func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	f.got = req
	return f.resp, f.err
}

type env struct {
	st      *store.SQLite
	agent   *fakeAgent
	mux     *http.ServeMux
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
	allow := func(h http.Handler) http.Handler { return h }
	NewAPI(st, e.agent).Routes(allow)(e.mux)
	return e
}

func (e *env) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: not JSON: %q", path, rec.Body.String())
	}
	return rec.Code, body
}

func object(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sampleMap() protocol.AppMap {
	return protocol.AppMap{
		Apps: []protocol.MapApp{{
			ID: "argocd/url-shortener", Name: "url-shortener", Namespace: "sre-lab-app", Source: "argocd", Health: "Healthy, Synced",
			Components: []protocol.MapComponent{
				{ID: "Deployment/sre-lab-app/url-shortener", Kind: "Deployment", Namespace: "sre-lab-app", Name: "url-shortener",
					Tier: protocol.TierBackend, TierReason: "calls a data store", Status: "2/2 ready", Healthy: true,
					Pods: []protocol.MapPod{{Name: "url-shortener-abc", Status: "Running", Ready: true}}},
			},
		}},
		Links: []protocol.MapLink{{From: "Deployment/sre-lab-app/url-shortener", To: "StatefulSet/sre-lab-app/redis-master",
			Via: []string{"Service/sre-lab-app/redis-master"}}},
	}
}

func TestAppMapPassesTheMapThrough(t *testing.T) {
	e := newEnv(t)
	e.agent.resp = &protocol.Response{Object: object(t, sampleMap())}
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/appmap?namespace=sre-lab-app&app=argocd/url-shortener")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if e.agent.got.Op != protocol.OpAppMap || e.agent.got.Namespace != "sre-lab-app" ||
		e.agent.got.AppMap == nil || e.agent.got.AppMap.App != "argocd/url-shortener" {
		t.Fatalf("request sent: %+v", e.agent.got)
	}
	apps := body["apps"].([]any)
	if len(apps) != 1 || apps[0].(map[string]any)["id"] != "argocd/url-shortener" {
		t.Fatalf("apps: %v", body["apps"])
	}
	if body["at"] == nil {
		t.Fatal("no time stamp")
	}
	// A link without evidence becomes an empty list, never null.
	link := body["links"].([]any)[0].(map[string]any)
	if ev, ok := link["evidence"].([]any); !ok || len(ev) != 0 {
		t.Fatalf("evidence: %#v", link["evidence"])
	}
}

func TestAppMapWithoutFilterAndEmptyMap(t *testing.T) {
	e := newEnv(t)
	e.agent.resp = &protocol.Response{Object: json.RawMessage(`{"apps":null}`)}
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/appmap")
	if code != http.StatusOK || e.agent.got.AppMap != nil || e.agent.got.Namespace != "" {
		t.Fatalf("status %d, request %+v", code, e.agent.got)
	}
	if apps, ok := body["apps"].([]any); !ok || len(apps) != 0 {
		t.Fatalf("apps: %#v", body["apps"])
	}
	if links, ok := body["links"].([]any); !ok || len(links) != 0 {
		t.Fatalf("links: %#v", body["links"])
	}
}

func TestAppMapDropsUnknownFields(t *testing.T) {
	e := newEnv(t)
	e.agent.resp = &protocol.Response{Object: json.RawMessage(`{"apps":[{"id":"a","name":"a","components":[],"html":"<script>"}],"extra":1}`)}
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/appmap")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if _, ok := body["extra"]; ok {
		t.Fatal("unknown top-level field passed through")
	}
	if _, ok := body["apps"].([]any)[0].(map[string]any)["html"]; ok {
		t.Fatal("unknown app field passed through")
	}
}

func TestAppMapErrors(t *testing.T) {
	cases := []struct {
		name     string
		cluster  func(e *env) string
		query    string
		resp     *protocol.Response
		err      error
		status   int
		code     string
		noSend   bool
		contains string
	}{
		{name: "unknown cluster", cluster: func(*env) string { return "c-missing" }, status: 404, code: "not_found", noSend: true},
		{name: "bad app id", query: "?app=a%20b", status: 400, code: "bad_request", noSend: true},
		{name: "agent offline", err: tunnel.ErrNotConnected, status: 503, code: "agent_offline"},
		{name: "timeout", err: fmt.Errorf("tunnel: request appmap: %w", context.DeadlineExceeded), status: 504, code: "timeout"},
		{name: "send failed", err: errors.New("tunnel: send request: closed"), status: 502, code: "agent_error"},
		{name: "old agent", resp: &protocol.Response{Error: &protocol.Error{Code: protocol.CodeUnsupported, Message: "request: unknown op"}},
			status: 501, code: "unsupported", contains: "update it"},
		{name: "unknown app", resp: &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "no app x"}},
			status: 404, code: "not_found", contains: "no app x"},
		{name: "too large", resp: &protocol.Response{Error: &protocol.Error{Code: protocol.CodeTooLarge, Message: "big"}},
			status: 422, code: "too_large", contains: "namespace"},
		{name: "forbidden", resp: &protocol.Response{Error: &protocol.Error{Code: protocol.CodeForbidden, Message: "rbac"}},
			status: 502, code: "agent_forbidden"},
		{name: "agent internal", resp: &protocol.Response{Error: &protocol.Error{Code: protocol.CodeInternal, Message: "boom"}},
			status: 502, code: "agent_error", contains: "boom"},
		{name: "no object", resp: &protocol.Response{}, status: 502, code: "agent_error"},
		{name: "broken object", resp: &protocol.Response{Object: json.RawMessage(`{"apps":"nope"}`)}, status: 502, code: "agent_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent.resp, e.agent.err = tc.resp, tc.err
			id := e.active.ID
			if tc.cluster != nil {
				id = tc.cluster(e)
			}
			code, body := e.get(t, "/api/clusters/"+id+"/appmap"+tc.query)
			if code != tc.status || body["error"] != tc.code {
				t.Fatalf("got %d %v, want %d %s", code, body, tc.status, tc.code)
			}
			if tc.noSend && e.agent.got != nil {
				t.Fatalf("request sent to the agent: %+v", e.agent.got)
			}
			if msg, _ := body["message"].(string); !strings.Contains(msg, tc.contains) {
				t.Fatalf("message %q does not contain %q", msg, tc.contains)
			}
		})
	}
}

// The protocol's golden appmap response goes through the portal unchanged:
// every field the page draws survives the decode and encode.
func TestAppMapGoldenRoundTrip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "pkg", "protocol", "testdata", "response-appmap.json"))
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
	e := newEnv(t)
	e.agent.resp = &protocol.Response{Object: env.Payload.Object}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/clusters/"+e.active.ID+"/appmap", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	delete(got, "at")
	var want map[string]any
	if err := json.Unmarshal(env.Payload.Object, &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("map changed on the way through:\n got %s\nwant %s", gb, wb)
	}
}

// KM-35: the chain route passes the id or the query to op object.chain, and
// the agent's answer through; bad input never reaches the agent.
func TestChain(t *testing.T) {
	e := newEnv(t)
	self := protocol.ChainItem{ID: "Deployment/sre-lab-app/url-shortener", Kind: "Deployment", Namespace: "sre-lab-app", Name: "url-shortener"}
	e.agent.resp = &protocol.Response{Object: object(t, protocol.ObjectChain{Object: &self,
		Sections: []protocol.ChainSection{{Key: protocol.ChainPods, Items: []protocol.ChainItem{{ID: "Pod/sre-lab-app/a", Kind: "Pod", Name: "a"}}}}})}
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/chain?id=Deployment/sre-lab-app/url-shortener")
	if code != 200 || e.agent.got.Op != protocol.OpChain || e.agent.got.Chain.ID != "Deployment/sre-lab-app/url-shortener" {
		t.Fatalf("%d %v %+v", code, body, e.agent.got)
	}
	ch := body["chain"].(map[string]any)
	if ch["object"].(map[string]any)["id"] != self.ID || len(ch["sections"].([]any)) != 1 {
		t.Errorf("chain %v", ch)
	}
	e.agent.resp = &protocol.Response{Object: object(t, protocol.ObjectChain{Matches: []protocol.ChainItem{self}})}
	if code, body := e.get(t, "/api/clusters/"+e.active.ID+"/chain?q=url"); code != 200 || e.agent.got.Chain.Query != "url" ||
		len(body["chain"].(map[string]any)["matches"].([]any)) != 1 {
		t.Errorf("query: %d %v", code, body)
	}
	e.agent.got = nil
	for _, q := range []string{"", "?id=x", "?id=Pod/a/b&q=b"} {
		if code, _ := e.get(t, "/api/clusters/"+e.active.ID+"/chain"+q); code != 400 {
			t.Errorf("%q: %d", q, code)
		}
	}
	if e.agent.got != nil {
		t.Error("a bad request reached the agent")
	}
	e.agent.resp = &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "no object"}}
	if code, _ := e.get(t, "/api/clusters/"+e.active.ID+"/chain?id=Pod/a/b"); code != 404 {
		t.Errorf("not found: %d", code)
	}
}
