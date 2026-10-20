// Package netpol serves the NetworkPolicy checker (KM-36 UI): "can A reach
// B on port X?" with the deciding policy, and a namespace matrix of which
// workloads can reach which Service ports. Every verdict comes from the
// cluster agent's netpol.check op (KM-33), which evaluates the policies in
// its cache; the matrix is many such checks, asked in parallel.
package netpol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/resources"
	"kubemap/portal/store"
)

const (
	checkTimeout  = 10 * time.Second
	matrixTimeout = 60 * time.Second
	// A matrix has at most maxSources rows and maxTargets columns; the
	// checks run parallel at a time.
	maxSources = 20
	maxTargets = 20
	parallel   = 8
)

// API serves (behind the login):
//
//	GET /api/clusters/{id}/netpol/check?fromNamespace=&fromPod=|fromIp=&toNamespace=&toPod=|toService=|toIp=&port=&protocol=
//	GET /api/clusters/{id}/netpol/matrix?namespace=
type API struct {
	store store.Store
	agent agentcall.Requester
}

// NewAPI returns the NetworkPolicy checker API.
func NewAPI(st store.Store, agent agentcall.Requester) *API { return &API{store: st, agent: agent} }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/netpol/check", require(http.HandlerFunc(a.check)))
		mux.Handle("GET /api/clusters/{id}/netpol/matrix", require(http.HandlerFunc(a.matrix)))
	}
}

// ask runs one netpol.check. An agent error is returned as *protocol.Error.
func (a *API) ask(ctx context.Context, clusterID string, q *protocol.NetpolCheckRequest) (*protocol.NetpolResult, error) {
	req := &protocol.Request{Op: protocol.OpNetpolCheck, NetpolCheck: q}
	if err := req.Validate(); err != nil {
		return nil, protocol.AsError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, clusterID, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var res protocol.NetpolResult
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &res) != nil {
		return nil, &protocol.Error{Code: protocol.CodeInternal, Message: "unreadable netpol.check answer"}
	}
	if res.Paths == nil {
		res.Paths = []protocol.NetpolPath{}
	}
	return &res, nil
}

func (a *API) check(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	q := r.URL.Query()
	port, err := strconv.Atoi(q.Get("port"))
	if err != nil || port < 1 || port > 65535 {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "port must be 1 to 65535")
		return
	}
	req := &protocol.NetpolCheckRequest{
		From:     protocol.NetpolEndpoint{Namespace: q.Get("fromNamespace"), Pod: q.Get("fromPod"), IP: q.Get("fromIp")},
		To:       protocol.NetpolEndpoint{Namespace: q.Get("toNamespace"), Pod: q.Get("toPod"), Service: q.Get("toService"), IP: q.Get("toIp")},
		Port:     int32(port),
		Protocol: strings.ToUpper(q.Get("protocol")),
	}
	if req.From.IP != "" {
		req.From.Namespace = ""
	}
	if req.To.IP != "" {
		req.To.Namespace = ""
	}
	res, err := a.ask(r.Context(), c.ID, req)
	if err != nil {
		agentcall.Error(w, err, "check one pod")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"result": res, "at": time.Now().UTC()})
}

// Source is a matrix row: one pod stands for its workload.
type Source struct {
	Label string `json:"label"` // the owning workload ("ReplicaSet/web-59f8"), else the pod
	Pod   string `json:"pod"`
}

// Target is a matrix column: one port of one Service.
type Target struct {
	Service  string `json:"service"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
	PortName string `json:"portName,omitempty"`
}

// Cell is one verdict of the matrix; Error when the check failed.
// NoDestination when the Service has no pods behind it: nothing to allow
// or block (the demo's selector-typo Service), which is not "blocked".
type Cell struct {
	Allowed       bool   `json:"allowed"`
	NoDestination bool   `json:"noDestination,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Error         string `json:"error,omitempty"`
}

// cellOf turns a netpol.check result into a matrix cell.
func cellOf(res *protocol.NetpolResult) Cell {
	if len(res.Paths) == 0 {
		return Cell{NoDestination: true, Summary: res.Summary}
	}
	return Cell{Allowed: res.Allowed, Summary: res.Summary}
}

// Matrix is the answer of GET .../netpol/matrix.
type Matrix struct {
	Namespace string   `json:"namespace"`
	Sources   []Source `json:"sources"`
	Targets   []Target `json:"targets"`
	// Cells[i][j] is Sources[i] → Targets[j].
	Cells [][]Cell `json:"cells"`
	// Truncated when the namespace had more workloads or Service ports
	// than a matrix shows.
	Truncated bool      `json:"truncated"`
	At        time.Time `json:"at"`
}

func (a *API) matrix(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "choose a namespace")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), matrixTimeout)
	defer cancel()

	podItems, podsCut, err := resources.ListAll(ctx, a.agent, c.ID, protocol.Request{Resource: "pods", Namespace: ns})
	if err != nil {
		agentcall.Error(w, err, "choose one namespace")
		return
	}
	svcItems, svcsCut, err := resources.ListAll(ctx, a.agent, c.ID, protocol.Request{Resource: "services", Namespace: ns})
	if err != nil {
		agentcall.Error(w, err, "choose one namespace")
		return
	}
	m := Matrix{Namespace: ns, Truncated: podsCut || svcsCut, At: time.Now().UTC()}
	m.Sources, m.Truncated = sources(podItems, m.Truncated)
	m.Targets, m.Truncated = targets(svcItems, m.Truncated)

	m.Cells = make([][]Cell, len(m.Sources))
	for i := range m.Cells {
		m.Cells[i] = make([]Cell, len(m.Targets))
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, s := range m.Sources {
		for j, t := range m.Targets {
			wg.Add(1)
			go func(i, j int, s Source, t Target) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res, err := a.ask(ctx, c.ID, &protocol.NetpolCheckRequest{
					From: protocol.NetpolEndpoint{Namespace: ns, Pod: s.Pod},
					To:   protocol.NetpolEndpoint{Namespace: ns, Service: t.Service},
					Port: t.Port, Protocol: t.Protocol,
				})
				if err != nil {
					_, _, msg := errorText(err)
					m.Cells[i][j] = Cell{Error: msg}
					return
				}
				m.Cells[i][j] = cellOf(res)
			}(i, j, s, t)
		}
	}
	wg.Wait()
	httpjson.Write(w, http.StatusOK, m)
}

func errorText(err error) (int, string, string) {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return agentcall.Status(pe, "")
	}
	return agentcall.TunnelStatus(err)
}

// sources picks one running pod per owning workload (pods of a ReplicaSet
// share their policies' view, as they share labels), at most maxSources.
func sources(items []json.RawMessage, cut bool) ([]Source, bool) {
	byOwner := map[string]Source{}
	for _, raw := range items {
		p, err := resources.PodView(raw)
		if err != nil || p.Phase != "Running" {
			continue
		}
		label := p.Owner
		if label == "" {
			label = "Pod/" + p.Name
		}
		if _, dup := byOwner[label]; !dup {
			byOwner[label] = Source{Label: label, Pod: p.Name}
		}
	}
	out := make([]Source, 0, len(byOwner))
	for _, s := range byOwner {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	if len(out) > maxSources {
		return out[:maxSources], true
	}
	return out, cut
}

// targets lists every port of every Service with a selector (a Service
// without one has no pods for the policies to protect), at most maxTargets.
func targets(items []json.RawMessage, cut bool) ([]Target, bool) {
	var out []Target
	for _, raw := range items {
		s, err := resources.ServiceView(raw)
		if err != nil || len(s.Selector) == 0 || s.Type == "ExternalName" {
			continue
		}
		for _, p := range s.Ports {
			out = append(out, Target{Service: s.Name, Port: p.Port, Protocol: p.Protocol, PortName: p.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Port < out[j].Port
	})
	if len(out) > maxTargets {
		return out[:maxTargets], true
	}
	return out, cut
}
