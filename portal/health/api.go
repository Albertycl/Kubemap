// Package health serves the problem finder (KM-61, KM-62 UI): the findings of
// the cluster agents' built-in health rules (request op health.check), for
// one cluster or for every cluster at once. The rules run in the agents;
// the portal only asks and passes the findings on.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

// requestTimeout bounds one health.check. The agent reads each node's
// kubelet summary, which takes about 100 ms on the lab cluster.
const requestTimeout = 20 * time.Second

// parallel is how many clusters are asked at the same time.
const parallel = 4

// Requester sends a request to a cluster agent (tunnel.Hub).
type Requester interface {
	Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error)
}

// API serves (behind the login):
//
//	GET /api/health                       every cluster's findings
//	GET /api/clusters/{id}/health[?namespace=ns]
type API struct {
	store store.Store
	agent Requester
	// Reports gives a cluster's latest node agent reports
	// (metrics.Collector.Reports): the node rules judge PSI from them, as the
	// cluster agent makes no kubelet calls. Nil sends none.
	Reports func(clusterID string) []protocol.Metrics
}

// NewAPI returns the problem finder API.
func NewAPI(st store.Store, agent Requester) *API { return &API{store: st, agent: agent} }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/health", require(http.HandlerFunc(a.all)))
		mux.Handle("GET /api/clusters/{id}/health", require(http.HandlerFunc(a.one)))
	}
}

// ClusterHealth is one cluster's answer in GET /api/health: its report, or
// why there is none (agent offline, an agent too old for the op).
type ClusterHealth struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Status string                 `json:"status"` // the cluster's status: active, disconnected
	Report *protocol.HealthReport `json:"report,omitempty"`
	Error  *httpjson.ErrorBody    `json:"error,omitempty"`
}

// ask runs health.check on one cluster. It returns the report, or the
// HTTP status, code and message of the failure.
func (a *API) ask(ctx context.Context, clusterID, namespace string) (*protocol.HealthReport, int, string, string) {
	req := &protocol.Request{Op: protocol.OpHealthCheck, Namespace: namespace}
	if a.Reports != nil {
		req.NodeMetrics = a.Reports(clusterID)
	}
	if err := req.Validate(); err != nil {
		return nil, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, clusterID, req)
	if err != nil {
		status, code, msg := agentcall.TunnelStatus(err)
		return nil, status, code, msg
	}
	if resp.Error != nil {
		status, code, msg := agentcall.Status(resp.Error, "choose one namespace")
		return nil, status, code, msg
	}
	// Decode into the protocol types: the browser only sees known fields.
	var r protocol.HealthReport
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &r) != nil {
		return nil, http.StatusBadGateway, "agent_error", "the cluster agent sent an unreadable health report"
	}
	normalize(&r)
	return &r, 0, "", ""
}

// Check runs health.check on one cluster for other parts of the portal
// (the incident drafter, KM-90): the report, or an error.
func (a *API) Check(ctx context.Context, clusterID string) (*protocol.HealthReport, error) {
	rep, status, code, msg := a.ask(ctx, clusterID, "")
	if rep == nil {
		return nil, fmt.Errorf("health.check on %s: %d %s: %s", clusterID, status, code, msg)
	}
	return rep, nil
}

// Report runs health.check on one cluster, for callers outside this API
// (the incident journal's evidence). A failure is an *httpjson.Failure.
func (a *API) Report(ctx context.Context, clusterID, namespace string) (*protocol.HealthReport, error) {
	r, status, code, msg := a.ask(ctx, clusterID, namespace)
	if r == nil {
		return nil, &httpjson.Failure{Status: status, Code: code, Message: msg}
	}
	return r, nil
}

// normalize sorts the findings worst first (the agent does, but the page
// relies on it) and gives the UI empty lists instead of null.
func normalize(r *protocol.HealthReport) {
	if r.Findings == nil {
		r.Findings = []protocol.HealthFinding{}
	}
	if r.Rules == nil {
		r.Rules = []string{}
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return severityRank(r.Findings[i].Severity) < severityRank(r.Findings[j].Severity)
	})
}

func severityRank(s string) int {
	switch s {
	case protocol.SeverityCritical:
		return 0
	case protocol.SeverityWarning:
		return 1
	case protocol.SeverityInfo:
		return 2
	}
	return 3
}

func (a *API) one(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	rep, status, code, msg := a.ask(r.Context(), c.ID, r.URL.Query().Get("namespace"))
	if rep == nil {
		httpjson.Error(w, status, code, msg)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"report": rep, "at": time.Now().UTC()})
}

func (a *API) all(w http.ResponseWriter, r *http.Request) {
	clusters, err := a.store.ListClusters(r.Context())
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]ClusterHealth, len(clusters))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, c := range clusters {
		out[i] = ClusterHealth{ID: c.ID, Name: c.Name, Status: c.Status}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rep, _, code, msg := a.ask(r.Context(), id, "")
			if rep == nil {
				out[i].Error = &httpjson.ErrorBody{Error: code, Message: msg}
				return
			}
			out[i].Report = rep
		}(i, c.ID)
	}
	wg.Wait()
	httpjson.Write(w, http.StatusOK, map[string]any{"clusters": out, "at": time.Now().UTC()})
}
