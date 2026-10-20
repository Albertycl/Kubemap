package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/nodestate"
	"kubemap/portal/store"
)

// The all-servers overview (KM-66): every node of every cluster in one
// list, with its latest numbers from its node agent and, from the cluster
// agent, whether its workloads are really waiting for a resource
// (node.pressure, KM-60: kubelet conditions and kernel PSI).

const (
	// pressureTimeout bounds one node.pressure request; the agent reads each
	// node's kubelet summary.
	pressureTimeout = 10 * time.Second
	// staleAfter is how old a sample may be before it is shown as stale: a
	// node agent sends every 10 s, so three missed samples.
	staleAfter = 30 * time.Second
	// pressureParallel is how many clusters are asked at a time.
	pressureParallel = 4
)

// Server is one node in the overview.
type Server struct {
	ClusterID     string `json:"clusterId"`
	ClusterName   string `json:"clusterName"`
	ClusterStatus string `json:"clusterStatus"`
	Name          string `json:"name"`
	// Status and Connected are the node agent's; "none" when only the
	// cluster agent knows the node (no node agent registered for it).
	Status    string    `json:"status"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"lastSeen,omitzero"`
	Current   *current  `json:"current,omitempty"`
	// Stale is true when Current is older than staleAfter or the node agent
	// is not connected: the numbers are the last known, not now.
	Stale bool `json:"stale,omitempty"`
	// Pressure is the cluster agent's view of the node, when it answered.
	Pressure *protocol.NodePressure `json:"pressure,omitempty"`
	// State tells node down from node agent down (KM-65).
	State nodestate.State `json:"state"`
}

// Overview is the answer of GET /api/servers.
type Overview struct {
	Servers []Server `json:"servers"`
	// PressureErrors say, per cluster ID, why there is no pressure view
	// (agent offline, an agent too old for node.pressure).
	PressureErrors map[string]httpjson.ErrorBody `json:"pressureErrors,omitempty"`
	At             time.Time                     `json:"at"`
}

func (a *API) servers(w http.ResponseWriter, r *http.Request) {
	clusters, err := a.store.ListClusters(r.Context())
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	now := a.c.now()
	out := Overview{Servers: []Server{}, At: now.UTC()}
	pressure := a.pressures(r.Context(), clusters)
	for _, c := range clusters {
		nodes, err := a.store.ListNodes(r.Context(), c.ID)
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		seen := map[string]bool{}
		kube, listed := a.kube(c.ID)
		for _, n := range nodes {
			v := a.view(c.ID, n, kube)
			s := Server{ClusterID: c.ID, ClusterName: c.Name, ClusterStatus: c.Status, Name: n.Name,
				Status: v.Status, Connected: v.Connected, LastSeen: v.LastSeen, Current: v.Current, State: v.State}
			s.Stale = s.Current != nil && (!s.Connected || now.Sub(s.Current.At) > staleAfter)
			if p, ok := pressure[c.ID].nodes[n.Name]; ok {
				s.Pressure = p
			}
			seen[n.Name] = true
			out.Servers = append(out.Servers, s)
		}
		// Nodes the cluster agent sees but no node agent reports for: from
		// its summary, and from its pressure answer.
		for name, p := range pressure[c.ID].nodes {
			if !seen[name] {
				seen[name] = true
				out.Servers = append(out.Servers, Server{ClusterID: c.ID, ClusterName: c.Name, ClusterStatus: c.Status,
					Name: name, Status: "none", Pressure: p, State: nodestate.Classify(nodestate.Agent{}, kube(name), now)})
			}
		}
		for _, name := range listed {
			if !seen[name] {
				out.Servers = append(out.Servers, Server{ClusterID: c.ID, ClusterName: c.Name, ClusterStatus: c.Status,
					Name: name, Status: "none", State: nodestate.Classify(nodestate.Agent{}, kube(name), now)})
			}
		}
		if e := pressure[c.ID].err; e != nil {
			if out.PressureErrors == nil {
				out.PressureErrors = map[string]httpjson.ErrorBody{}
			}
			out.PressureErrors[c.ID] = *e
		}
	}
	sort.SliceStable(out.Servers, func(i, j int) bool {
		x, y := out.Servers[i], out.Servers[j]
		if x.ClusterName != y.ClusterName {
			return x.ClusterName < y.ClusterName
		}
		return x.Name < y.Name
	})
	httpjson.Write(w, http.StatusOK, out)
}

type clusterPressure struct {
	nodes map[string]*protocol.NodePressure
	err   *httpjson.ErrorBody
}

// pressures asks every cluster agent for node.pressure, in
// parallel. Without an Agent (tests of the node pages) it asks nobody.
func (a *API) pressures(ctx context.Context, clusters []store.Cluster) map[string]clusterPressure {
	out := make(map[string]clusterPressure, len(clusters))
	if a.Agent == nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, pressureParallel)
	for _, c := range clusters {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cp := a.pressure(ctx, id)
			mu.Lock()
			out[id] = cp
			mu.Unlock()
		}(c.ID)
	}
	wg.Wait()
	return out
}

func (a *API) pressure(ctx context.Context, clusterID string) clusterPressure {
	ctx, cancel := context.WithTimeout(ctx, pressureTimeout)
	defer cancel()
	// The node agents' reports carry the PSI the cluster agent judges.
	resp, err := a.Agent.Request(ctx, clusterID, &protocol.Request{Op: protocol.OpNodePressure, NodeMetrics: a.c.Reports(clusterID)})
	if err != nil {
		_, code, msg := agentcall.TunnelStatus(err)
		return clusterPressure{err: &httpjson.ErrorBody{Error: code, Message: msg}}
	}
	if resp.Error != nil {
		_, code, msg := agentcall.Status(resp.Error, "")
		return clusterPressure{err: &httpjson.ErrorBody{Error: code, Message: msg}}
	}
	var rep protocol.NodePressureReport
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &rep) != nil {
		return clusterPressure{err: &httpjson.ErrorBody{Error: "agent_error", Message: "the cluster agent sent an unreadable pressure report"}}
	}
	cp := clusterPressure{nodes: map[string]*protocol.NodePressure{}}
	for i := range rep.Nodes {
		n := rep.Nodes[i]
		if n.Resources == nil {
			n.Resources = []protocol.ResourcePressure{}
		}
		cp.nodes[n.Node] = &n
	}
	return cp
}
