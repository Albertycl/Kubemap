package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

type pressureAgent struct {
	// mu: the servers overview asks several clusters at once.
	mu      sync.Mutex
	asked   map[string]bool
	reports map[string][]protocol.Metrics
	answer  func(id string) (*protocol.Response, error)
}

func (p *pressureAgent) Request(_ context.Context, id string, req *protocol.Request) (*protocol.Response, error) {
	if req.Op != protocol.OpNodePressure {
		return nil, nil
	}
	p.mu.Lock()
	p.asked[id] = true
	if p.reports != nil {
		p.reports[id] = req.NodeMetrics
	}
	p.mu.Unlock()
	return p.answer(id)
}

func TestServersOverview(t *testing.T) {
	e := newEnv(t) // e.cl is pending
	ctx := context.Background()
	lab, _ := e.st.EnsureCluster(ctx, "c-lab", "lab")
	down, _ := e.st.EnsureCluster(ctx, "c-down", "down")

	e.st.UpsertNode(ctx, store.Node{ClusterID: lab.ID, Name: "k8server", Status: "connected", LastSeen: e.now})
	e.st.UpsertNode(ctx, store.Node{ClusterID: lab.ID, Name: "old", Status: "disconnected", LastSeen: e.now.Add(-time.Hour)})
	top := sample(37) // k8server, now
	top.TopProcesses = []protocol.ProcessStats{{PID: 1, Command: "k3s"}}
	e.c.Observe(lab.ID, top)
	m := sample(12)
	m.NodeName = "old"
	e.now = e.now.Add(-2 * time.Minute) // "old" last reported 2 min ago
	e.c.Observe(lab.ID, m)
	e.now = e.now.Add(2 * time.Minute)

	agent := &pressureAgent{asked: map[string]bool{}, reports: map[string][]protocol.Metrics{}, answer: func(id string) (*protocol.Response, error) {
		if id != lab.ID {
			return nil, tunnel.ErrNotConnected
		}
		b, _ := json.Marshal(protocol.NodePressureReport{Nodes: []protocol.NodePressure{
			{Node: "k8server", Ready: true, PSIAvailable: true, Verdict: protocol.VerdictPressure, Summary: "pressure: [cpu]",
				Resources: []protocol.ResourcePressure{{Resource: "cpu", Verdict: protocol.VerdictPressure, Detail: "tasks waited 27%"}}},
			{Node: "k8worker", Ready: false, Verdict: protocol.VerdictUnknown, Summary: "NotReady"},
		}})
		return &protocol.Response{Object: b}, nil
	}}
	api := NewAPI(e.c, e.st, func(cluster, node string) bool { return cluster == lab.ID && node == "k8server" })
	api.Agent = agent
	mux := http.NewServeMux()
	var guarded int
	api.Routes(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; h.ServeHTTP(w, r) })
	})(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/servers", nil))
	if w.Code != 200 || guarded != 1 {
		t.Fatalf("%d guarded %d: %s", w.Code, guarded, w.Body)
	}
	var o Overview
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	byName := map[string]Server{}
	for _, s := range o.Servers {
		byName[s.ClusterName+"/"+s.Name] = s
	}
	if len(o.Servers) != 3 {
		t.Fatalf("servers %+v", o.Servers)
	}
	k := byName["lab/k8server"]
	if !k.Connected || k.Stale || k.Current == nil || k.Current.Point.CPU != 37 || k.Pressure == nil || k.Pressure.Verdict != "pressure" {
		t.Fatalf("k8server %+v", k)
	}
	old := byName["lab/old"]
	if old.Connected || !old.Stale || old.Current == nil || old.Pressure != nil {
		t.Fatalf("old: not connected, last numbers kept but stale: %+v", old)
	}
	wk := byName["lab/k8worker"]
	if wk.Status != "none" || wk.Current != nil || wk.Pressure == nil || wk.Pressure.Ready {
		t.Fatalf("k8worker (no node agent): %+v", wk)
	}
	// An unconnected agent: its reason.
	if o.PressureErrors[down.ID].Error != "agent_offline" {
		t.Fatalf("errors %+v asked %v", o.PressureErrors, agent.asked)
	}
	if _, ok := o.PressureErrors[lab.ID]; ok {
		t.Fatal("lab has a pressure error")
	}
	// The node agents' reports went along (KM-60: the agent judges PSI from
	// them), without top processes or network counters.
	sent := agent.reports[lab.ID]
	if len(sent) != 2 || sent[0].NodeName != "k8server" || sent[1].NodeName != "old" || sent[0].PSI == nil {
		t.Fatalf("node reports sent: %+v", sent)
	}
	for _, m := range sent {
		if m.TopProcesses != nil || m.Network != nil {
			t.Fatalf("report for %s carries processes or network", m.NodeName)
		}
	}
	// The node page still has them.
	if l, _ := e.c.Latest(lab.ID, "k8server"); len(l.Metrics.TopProcesses) != 1 {
		t.Fatal("Reports changed the stored sample")
	}
}

// Without an Agent (the node pages' tests) the overview has no pressure view.
func TestServersWithoutAgent(t *testing.T) {
	e := newEnv(t)
	e.st.UpsertNode(context.Background(), store.Node{ClusterID: e.cl.ID, Name: "k8server", Status: "connected", LastSeen: e.now})
	api := NewAPI(e.c, e.st, nil)
	mux := http.NewServeMux()
	api.Routes(func(h http.Handler) http.Handler { return h })(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/servers", nil))
	var o Overview
	json.Unmarshal(w.Body.Bytes(), &o)
	if w.Code != 200 || len(o.Servers) != 1 || o.Servers[0].Pressure != nil || len(o.PressureErrors) != 0 {
		t.Fatalf("%d %+v", w.Code, o)
	}
}
