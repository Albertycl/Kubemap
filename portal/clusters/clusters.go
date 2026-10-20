// Package clusters serves the cluster list. The server runs inside the one
// cluster it manages, so there is no registration: the cluster record is
// created by the server at startup (store.EnsureCluster).
//
// Admin API (behind the login, JSON):
//
//	GET /api/clusters
//	GET /api/clusters/{id}
package clusters

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

// Config configures the API.
type Config struct {
	Store store.Store
	Log   *slog.Logger
}

// API serves the cluster list.
type API struct{ cfg Config }

// New returns the API.
func New(cfg Config) *API { return &API{cfg: cfg} }

// Routes registers the API. require wraps the routes (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters", require(http.HandlerFunc(a.list)))
		mux.Handle("GET /api/clusters/{id}", require(http.HandlerFunc(a.get)))
	}
}

// clusterView is the JSON form of a cluster.
type clusterView struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"createdAt"`
	LastSeen          *time.Time `json:"lastSeen,omitempty"`
	AgentVersion      string     `json:"agentVersion,omitempty"`
	KubernetesVersion string     `json:"kubernetesVersion,omitempty"`
	NodeCount         int        `json:"nodeCount"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func view(c store.Cluster) clusterView {
	return clusterView{
		ID: c.ID, Name: c.Name, Status: c.Status, CreatedAt: c.CreatedAt, LastSeen: timePtr(c.LastSeen),
		AgentVersion: c.AgentVersion, KubernetesVersion: c.KubernetesVersion, NodeCount: c.NodeCount,
	}
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	cs, err := a.cfg.Store.ListClusters(r.Context())
	if err != nil {
		a.internal(w, "list clusters", err)
		return
	}
	out := make([]clusterView, 0, len(cs))
	for _, c := range cs {
		out = append(out, view(c))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"clusters": out})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	c, err := a.cfg.Store.GetCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such cluster")
		return
	}
	if err != nil {
		a.internal(w, "get cluster", err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"cluster": view(c)})
}

func (a *API) internal(w http.ResponseWriter, what string, err error) {
	if a.cfg.Log != nil {
		a.cfg.Log.Error(what, "err", err)
	}
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}
