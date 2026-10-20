package yamlview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/resources"
	"kubemap/portal/store"
)

// requestTimeout bounds the get through the tunnel.
const requestTimeout = 20 * time.Second

// maxEdited bounds an edited object (the agent's objects are far smaller).
const maxEdited = 1 << 20

// API serves (behind the login):
//
//	GET  /api/clusters/{id}/yaml?group=&version=&resource=&namespace=&name=
//	     -> the object's YAML, redacted (View)
//	GET  /api/clusters/{id}/yaml/names?group=&version=&resource=&namespace=
//	     -> {names, truncated}: the object names of one kind, for the picker
//	POST /api/clusters/{id}/yaml/diff {group, version, resource, namespace, name, edited}
//	     -> the diff against the live object, the patch and the kubectl
//	        commands (Result); nothing is sent to the cluster but a get
type API struct {
	store store.Store
	agent agentcall.Requester
}

// NewAPI returns the YAML API.
func NewAPI(st store.Store, agent agentcall.Requester) *API { return &API{store: st, agent: agent} }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/yaml", require(http.HandlerFunc(a.view)))
		mux.Handle("GET /api/clusters/{id}/yaml/names", require(http.HandlerFunc(a.names)))
		mux.Handle("POST /api/clusters/{id}/yaml/diff", require(http.HandlerFunc(a.diff)))
	}
}

type target struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Edited    string `json:"edited,omitempty"`
}

// get asks the agent for the live object (the get op: from its cache, or
// read live for uncached kinds; Secrets are refused by the agent).
func (a *API) get(w http.ResponseWriter, r *http.Request, t target) ([]byte, bool) {
	// Any group and any spelling (review-km-44 M1, review-km-39 N-3).
	if strings.EqualFold(strings.TrimSpace(t.Resource), "secrets") {
		httpjson.Error(w, http.StatusForbidden, "secrets_never_shown", "Kubemap never reads Secrets, so it can't show or edit them (A4). Use kubectl with your own credentials.")
		return nil, false
	}
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return nil, false
	}
	req := &protocol.Request{Op: protocol.OpGet, Group: t.Group, Version: t.Version, Resource: t.Resource, Namespace: t.Namespace, Name: t.Name}
	if err := req.Validate(); err != nil {
		var pe *protocol.Error
		if errors.As(err, &pe) {
			agentcall.AgentError(w, pe, "")
			return nil, false
		}
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.Error(w, err, "")
		return nil, false
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "")
		return nil, false
	}
	return resp.Object, true
}

func (a *API) view(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t := target{Group: q.Get("group"), Version: q.Get("version"), Resource: q.Get("resource"), Namespace: q.Get("namespace"), Name: q.Get("name")}
	raw, ok := a.get(w, r, t)
	if !ok {
		return
	}
	v, _, err := NewView(raw)
	if err != nil {
		httpjson.Error(w, http.StatusBadGateway, "bad_object", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, v)
}

func (a *API) diff(w http.ResponseWriter, r *http.Request) {
	var t target
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEdited+4096)).Decode(&t); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "body: "+err.Error())
		return
	}
	if len(t.Edited) > maxEdited {
		httpjson.Error(w, http.StatusRequestEntityTooLarge, "too_large", "the edited object is bigger than 1 MiB")
		return
	}
	// Always against the live object now, not the one the page loaded.
	raw, ok := a.get(w, r, t)
	if !ok {
		return
	}
	res, err := Diff(Target{t.Group, t.Version, t.Resource, t.Namespace, t.Name}, raw, t.Edited)
	var ee *ErrEdit
	switch {
	case errors.As(err, &ee):
		httpjson.Error(w, http.StatusUnprocessableEntity, "bad_edit", ee.Msg)
		return
	case err != nil:
		httpjson.Error(w, http.StatusBadGateway, "bad_object", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, res)
}

func (a *API) names(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.EqualFold(strings.TrimSpace(q.Get("resource")), "secrets") {
		httpjson.Error(w, http.StatusForbidden, "secrets_never_shown", "Kubemap never reads Secrets.")
		return
	}
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	items, truncated, err := resources.ListAll(ctx, a.agent, c.ID, protocol.Request{
		Group: q.Get("group"), Version: q.Get("version"), Resource: q.Get("resource"), Namespace: q.Get("namespace"),
	})
	if err != nil {
		agentcall.Error(w, err, "")
		return
	}
	names := make([]string, 0, len(items))
	for _, raw := range items {
		var o struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &o) == nil && o.Metadata.Name != "" {
			names = append(names, o.Metadata.Name)
		}
	}
	sort.Strings(names)
	httpjson.Write(w, http.StatusOK, map[string]any{"names": names, "truncated": truncated})
}
