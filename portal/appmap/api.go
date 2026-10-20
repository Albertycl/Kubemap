// Package appmap serves a cluster's application map (KM-53) to the web UI.
// The cluster agent does the analysis (apps, tiers, links and their
// evidence); the portal asks for it through the tunnel with a request of op
// "appmap", checks the answer's shape and hands it to the browser.
package appmap

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

// requestTimeout bounds one appmap request through the tunnel. The agent
// builds the map from its cache in milliseconds; the rest is the network.
const requestTimeout = 15 * time.Second

// Requester sends a request to a cluster agent (tunnel.Hub).
type Requester interface {
	Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error)
}

// API serves (behind the login):
//
//	GET /api/clusters/{id}/appmap[?namespace=ns][&app=appID]
//	GET /api/clusters/{id}/chain?id=Kind/ns/name   the whole chain of one object (KM-35)
//	GET /api/clusters/{id}/chain?q=text            objects whose ID contains the text
type API struct {
	store store.Store
	agent Requester
}

// NewAPI returns the app map API.
func NewAPI(st store.Store, agent Requester) *API {
	return &API{store: st, agent: agent}
}

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/appmap", require(http.HandlerFunc(a.get)))
		mux.Handle("GET /api/clusters/{id}/chain", require(http.HandlerFunc(a.chain)))
	}
}

// Response is what the browser gets: the agent's map plus when it was made.
type Response struct {
	protocol.AppMap
	At time.Time `json:"at"`
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	q := r.URL.Query()
	req := &protocol.Request{Op: protocol.OpAppMap, Namespace: q.Get("namespace")}
	if app := q.Get("app"); app != "" {
		req.AppMap = &protocol.AppMapOptions{App: app}
	}
	if err := req.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "choose one namespace")
		return
	}

	// Decode into the protocol types: the browser only ever sees the fields
	// the portal knows, whatever else an agent sends.
	var m protocol.AppMap
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &m) != nil {
		httpjson.Error(w, http.StatusBadGateway, "agent_error", "the cluster agent sent an unreadable app map")
		return
	}
	normalize(&m)
	httpjson.Write(w, http.StatusOK, Response{AppMap: m, At: time.Now().UTC()})
}

// normalize gives the UI empty lists instead of null, so it can iterate
// without checks.
func normalize(m *protocol.AppMap) {
	if m.Apps == nil {
		m.Apps = []protocol.MapApp{}
	}
	if m.Links == nil {
		m.Links = []protocol.MapLink{}
	}
	for i := range m.Apps {
		if m.Apps[i].Components == nil {
			m.Apps[i].Components = []protocol.MapComponent{}
		}
	}
	for i := range m.Links {
		if m.Links[i].Evidence == nil {
			m.Links[i].Evidence = []protocol.MapEvidence{}
		}
	}
}

// chain asks the agent for one object's chain (op object.chain), or for the
// objects matching a text.
func (a *API) chain(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	q := r.URL.Query()
	req := &protocol.Request{Op: protocol.OpChain, Chain: &protocol.ChainRequest{ID: q.Get("id"), Query: q.Get("q")}}
	if err := req.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "the chain is too large")
		return
	}
	var ch protocol.ObjectChain
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &ch) != nil {
		httpjson.Error(w, http.StatusBadGateway, "agent_error", "the cluster agent sent an unreadable chain")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"chain": ch, "at": time.Now().UTC()})
}
