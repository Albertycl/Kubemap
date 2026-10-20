package protocol

import "time"

// The appmap op (KM-53) returns the application map ready to draw: each app
// with its components in tiers (entry → frontend → backend → database /
// cache), the pods behind each component with their health, and the links
// between components with the evidence for each link. The agent does the
// analysis (KM-32 graph, KM-49 apps, KM-50 dependencies, KM-51 routes,
// KM-52 data stores); the portal only lays it out. These types are the
// response object of an appmap request.

// AppMapOptions narrow an appmap request. Request.Namespace narrows it to the
// apps whose main namespace is that one.
type AppMapOptions struct {
	// App is an app ID ("argocd/flask-vue", "sre-lab-app/redis"): only that
	// app, the links that touch it and the components at their other end.
	App string `json:"app,omitempty"`
}

// maxAppIDLen bounds AppMapOptions.App ("namespace/name" or "argocd/name").
const maxAppIDLen = 512

// Tier is the column a component is drawn in.
type Tier string

const (
	TierEntry    Tier = "entry"    // Ingress or IngressRoute: host and path
	TierFrontend Tier = "frontend" // serves users, calls backends
	TierBackend  Tier = "backend"  // APIs and workers; the default
	TierDatabase Tier = "database" // databases, poolers, queues and search engines
	TierCache    Tier = "cache"    // Redis, Memcached and similar
)

// Tiers lists the tiers in drawing order, left to right.
var Tiers = []Tier{TierEntry, TierFrontend, TierBackend, TierDatabase, TierCache}

// EvidenceType says what kind of configuration proves a link.
type EvidenceType string

const (
	EvidenceRoute     EvidenceType = "route"     // an Ingress or IngressRoute rule sends traffic to the Service
	EvidenceEnv       EvidenceType = "env"       // a literal env var in the pod template names the Service
	EvidenceConfigMap EvidenceType = "configmap" // an env var whose value comes from a ConfigMap key names it
	EvidenceFile      EvidenceType = "file"      // a ConfigMap mounted as a file names it
	EvidenceOperator  EvidenceType = "operator"  // an operator resource says so (CNPG Pooler → Cluster)
)

// AppMap is the response object of an appmap request.
type AppMap struct {
	Apps  []MapApp  `json:"apps"`
	Links []MapLink `json:"links"`
	// Peers are components outside Apps that a link points to, for example
	// the OTel collector of another namespace. They carry no pods.
	Peers []MapComponent `json:"peers,omitempty"`
	// Unassigned are components no app owns: an Ingress whose backend is
	// gone, a Service nothing runs behind.
	Unassigned []MapComponent `json:"unassigned,omitempty"`
	// Incomplete lists resources the agent could not read (for example
	// "forbidden"), so the map may lack their parts.
	Incomplete []string `json:"incomplete,omitempty"`
	// Traces says what the tracing backend showed (KM-54); nil when the
	// agent has no Tempo to ask.
	Traces *MapTraceSummary `json:"traces,omitempty"`
}

// MapTraceSummary is what the agent learned from traces for this map
// (KM-54). Configuration finds the links; traces confirm that calls
// really happen.
type MapTraceSummary struct {
	Source string `json:"source"`           // "tempo http://tempo.observability.svc:3200"
	Window string `json:"window"`           // how far back, "1h"
	Spans  int    `json:"spans"`            // spans looked at
	Probes int    `json:"probes,omitempty"` // kubelet health probes, ignored
	// Confirmed counts the links with Traces set.
	Confirmed int `json:"confirmed"`
	// Unmatched are calls the traces show that no link on the map
	// explains: a dependency the configuration hides, or a wrong map.
	Unmatched []string `json:"unmatched,omitempty"`
	// Error is set when the tracing backend could not be read; the map
	// is then shown without confirmations.
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// MapTraces confirms one link from traces (KM-54).
type MapTraces struct {
	Spans    int       `json:"spans"`    // spans that show this call
	LastSeen time.Time `json:"lastSeen"` // the newest of them
	// Detail is one line for the tooltip: "12 server spans in
	// flask-vue-backend for Host flask-vue.albertycliu.xyz".
	Detail string `json:"detail"`
}

// MapApp is one application (KM-49).
type MapApp struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"` // argocd, helm, label or workload
	// Health is "Healthy", "Degraded", "Missing" or, for Argo CD apps,
	// Argo's health and sync status ("Healthy, OutOfSync").
	Health string `json:"health"`
	// State is Health for colour: ok, warn (OutOfSync, Progressing,
	// Missing references) or fail (Degraded).
	State    AppState     `json:"state"`
	Problems []MapProblem `json:"problems,omitempty"`
	System   bool         `json:"system,omitempty"`
	// Components are sorted by tier (in Tiers order), then by ID.
	Components []MapComponent `json:"components"`
}

// AppState is an app's health for colour.
type AppState string

const (
	StateOK   AppState = "ok"
	StateWarn AppState = "warn"
	StateFail AppState = "fail"
)

// MapProblem is one concrete problem of an app, linked to its object.
type MapProblem struct {
	ID     string `json:"id"`     // graph node ID, "Pod/shop/orders-1"
	Status string `json:"status"` // "CrashLoopBackOff", "0/1 ready", "missing"
}

// MapComponent is one box on the map: an entry point, a workload, an
// operator-managed data store, or a Service with nothing behind it.
type MapComponent struct {
	ID        string `json:"id"` // graph node ID, "Deployment/sre-lab-app/url-shortener"
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	App       string `json:"app,omitempty"` // app ID; empty when unassigned
	Tier      Tier   `json:"tier"`
	// TierReason says why the component is in its tier, for the UI's
	// tooltip: "redis image", "label app.kubernetes.io/component=frontend",
	// "receives traffic from an entry and calls a data store".
	TierReason string `json:"tierReason"`
	// Status is the short health text ("2/3 ready", "Cluster in healthy
	// state", "no pods match selector"); Healthy is the verdict for colour.
	Status  string `json:"status,omitempty"`
	Healthy bool   `json:"healthy"`
	// Missing is true when something refers to this object but it does not
	// exist (a deleted Service, a typo).
	Missing bool `json:"missing,omitempty"`
	// Data stores only.
	Engine  string `json:"engine,omitempty"` // postgres, pgbouncer, redis ...
	Version string `json:"version,omitempty"`
	// Entry points only: the routing rules of this Ingress or IngressRoute.
	Routes []MapRoute `json:"routes,omitempty"`
	// Services that select this component's pods.
	Services []string `json:"services,omitempty"`
	Pods     []MapPod `json:"pods,omitempty"`
}

// MapRoute is one routing rule of an entry point (KM-51).
type MapRoute struct {
	Host        string   `json:"host"` // "" matches any host
	Path        string   `json:"path"` // "" matches every path
	TLS         bool     `json:"tls"`
	EntryPoints []string `json:"entryPoints,omitempty"`
	Service     string   `json:"service"` // "Service/ns/name"
	Port        string   `json:"port,omitempty"`
	// ReadyEndpoints behind the Service; -1 when unknown.
	ReadyEndpoints int `json:"readyEndpoints"`
	// Problems found on this rule: "service missing", "no ready endpoints
	// (requests get 503)", a conflict, plain HTTP without a redirect.
	Problems []string `json:"problems,omitempty"`
}

// MapPod is one pod of a component, with the health an SRE looks at first.
type MapPod struct {
	Name string `json:"name"`
	Node string `json:"node,omitempty"`
	// Status is what "kubectl get pods" shows: "Running",
	// "CrashLoopBackOff (OOMKilled)", "Pending (Unschedulable)".
	Status   string `json:"status"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Primary  bool   `json:"primary,omitempty"` // CNPG: the current primary instance
}

// MapLink is one arrow between two components: an entry routing to a
// workload, a workload naming a Service in its configuration, or an
// operator resource pointing at another.
type MapLink struct {
	From    string `json:"from"` // component ID
	To      string `json:"to"`   // component ID
	FromApp string `json:"fromApp,omitempty"`
	ToApp   string `json:"toApp,omitempty"`
	// Via are the Services the link goes through ("Service/ns/name").
	Via []string `json:"via,omitempty"`
	// Missing is true when the link is broken: the named Service does not
	// exist or no pod runs behind it.
	Missing  bool          `json:"missing,omitempty"`
	Evidence []MapEvidence `json:"evidence"`
	// Traces is set when traces show this call really happens (KM-54).
	Traces *MapTraces `json:"traces,omitempty"`
}

// MapEvidence is one piece of configuration that proves a link.
type MapEvidence struct {
	Type EvidenceType `json:"type"`
	// Object holds the evidence: the Ingress or IngressRoute, the workload
	// (env), the ConfigMap (configmap, file) or the operator resource.
	Object string `json:"object"`
	// Name is the route's host and path, the env var name, or the file
	// name; Key the ConfigMap key when it differs from Name.
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
	Port string `json:"port,omitempty"`
	// Detail is a one-line human summary, for tooltips.
	Detail string `json:"detail"`
}
