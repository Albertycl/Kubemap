// Package alerts serves the portal's alerts (KM-31): which clusters have a
// critical finding now. It reads the draft incidents the drafter keeps up
// to date (KM-90, one health.check per cluster per minute), so an alert
// appears within about a minute of a finding turning critical, and clears
// when the finding does. Namespaces the drafter ignores (the practice labs
// in kubemap-demo) raise no alert.
package alerts

import (
	"net/http"
	"sort"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/httpjson"
	"kubemap/portal/incidents"
)

// API serves (behind the login):
//
//	GET /api/alerts    every cluster with a critical finding now
type API struct{ Sink incidents.Sink }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/alerts", require(http.HandlerFunc(a.list)))
	}
}

// Alert is one cluster's critical findings now.
type Alert struct {
	ClusterID   string `json:"clusterId"`
	ClusterName string `json:"clusterName"`
	// Since is when the episode started (the draft's start).
	Since    time.Time `json:"since"`
	DraftID  string    `json:"draftId"`
	Findings []Finding `json:"findings"`
	// Key changes when the set of critical findings changes, so a banner
	// the user closed comes back for a new problem.
	Key string `json:"key"`
}

// Finding is one active critical finding.
type Finding struct {
	Rule   string `json:"rule"`
	Object string `json:"object"`
	Title  string `json:"title"`
}

// Alerts turns drafts into alerts: per cluster, the newest open draft's
// active critical findings.
func Alerts(drafts []incidents.Draft) []Alert {
	seen := map[string]bool{}
	var out []Alert
	sort.SliceStable(drafts, func(i, j int) bool { return drafts[i].Started.After(drafts[j].Started) })
	for _, d := range drafts {
		// Only a cluster's newest draft counts: an older one left open is stale.
		if seen[d.ClusterID] {
			continue
		}
		seen[d.ClusterID] = true
		if d.Status != incidents.StatusOpen {
			continue
		}
		al := Alert{ClusterID: d.ClusterID, ClusterName: d.ClusterName, Since: d.Started, DraftID: d.ID, Findings: []Finding{}}
		keys := make([]string, 0, len(d.Active))
		for k := range d.Active {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := d.Active[k]
			if f.Severity != protocol.SeverityCritical {
				continue
			}
			al.Findings = append(al.Findings, Finding{Rule: f.Rule, Object: f.Object, Title: f.Title})
			al.Key += k + ";"
		}
		if len(al.Findings) > 0 {
			al.Key = d.ID + "|" + al.Key
			out = append(out, al)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterName < out[j].ClusterName })
	return out
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	ds, err := a.Sink.List(r.Context())
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := Alerts(ds)
	if out == nil {
		out = []Alert{}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"alerts": out, "at": time.Now().UTC()})
}
