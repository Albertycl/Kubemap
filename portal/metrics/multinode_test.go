package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

// Three nodes in one cluster, each in a different state: the servers view
// tells "node down" from "node agent down" per node and ties nothing to one
// node name.
func TestServersSeveralNodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl, _ := e.st.EnsureCluster(ctx, "local", "this cluster")
	for _, n := range []struct {
		name string
		ago  time.Duration
	}{{"cp-1", 0}, {"worker-1", 10 * time.Minute}, {"worker-2", 10 * time.Minute}} {
		status := "connected"
		if n.ago > 0 {
			status = "disconnected"
		}
		if err := e.st.UpsertNode(ctx, store.Node{ClusterID: cl.ID, Name: n.name, Status: status, LastSeen: e.now.Add(-n.ago)}); err != nil {
			t.Fatal(err)
		}
	}
	api := NewAPI(e.c, e.st, func(cluster, node string) bool { return node == "cp-1" })
	api.Started = e.now.Add(-time.Hour)
	api.Kube = func(string) (*protocol.Summary, bool) {
		return &protocol.Summary{
			Nodes: protocol.NodeCounts{Total: 4},
			NodeList: []protocol.NodeStatus{
				{Name: "cp-1", Ready: true},
				{Name: "worker-1", Ready: false}, // machine down
				{Name: "worker-2", Ready: true},  // only its agent is gone
				{Name: "worker-3", Ready: true},  // no node agent ever registered
			},
		}, true
	}
	mux := http.NewServeMux()
	api.Routes(func(h http.Handler) http.Handler { return h })(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/servers", nil))
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
	var o Overview
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range o.Servers {
		got[s.Name] = s.State.Code
	}
	want := map[string]string{"cp-1": "ok", "worker-1": "node-down", "worker-2": "agent-down", "worker-3": "no-agent"}
	if len(got) != len(want) {
		t.Fatalf("servers %v", got)
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("%s: state %q, want %q (all: %v)", n, got[n], c, got)
		}
	}
}
