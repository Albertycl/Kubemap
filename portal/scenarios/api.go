// Package scenarios serves the scenario engine (KM-82): the embedded
// scenarios (package kubemap/scenarios) and their read-only live checks,
// which it runs through the cluster agent and turns into a verdict for the
// step: good, bad, warn or unknown, with the evidence that decided it.
package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
	"kubemap/scenarios"
)

// checkTimeout bounds one live check; pod.debug is the slowest (logs,
// events and policies of one pod).
const checkTimeout = 30 * time.Second

// Requester sends a request to a cluster agent (tunnel.Hub).
type Requester interface {
	Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error)
}

// API serves (behind the login):
//
//	GET /api/scenarios                              every scenario, short
//	GET /api/scenarios/{sid}                        one scenario in full
//	GET /api/clusters/{id}/scenarios/{sid}/steps/{n}/check?ns=&svc=&...
//	                                                run step n's live check
type API struct {
	store store.Store
	agent Requester
	// Reports gives a cluster's latest node agent reports
	// (metrics.Collector.Reports); node.pressure and health.check judge PSI
	// from them (the cluster agent makes no kubelet calls). Nil sends none.
	Reports func(clusterID string) []protocol.Metrics
	list    []scenarios.Scenario
	byID    map[string]*scenarios.Scenario
}

// NewAPI loads the embedded scenarios. A bad file is an error here too,
// though the scenarios package's tests catch it at build time.
func NewAPI(st store.Store, agent Requester) (*API, error) {
	list, err := scenarios.Load()
	if err != nil {
		return nil, err
	}
	a := &API{store: st, agent: agent, list: list, byID: map[string]*scenarios.Scenario{}}
	for i := range a.list {
		a.byID[a.list[i].ID] = &a.list[i]
	}
	return a, nil
}

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/scenarios", require(http.HandlerFunc(a.index)))
		mux.Handle("GET /api/scenarios/{sid}", require(http.HandlerFunc(a.one)))
		mux.Handle("GET /api/clusters/{id}/scenarios/{sid}/steps/{n}/check", require(http.HandlerFunc(a.check)))
	}
}

// Summary is one scenario in the list.
type Summary struct {
	ID       string   `json:"id"`
	Area     string   `json:"area"`
	Title    string   `json:"title"`
	Question string   `json:"question"`
	Symptoms []string `json:"symptoms,omitempty"`
	Steps    int      `json:"steps"`
	Checks   int      `json:"checks"` // steps with a live check
	Params   []string `json:"params"`
	Lab      string   `json:"lab,omitempty"`
}

func (a *API) index(w http.ResponseWriter, _ *http.Request) {
	out := make([]Summary, 0, len(a.list))
	for _, s := range a.list {
		n := 0
		for _, st := range s.Steps {
			if st.Check != nil {
				n++
			}
		}
		out = append(out, Summary{ID: s.ID, Area: s.Area, Title: s.Title, Question: s.Question, Symptoms: s.Symptoms,
			Steps: len(s.Steps), Checks: n, Params: s.Params, Lab: s.Lab})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"scenarios": out, "areas": scenarios.Areas, "placeholders": scenarios.Placeholders})
}

func (a *API) one(w http.ResponseWriter, r *http.Request) {
	s, ok := a.byID[r.PathValue("sid")]
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such scenario")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"scenario": s, "placeholders": scenarios.Placeholders})
}

// Verdicts of a live check.
const (
	Good       = "good"
	Bad        = "bad"
	Warn       = "warn"
	Unknown    = "unknown"
	NeedsInput = "needs-input" // a placeholder has no value yet
)

// Result is a live check's answer.
type Result struct {
	Verdict string   `json:"verdict"`
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
	// Missing are the placeholders the check still needs (NeedsInput).
	Missing []string `json:"missing,omitempty"`
	// Op is the agent op that ran.
	Op string    `json:"op"`
	At time.Time `json:"at"`
}

func (a *API) check(w http.ResponseWriter, r *http.Request) {
	s, ok := a.byID[r.PathValue("sid")]
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such scenario")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > len(s.Steps) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such step")
		return
	}
	step := s.Steps[n-1]
	if step.Check == nil {
		httpjson.Error(w, http.StatusNotFound, "not_found", "this step has no live check")
		return
	}
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	values := map[string]string{}
	for name := range scenarios.Placeholders {
		if v := r.URL.Query().Get(name); v != "" {
			// The same rules as for the commands the page shows (B1).
			if err := scenarios.ValidValue(name, v); err != nil {
				httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			values[name] = v
		}
	}
	req, err := step.Check.Request(values)
	var miss *scenarios.ErrMissing
	if errors.As(err, &miss) {
		httpjson.Write(w, http.StatusOK, Result{Verdict: NeedsInput, Op: step.Check.Op, Missing: miss.Names, At: time.Now().UTC(),
			Summary: "This check needs " + joinNames(miss.Names) + "."})
		return
	}
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if a.Reports != nil && (req.Op == protocol.OpNodePressure || req.Op == protocol.OpHealthCheck) {
		req.NodeMetrics = a.Reports(c.ID)
	}
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "narrow the check")
		return
	}
	res, err := interpret(req, resp.Object)
	if err != nil {
		httpjson.Error(w, http.StatusBadGateway, "agent_error", err.Error())
		return
	}
	res.Op, res.At = step.Check.Op, time.Now().UTC()
	httpjson.Write(w, http.StatusOK, res)
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		switch {
		case i == 0:
		case i == len(names)-1:
			out += " and "
		default:
			out += ", "
		}
		out += scenarios.Placeholders[n]
	}
	return out
}

// interpret turns an agent answer into a step verdict.
func interpret(req *protocol.Request, obj json.RawMessage) (Result, error) {
	decode := func(v any) error {
		if len(obj) == 0 || json.Unmarshal(obj, v) != nil {
			return fmt.Errorf("the cluster agent sent an unreadable %s answer", req.Op)
		}
		return nil
	}
	switch req.Op {
	case protocol.OpServiceCheck:
		var r protocol.ServiceCheckResult
		if err := decode(&r); err != nil {
			return Result{}, err
		}
		return serviceVerdict(r), nil
	case protocol.OpPodDebug:
		var d protocol.PodDebug
		if err := decode(&d); err != nil {
			return Result{}, err
		}
		return podDebugVerdict(d), nil
	case protocol.OpPodDiagnose:
		var d protocol.PodDiagnosis
		if err := decode(&d); err != nil {
			return Result{}, err
		}
		return diagnoseVerdict(d), nil
	case protocol.OpNetpolCheck:
		var n protocol.NetpolResult
		if err := decode(&n); err != nil {
			return Result{}, err
		}
		return netpolVerdict(n), nil
	case protocol.OpNodePressure:
		var p protocol.NodePressureReport
		if err := decode(&p); err != nil {
			return Result{}, err
		}
		return pressureVerdict(p), nil
	case protocol.OpHealthCheck:
		var h protocol.HealthReport
		if err := decode(&h); err != nil {
			return Result{}, err
		}
		return healthVerdict(h), nil
	case protocol.OpAppMap:
		var m protocol.AppMap
		if err := decode(&m); err != nil {
			return Result{}, err
		}
		return appMapVerdict(m), nil
	}
	return Result{}, fmt.Errorf("no verdict for op %s", req.Op)
}
