package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/resources"
)

// ArgoApp is one Argo CD Application: where it comes from, where it goes,
// its sync and health, and every resource it manages.
type ArgoApp struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"` // where the Application object lives
	// MapID is the app's ID on the app map ("argocd/<name>").
	MapID         string       `json:"mapId"`
	Project       string       `json:"project"`
	Sources       []ArgoSource `json:"sources"`
	DestServer    string       `json:"destServer,omitempty"`
	DestNamespace string       `json:"destNamespace,omitempty"`
	AutoSync      bool         `json:"autoSync"`
	Prune         bool         `json:"prune"`
	SelfHeal      bool         `json:"selfHeal"`

	Sync          string `json:"sync"` // Synced, OutOfSync, Unknown
	Revision      string `json:"revision,omitempty"`
	Health        string `json:"health"` // Healthy, Progressing, Degraded, Suspended, Missing, Unknown
	HealthMessage string `json:"healthMessage,omitempty"`
	// Operation is the last sync: phase, message and when it finished.
	OperationPhase    string    `json:"operationPhase,omitempty"`
	OperationMessage  string    `json:"operationMessage,omitempty"`
	OperationFinished time.Time `json:"operationFinished,omitempty"`
	// OperationStarted, OperationBy (the user name Argo CD recorded, for
	// example "kubemap:admin" for a sync asked from Kubemap, KM-41) and
	// OperationRevision (the commit it synced) complete the last sync.
	OperationStarted  time.Time `json:"operationStarted,omitempty"`
	OperationBy       string    `json:"operationBy,omitempty"`
	OperationRevision string    `json:"operationRevision,omitempty"`
	// OperationRunning is true while Argo CD runs an operation: a new sync
	// would be refused until it finishes.
	OperationRunning bool            `json:"operationRunning"`
	ReconciledAt     time.Time       `json:"reconciledAt,omitempty"`
	Conditions       []ArgoCondition `json:"conditions,omitempty"`
	Resources        []ArgoResource  `json:"resources"`
}

type ArgoSource struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path,omitempty"`
	Chart          string `json:"chart,omitempty"`
	TargetRevision string `json:"targetRevision,omitempty"`
}

type ArgoCondition struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ArgoResource is one managed resource with its own sync and health.
type ArgoResource struct {
	Group           string `json:"group,omitempty"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	Status          string `json:"status,omitempty"` // Synced, OutOfSync
	Health          string `json:"health,omitempty"`
	HealthMessage   string `json:"healthMessage,omitempty"`
	Hook            bool   `json:"hook,omitempty"`
	RequiresPruning bool   `json:"requiresPruning,omitempty"`
}

// application is the part of an argoproj.io/v1alpha1 Application read here.
type application struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	// Operation is set while an operation is asked for or running.
	Operation json.RawMessage `json:"operation"`
	Spec      struct {
		Project     string       `json:"project"`
		Source      *ArgoSource  `json:"source"`
		Sources     []ArgoSource `json:"sources"`
		Destination struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
		SyncPolicy *struct {
			Automated *struct {
				Prune    bool `json:"prune"`
				SelfHeal bool `json:"selfHeal"`
			} `json:"automated"`
		} `json:"syncPolicy"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"health"`
		OperationState *struct {
			Phase      string    `json:"phase"`
			Message    string    `json:"message"`
			StartedAt  time.Time `json:"startedAt"`
			FinishedAt time.Time `json:"finishedAt"`
			Operation  struct {
				InitiatedBy struct {
					Username  string `json:"username"`
					Automated bool   `json:"automated"`
				} `json:"initiatedBy"`
			} `json:"operation"`
			SyncResult *struct {
				Revision string `json:"revision"`
			} `json:"syncResult"`
		} `json:"operationState"`
		ReconciledAt time.Time       `json:"reconciledAt"`
		Conditions   []ArgoCondition `json:"conditions"`
		Resources    []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			Health    *struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"health"`
			Hook            bool `json:"hook"`
			RequiresPruning bool `json:"requiresPruning"`
		} `json:"resources"`
	} `json:"status"`
}

// ArgoView reduces an Application object for the page.
func ArgoView(raw json.RawMessage) (ArgoApp, error) {
	var o application
	if err := json.Unmarshal(raw, &o); err != nil {
		return ArgoApp{}, err
	}
	s := o.Status
	app := ArgoApp{
		Name: o.Metadata.Name, Namespace: o.Metadata.Namespace, MapID: "argocd/" + o.Metadata.Name,
		Project: o.Spec.Project, Sources: o.Spec.Sources,
		DestServer: o.Spec.Destination.Server, DestNamespace: o.Spec.Destination.Namespace,
		Sync: orUnknown(s.Sync.Status), Revision: s.Sync.Revision,
		Health: orUnknown(s.Health.Status), HealthMessage: s.Health.Message,
		ReconciledAt: s.ReconciledAt, Conditions: s.Conditions, Resources: []ArgoResource{},
	}
	if app.DestServer == "" {
		app.DestServer = o.Spec.Destination.Name
	}
	if o.Spec.Source != nil {
		app.Sources = append([]ArgoSource{*o.Spec.Source}, app.Sources...)
	}
	if app.Sources == nil {
		app.Sources = []ArgoSource{}
	}
	if p := o.Spec.SyncPolicy; p != nil && p.Automated != nil {
		app.AutoSync, app.Prune, app.SelfHeal = true, p.Automated.Prune, p.Automated.SelfHeal
	}
	if op := s.OperationState; op != nil {
		app.OperationPhase, app.OperationMessage, app.OperationFinished = op.Phase, op.Message, op.FinishedAt
		app.OperationStarted = op.StartedAt
		app.OperationBy = op.Operation.InitiatedBy.Username
		if op.Operation.InitiatedBy.Automated {
			app.OperationBy = "automated sync"
		}
		if op.SyncResult != nil {
			app.OperationRevision = op.SyncResult.Revision
		}
		app.OperationRunning = op.Phase == "Running" || op.Phase == "Terminating"
	}
	if len(o.Operation) > 0 && string(o.Operation) != "null" {
		app.OperationRunning = true
		// Asked but not picked up yet: operationState still describes the
		// previous operation, so who asked comes from operation itself.
		var op struct {
			InitiatedBy struct {
				Username  string `json:"username"`
				Automated bool   `json:"automated"`
			} `json:"initiatedBy"`
		}
		if json.Unmarshal(o.Operation, &op) == nil {
			app.OperationBy = op.InitiatedBy.Username
			if op.InitiatedBy.Automated {
				app.OperationBy = "automated sync"
			}
		}
	}
	for _, r := range s.Resources {
		ar := ArgoResource{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Status: r.Status,
			Hook: r.Hook, RequiresPruning: r.RequiresPruning}
		if r.Health != nil {
			ar.Health, ar.HealthMessage = r.Health.Status, r.Health.Message
		}
		app.Resources = append(app.Resources, ar)
	}
	// Problems first: out of sync or not healthy, then by kind and name.
	sort.SliceStable(app.Resources, func(i, j int) bool {
		a, b := resourceRank(app.Resources[i]), resourceRank(app.Resources[j])
		if a != b {
			return a < b
		}
		if app.Resources[i].Kind != app.Resources[j].Kind {
			return app.Resources[i].Kind < app.Resources[j].Kind
		}
		return app.Resources[i].Name < app.Resources[j].Name
	})
	return app, nil
}

func resourceRank(r ArgoResource) int {
	switch {
	case r.Health == "Degraded" || r.Health == "Missing":
		return 0
	case r.Status == "OutOfSync" || (r.Health != "" && r.Health != "Healthy"):
		return 1
	}
	return 2
}

func appRank(a ArgoApp) int {
	switch {
	case a.Health == "Degraded" || a.Health == "Missing" || a.OperationPhase == "Failed" || a.OperationPhase == "Error":
		return 0
	case a.Sync != "Synced" || a.Health != "Healthy":
		return 1
	}
	return 2
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

func (a *API) argocd(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	items, truncated, err := resources.ListAll(ctx, a.agent, c.ID,
		protocol.Request{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"})
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Code == protocol.CodeNotFound {
		// No Application resource in this cluster: Argo CD is not installed.
		httpjson.Write(w, http.StatusOK, map[string]any{"installed": false, "apps": []ArgoApp{}, "at": time.Now().UTC()})
		return
	}
	if err != nil {
		agentcall.Error(w, err, "choose one namespace")
		return
	}
	apps := make([]ArgoApp, 0, len(items))
	bad := 0
	for _, raw := range items {
		app, err := ArgoView(raw)
		if err != nil {
			bad++
			continue
		}
		apps = append(apps, app)
	}
	sort.SliceStable(apps, func(i, j int) bool {
		if ri, rj := appRank(apps[i]), appRank(apps[j]); ri != rj {
			return ri < rj
		}
		return apps[i].Name < apps[j].Name
	})
	out := map[string]any{"installed": true, "apps": apps, "truncated": truncated, "at": time.Now().UTC()}
	if bad > 0 {
		out["unreadable"] = bad
	}
	httpjson.Write(w, http.StatusOK, out)
}
