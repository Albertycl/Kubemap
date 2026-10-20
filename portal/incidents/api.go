package incidents

import (
	"net/http"

	"kubemap/portal/httpjson"
)

// API serves the drafts (behind the login), read-only:
//
//	GET /api/incident-drafts          every draft, newest first, without evidence
//	GET /api/incident-drafts/{id}     one draft in full
//
// The incident journal (KM-89) reads the same drafts through its Sink and
// turns one into an incident when a person writes it up.
type API struct{ Sink Sink }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/incident-drafts", require(http.HandlerFunc(a.list)))
		mux.Handle("GET /api/incident-drafts/{id}", require(http.HandlerFunc(a.get)))
	}
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	ds, err := a.Sink.List(r.Context())
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for i := range ds {
		ds[i].Evidence = nil // the list stays small; open a draft for its evidence
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"drafts": ds})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	d, err := a.Sink.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if d == nil {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such draft")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"draft": d})
}
