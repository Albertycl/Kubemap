package metrics

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/nodestate"
	"kubemap/portal/store"
)

// Ranges the node page offers, and the step each is shown at, so a chart
// has at most a few hundred points.
var ranges = map[string]struct{ span, step time.Duration }{
	"1h":  {time.Hour, time.Minute},
	"6h":  {6 * time.Hour, 2 * time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
}

// API serves node health (behind the login):
//
//	GET /api/clusters/{id}/nodes              every node with its latest numbers
//	GET /api/clusters/{id}/nodes/{node}?range=1h|6h|24h
//	                                          latest sample in full, and the history
//	GET /api/servers                          every node of every cluster (KM-66)
type API struct {
	c     *Collector
	store store.Store
	// Connected reports whether a node agent holds a connection (tunnel.Hub).
	Connected func(clusterID, node string) bool
	// Agent asks cluster agents for node.pressure in the all-servers
	// overview (tunnel.Hub); nil leaves the pressure view out.
	Agent agentcall.Requester
	// Kube returns the cluster agent's latest summary and whether it is
	// connected (tunnel.Hub), for each node's Kubernetes Ready (KM-65); nil
	// means no Kubernetes view.
	Kube func(clusterID string) (*protocol.Summary, bool)
	// Started is when the portal started; node agent silence is counted
	// from it at the earliest, so a portal restart does not make every
	// node read "node agent down" before its agent reconnects (KM-65).
	Started time.Time
}

// NewAPI returns the node health API; Started is set to now.
func NewAPI(c *Collector, st store.Store, connected func(clusterID, node string) bool) *API {
	return &API{c: c, store: st, Connected: connected, Started: c.now()}
}

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/nodes", require(http.HandlerFunc(a.list)))
		mux.Handle("GET /api/clusters/{id}/nodes/{node}", require(http.HandlerFunc(a.node)))
		mux.Handle("GET /api/servers", require(http.HandlerFunc(a.servers)))
	}
}

type current struct {
	At     time.Time `json:"at"`
	Point  Point     `json:"point"`
	HasPSI bool      `json:"hasPsi"`
	Load1  float64   `json:"load1"`
	Cores  int       `json:"cores"`
}

type nodeView struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"lastSeen"`
	// State crosses the node agent's heartbeat with Kubernetes' Ready
	// condition: node down, node agent down, unreachable … (KM-65).
	State nodestate.State `json:"state"`
	// Current is missing until the node sends metrics after a portal start.
	Current *current `json:"current,omitempty"`
}

func (a *API) cluster(w http.ResponseWriter, r *http.Request) (store.Cluster, bool) {
	c, err := a.store.GetCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such cluster")
		return c, false
	}
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return c, false
	}
	return c, true
}

// kube returns the Kubernetes witness for a cluster's nodes, and the node
// names Kubernetes lists.
func (a *API) kube(clusterID string) (func(string) nodestate.Kube, []string) {
	if a.Kube == nil {
		return func(string) nodestate.Kube { return nodestate.Kube{} }, nil
	}
	sum, connected := a.Kube(clusterID)
	return nodestate.KubeView(sum, connected), nodestate.Listed(sum, connected)
}

func (a *API) view(clusterID string, n store.Node, kube func(string) nodestate.Kube) nodeView {
	v := nodeView{Name: n.Name, Status: n.Status, LastSeen: n.LastSeen}
	if a.Connected != nil {
		v.Connected = a.Connected(clusterID, n.Name)
	}
	v.State = nodestate.Classify(nodestate.Agent{Known: true, Connected: v.Connected, LastSeen: n.LastSeen, Since: a.Started}, kube(n.Name), a.c.now())
	if s, ok := a.c.Latest(clusterID, n.Name); ok {
		v.Current = &current{At: s.At, Point: s.Point, HasPSI: s.HasPSI, Load1: s.Metrics.Load.Load1, Cores: s.Metrics.CPU.Cores}
	}
	return v
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	c, ok := a.cluster(w, r)
	if !ok {
		return
	}
	nodes, err := a.store.ListNodes(r.Context(), c.ID)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	kube, listed := a.kube(c.ID)
	out := make([]nodeView, 0, len(nodes))
	seen := map[string]bool{}
	for _, n := range nodes {
		out = append(out, a.view(c.ID, n, kube))
		seen[n.Name] = true
	}
	// Nodes Kubernetes lists but no node agent ever reported for.
	for _, name := range listed {
		if !seen[name] {
			out = append(out, nodeView{Name: name, Status: "none",
				State: nodestate.Classify(nodestate.Agent{}, kube(name), a.c.now())})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	httpjson.Write(w, http.StatusOK, map[string]any{"nodes": out})
}

// SeriesPoint is one step of a node's history. A step with no data (the
// node was not reporting) is left out; the UI draws a gap there. The PSI
// values are null in a step without PSI (the kernel does not report it),
// so the chart shows a gap, not 0 %.
type SeriesPoint struct {
	T      time.Time `json:"t"`
	CPU    float64   `json:"cpu"`
	Memory float64   `json:"memory"`
	Swap   float64   `json:"swap"`
	Disk   float64   `json:"disk"`
	Inodes float64   `json:"inodes"`
	PSICPU *float64  `json:"psiCpu"`
	PSIMem *float64  `json:"psiMemory"`
	PSIIO  *float64  `json:"psiIo"`
}

func (a *API) node(w http.ResponseWriter, r *http.Request) {
	c, ok := a.cluster(w, r)
	if !ok {
		return
	}
	name := r.PathValue("node")
	rg := r.URL.Query().Get("range")
	if rg == "" {
		rg = "1h"
	}
	spec, ok := ranges[rg]
	if !ok {
		httpjson.Error(w, http.StatusBadRequest, "bad_range", "range must be 1h, 6h or 24h")
		return
	}
	nodes, err := a.store.ListNodes(r.Context(), c.ID)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var found *store.Node
	for i := range nodes {
		if nodes[i].Name == name {
			found = &nodes[i]
		}
	}
	if found == nil {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	now := a.c.now()
	from := now.Add(-spec.span).Truncate(spec.step)
	rs, err := a.store.ListRollups(r.Context(), c.ID, name, from)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	kube, _ := a.kube(c.ID)
	resp := map[string]any{
		"node":   a.view(c.ID, *found, kube),
		"range":  rg,
		"from":   from,
		"to":     now,
		"step":   spec.step.Seconds(),
		"series": Downsample(rs, spec.step),
	}
	if s, ok := a.c.Latest(c.ID, name); ok {
		resp["latest"] = s
	}
	httpjson.Write(w, http.StatusOK, resp)
}

// Downsample averages rollups (oldest first) into steps of the given size.
// PSI is averaged over the rollups that have it.
func Downsample(rs []store.Rollup, step time.Duration) []SeriesPoint {
	out := []SeriesPoint{}
	var cur *SeriesPoint
	var psi Point // PSI sums of the current step
	n, nPSI := 0, 0
	flush := func() {
		if cur == nil {
			return
		}
		f := float64(n)
		cur.CPU, cur.Memory, cur.Swap = cur.CPU/f, cur.Memory/f, cur.Swap/f
		cur.Disk, cur.Inodes = cur.Disk/f, cur.Inodes/f
		if nPSI > 0 {
			fp := float64(nPSI)
			c, m, i := psi.PSICPU/fp, psi.PSIMem/fp, psi.PSIIO/fp
			cur.PSICPU, cur.PSIMem, cur.PSIIO = &c, &m, &i
		}
		out = append(out, *cur)
		cur, n, nPSI, psi = nil, 0, 0, Point{}
	}
	for _, r := range rs {
		t := r.Bucket.Truncate(step)
		if cur != nil && !cur.T.Equal(t) {
			flush()
		}
		if cur == nil {
			cur = &SeriesPoint{T: t}
		}
		cur.CPU += r.CPUPct
		cur.Memory += r.MemPct
		cur.Swap += r.SwapPct
		cur.Disk += r.DiskPct
		cur.Inodes += r.InodesPct
		n++
		if r.HasPSI {
			psi.PSICPU += r.PSICPU
			psi.PSIMem += r.PSIMemory
			psi.PSIIO += r.PSIIO
			nPSI++
		}
	}
	flush()
	return out
}
