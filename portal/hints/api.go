package hints

import (
	"net/http"
	"slices"
	"strings"

	"kubemap/portal/httpjson"
)

// API serves GET /api/hints?kind=Pod&namespace=ns&name=p&status=CrashLoopBackOff&notReady=1&rule=r
// (also kind=Service|Deployment|StatefulSet|PersistentVolumeClaim|Node, missing=1,
// node=, container=, initContainer= for the pod row)
// (behind the login). It is stateless and read-only: the browser sends what
// it already shows, the answer is text.
type API struct{}

// Routes registers the API; require wraps every route.
func (API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/hints", require(http.HandlerFunc(handle)))
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s := Signals{
		Kind:          q.Get("kind"),
		Namespace:     q.Get("namespace"),
		Name:          q.Get("name"),
		Status:        q.Get("status"),
		NotReady:      q.Get("notReady") == "1",
		Missing:       q.Get("missing") == "1",
		Rules:         q["rule"],
		Node:          q.Get("node"),
		Container:     q.Get("container"),
		InitContainer: q.Get("initContainer"),
	}
	if !slices.Contains(Kinds, s.Kind) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "kind must be one of "+strings.Join(Kinds, ", "))
		return
	}
	// Every value that can end up in a command must be a Kubernetes name.
	optional := func(v string) bool { return v == "" || NameOK(v) }
	if !NameOK(s.Name) || (s.Kind != "Node" && !NameOK(s.Namespace)) || len(s.Rules) > 20 || len(s.Status) > 100 ||
		!optional(s.Node) || !optional(s.Container) || !optional(s.InitContainer) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "bad name, namespace, status, node, container or rule")
		return
	}
	hs := Match(s)
	if hs == nil {
		hs = []Hint{}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"hints": hs})
}
