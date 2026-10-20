package access

import (
	"context"
	"encoding/json"
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

// fakeAgent answers rbac.whocan like a small cluster, and lists
// ServiceAccounts and Argo CD applications.
type fakeAgent struct {
	mu      sync.Mutex
	reqs    []protocol.Request
	noArgo  bool
	fail    map[string]bool // "verb resource" → agent error
	offline bool
	apps    []json.RawMessage
}

func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, *req)
	f.mu.Unlock()
	if f.offline {
		return nil, tunnel.ErrNotConnected
	}
	switch req.Op {
	case protocol.OpList:
		switch req.Resource {
		case "serviceaccounts":
			var items []json.RawMessage
			for _, sa := range [][2]string{{"monitoring", "grafana"}, {"monitoring", "prometheus"}, {"argocd", "argocd-server"}} {
				if req.Namespace == "" || req.Namespace == sa[0] {
					items = append(items, json.RawMessage(`{"metadata":{"namespace":"`+sa[0]+`","name":"`+sa[1]+`"}}`))
				}
			}
			return &protocol.Response{Items: items}, nil
		case "applications":
			if f.noArgo {
				return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "the server could not find the requested resource"}}, nil
			}
			return &protocol.Response{Items: f.apps}, nil
		}
	case protocol.OpRBACWhoCan:
		q := req.RBAC
		if f.fail[q.Verb+" "+q.Resource] {
			return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeUnavailable, Message: "RBAC is not cached yet"}}, nil
		}
		var res protocol.RBACResult
		switch q.Mode() {
		case protocol.RBACWhoCan:
			res.Mode = protocol.RBACWhoCan
			res.Subjects = []protocol.RBACWho{{Kind: "Group", Name: "system:masters", Binding: "ClusterRoleBinding//cluster-admin", Role: "ClusterRole//cluster-admin", Scope: "cluster"}}
			if q.Resource == "secrets" {
				res.Subjects = append(res.Subjects, protocol.RBACWho{Kind: "ServiceAccount", Namespace: "monitoring", Name: "grafana",
					Binding: "ClusterRoleBinding//grafana", Role: "ClusterRole//grafana", Scope: "cluster"})
			}
			if q.Resource == "configmaps" {
				res.Subjects = append(res.Subjects, protocol.RBACWho{Kind: "Group", Name: "system:authenticated", Everyone: true,
					Binding: "RoleBinding/monitoring/oops", Role: "Role/monitoring/cm", Scope: q.Namespace, ResourceNames: []string{"dash"}})
			}
		case protocol.RBACChain:
			res.Mode, res.Subject = protocol.RBACChain, q.Subject
			res.Grants = []protocol.RBACGrant{
				{Binding: "ClusterRoleBinding//x", Role: "ClusterRole//x", Scope: "cluster", Via: "direct"},
				{Binding: "ClusterRoleBinding//calico-tiered-policy-passthrough", Role: "ClusterRole//calico-tiered-policy-passthrough", Scope: "cluster",
					Via: "group system:authenticated", Risky: []string{"wildcard"}},
			}
			res.Risky = []string{"wildcard"}
			if q.Subject.Name == "prometheus" {
				// A real disaster: a risky grant to every account.
				res.Grants = append(res.Grants, protocol.RBACGrant{Binding: "ClusterRoleBinding//oops", Role: "ClusterRole//cluster-admin",
					Scope: "cluster", Via: "group system:serviceaccounts", Risky: []string{"read secrets"}})
			}
			if q.Subject.Name == "grafana" {
				res.Grants[0].Risky = []string{"read secrets", "wildcard"}
				res.Risky = []string{"read secrets", "wildcard"}
			}
		case protocol.RBACCheck:
			allowed := q.Subject.Name == "grafana" && q.Resource == "secrets"
			res.Mode, res.Subject, res.Allowed = protocol.RBACCheck, q.Subject, &allowed
		}
		res.Notes = []string{"RBAC authorizer only."}
		obj, _ := json.Marshal(res)
		return &protocol.Response{Object: obj}, nil
	}
	return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeBadRequest, Message: "unexpected"}}, nil
}

type env struct {
	agent   *fakeAgent
	mux     *http.ServeMux
	active  store.Cluster
	pending store.Cluster
	guarded int
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
	raw, err := os.ReadFile("testdata/applications.json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []json.RawMessage }
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	e := &env{agent: &fakeAgent{apps: list.Items, fail: map[string]bool{}}, mux: http.NewServeMux(), active: active, pending: pending}
	NewAPI(st, e.agent).Routes(func(h http.Handler) http.Handler {
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

func (e *env) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Test-Login", "yes")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestLoginRequired(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/rbac?verb=get&resource=pods", "/rbac/accounts", "/rbac/namespaces/monitoring", "/argocd"} {
		req := httptest.NewRequest("GET", "/api/clusters/"+e.active.ID+p, nil)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s without login: %d", p, rec.Code)
		}
	}
	if e.guarded != 4 {
		t.Fatalf("guarded %d", e.guarded)
	}
}

func TestQueryModes(t *testing.T) {
	e := newEnv(t)
	base := "/api/clusters/" + e.active.ID + "/rbac?"

	code, body := e.get(t, base+"subjectKind=ServiceAccount&subjectNamespace=monitoring&subject=grafana")
	res := body["result"].(map[string]any)
	if code != 200 || res["mode"] != "chain" || res["risky"].([]any)[0] != "read secrets" || len(res["grants"].([]any)) != 2 {
		t.Fatalf("chain: %d %v", code, body)
	}
	code, body = e.get(t, base+"subjectKind=ServiceAccount&subjectNamespace=monitoring&subject=grafana&verb=get&resource=secrets&namespace=sre-lab-app")
	if res := body["result"].(map[string]any); code != 200 || res["mode"] != "check" || res["allowed"] != true {
		t.Fatalf("check: %d %v", code, body)
	}
	code, body = e.get(t, base+"verb=get&resource=secrets&namespace=monitoring")
	if res := body["result"].(map[string]any); code != 200 || res["mode"] != "whocan" || len(res["subjects"].([]any)) != 2 {
		t.Fatalf("whocan: %d %v", code, body)
	}
	code, _ = e.get(t, base+"subjectKind=User&subject=alice&groups=dev-team,system:masters&verb=list&resource=pods")
	last := e.agent.reqs[len(e.agent.reqs)-1].RBAC
	if code != 200 || strings.Join(last.Subject.Groups, ",") != "dev-team,system:masters" {
		t.Fatalf("user with groups: %d %+v", code, last.Subject)
	}

	for name, q := range map[string]string{
		"nothing":         "",
		"verb only":       "verb=get",
		"bad kind":        "subjectKind=Robot&subject=x",
		"sa no namespace": "subjectKind=ServiceAccount&subject=grafana",
		"bad group":       "subjectKind=User&subject=a&groups=a%20b",
		"bad namespace":   "verb=get&resource=pods&namespace=Not_A_NS",
	} {
		if code, body := e.get(t, base+q); code != 400 {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
	if code, _ := e.get(t, "/api/clusters/c-none/rbac?verb=get&resource=pods"); code != 404 {
		t.Errorf("unknown cluster: %d", code)
	}
	e.agent.offline = true
	if code, body := e.get(t, base+"verb=get&resource=pods"); code != 503 || body["error"] != "agent_offline" {
		t.Errorf("offline: %d %v", code, body)
	}
}

func TestAccounts(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/rbac/accounts")
	accts := body["accounts"].([]any)
	if code != 200 || len(accts) != 3 || accts[0].(map[string]any)["namespace"] != "argocd" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestNamespaceView(t *testing.T) {
	e := newEnv(t)
	e.agent.fail["delete pods"] = true
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/rbac/namespaces/monitoring")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	var v NamespaceView
	b, _ := json.Marshal(body["view"])
	json.Unmarshal(b, &v)
	if len(v.Actions) != len(Actions) || v.Namespace != "monitoring" {
		t.Fatalf("actions %d", len(v.Actions))
	}
	// Everyone-groups first, then by the number of rights.
	if len(v.Subjects) != 3 || v.Subjects[0].Name != "system:authenticated" || v.Subjects[1].Name != "system:masters" {
		t.Fatalf("subjects %+v", v.Subjects)
	}
	if g := v.Subjects[0].Grants["update-configmaps"]; len(g.ResourceNames) != 1 || g.Role != "Role/monitoring/cm" {
		t.Fatalf("names-only grant %+v", g)
	}
	if len(v.Subjects[1].Grants) != len(Actions)-1 || v.Errors["delete-pods"] == "" {
		t.Fatalf("masters grants %d, errors %v", len(v.Subjects[1].Grants), v.Errors)
	}
	grafana := v.Subjects[2]
	if _, ok := grafana.Grants["get-secrets"]; !ok || grafana.Namespace != "monitoring" {
		t.Fatalf("grafana %+v", grafana)
	}
	// The namespace's own accounts, riskiest first; only its namespace.
	// Grafana's own bindings give both. Prometheus has wildcard only through
	// Calico's known platform grant (quiet), and read secrets through a
	// grant to every ServiceAccount (loud, and at the top of the view).
	g, p := v.Accounts[0], v.Accounts[1]
	if len(v.Accounts) != 2 || g.Name != "grafana" || strings.Join(g.Risky, ",") != "read secrets,wildcard" || len(g.Everyone) != 0 || len(g.Platform) != 0 ||
		len(p.Risky) != 0 || strings.Join(p.Everyone, ",") != "read secrets" || strings.Join(p.Platform, ",") != "wildcard" || p.Grants != 3 ||
		strings.Join(v.EveryoneRisky, ",") != "read secrets" {
		t.Fatalf("accounts %+v", v.Accounts)
	}
	if len(v.Notes) == 0 {
		t.Fatal("the authorizer note is missing")
	}
	if code, _ := e.get(t, "/api/clusters/"+e.active.ID+"/rbac/namespaces/Bad_NS"); code != 400 {
		t.Fatalf("bad namespace: %d", code)
	}
	// The first question failing fails the view.
	e.agent.fail["get secrets"] = true
	if code, body := e.get(t, "/api/clusters/"+e.active.ID+"/rbac/namespaces/monitoring"); code != 503 {
		t.Fatalf("unavailable: %d %v", code, body)
	}
}

func TestArgoCD(t *testing.T) {
	e := newEnv(t)
	code, body := e.get(t, "/api/clusters/"+e.active.ID+"/argocd")
	if code != 200 || body["installed"] != true {
		t.Fatalf("%d %v", code, body)
	}
	var apps []ArgoApp
	b, _ := json.Marshal(body["apps"])
	json.Unmarshal(b, &apps)
	if len(apps) != 2 {
		t.Fatalf("%d apps", len(apps))
	}
	// Out of sync first; its out-of-sync resources first.
	u := apps[0]
	if u.Name != "url-shortener" || u.Sync != "OutOfSync" || u.MapID != "argocd/url-shortener" || len(u.Resources) != 2 || u.Resources[0].Status != "OutOfSync" {
		t.Fatalf("url-shortener %+v", u)
	}
	f := apps[1]
	if f.Name != "flask-vue" || f.Sync != "Synced" || f.Health != "Healthy" || f.OperationPhase != "Succeeded" || len(f.Resources) != 5 || len(f.Sources) == 0 || f.Sources[0].RepoURL == "" {
		t.Fatalf("flask-vue %+v", f)
	}
	if last := e.agent.reqs[len(e.agent.reqs)-1]; last.Group != "argoproj.io" || last.Version != "v1alpha1" || last.Resource != "applications" {
		t.Fatalf("request %+v", last)
	}

	e.agent.noArgo = true
	code, body = e.get(t, "/api/clusters/"+e.active.ID+"/argocd")
	if code != 200 || body["installed"] != false || len(body["apps"].([]any)) != 0 {
		t.Fatalf("no Argo CD: %d %v", code, body)
	}
}

func TestArgoViewDefaults(t *testing.T) {
	app, err := ArgoView(json.RawMessage(`{"metadata":{"name":"a","namespace":"argocd"},"spec":{"destination":{"name":"in-cluster"},
		"syncPolicy":{"automated":{"prune":true}}},"status":{"resources":[{"kind":"Deployment","name":"d","health":{"status":"Degraded"}},{"kind":"Service","name":"s","status":"Synced"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if app.Sync != "Unknown" || app.Health != "Unknown" || app.DestServer != "in-cluster" || !app.AutoSync || !app.Prune || app.SelfHeal ||
		app.Resources[0].Health != "Degraded" || len(app.Sources) != 0 || app.Sources == nil {
		t.Fatalf("%+v", app)
	}
	if appRank(app) != 1 {
		t.Fatalf("rank %d", appRank(app))
	}
}

// KM-41: the last operation in full, and whether one is running. The
// operationState is flask-vue's on the lab cluster (2026-09-06), trimmed.
func TestArgoViewLastOperation(t *testing.T) {
	done := `{"metadata":{"name":"flask-vue","namespace":"argocd"},"spec":{"project":"default"},
	  "status":{"operationState":{"phase":"Succeeded","message":"successfully synced (all tasks run)",
	    "startedAt":"2026-09-06T08:42:56Z","finishedAt":"2026-09-06T08:42:59Z",
	    "operation":{"initiatedBy":{"username":"admin"},"sync":{"syncStrategy":{"hook":{}}}},
	    "syncResult":{"revision":"89ab3fbc67f8aa652fed162bec69bbfe36cf1da8"}}}}`
	a, err := ArgoView(json.RawMessage(done))
	if err != nil {
		t.Fatal(err)
	}
	if a.OperationBy != "admin" || a.OperationRevision != "89ab3fbc67f8aa652fed162bec69bbfe36cf1da8" || a.OperationRunning ||
		a.OperationStarted.IsZero() || a.OperationFinished.Sub(a.OperationStarted) != 3*time.Second || a.OperationPhase != "Succeeded" {
		t.Errorf("done = %+v", a)
	}
	// Asked from Kubemap, not picked up yet: operation set, no new state.
	asked := `{"metadata":{"name":"u","namespace":"argocd"},"spec":{},
	  "operation":{"initiatedBy":{"username":"kubemap:admin"},"sync":{"syncStrategy":{"hook":{}}}},
	  "status":{"operationState":{"phase":"Succeeded","operation":{"initiatedBy":{"username":"admin"}}}}}`
	if a, _ := ArgoView(json.RawMessage(asked)); !a.OperationRunning || a.OperationBy != "kubemap:admin" {
		t.Errorf("asked, not picked up: running %v by %q (want kubemap:admin, not the last user)", a.OperationRunning, a.OperationBy)
	}
	running := `{"metadata":{"name":"u","namespace":"argocd"},"spec":{},
	  "status":{"operationState":{"phase":"Running","operation":{"initiatedBy":{"automated":true}}}}}`
	if a, _ := ArgoView(json.RawMessage(running)); !a.OperationRunning || a.OperationBy != "automated sync" {
		t.Errorf("running = %+v", a)
	}
}
