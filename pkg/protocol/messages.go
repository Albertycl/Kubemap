package protocol

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Role says which kind of agent is registering. Only node agents register
// over the network: the cluster agent runs inside the server process.
type Role string

const (
	RoleNode Role = "node" // node agent (DaemonSet), one per node
)

// Register is the first message a node agent sends after the WebSocket
// opens. Direction: agent → server. The server answers with register.ack
// (same id) or with an error (code unauthorized) and closes the connection.
//
// A node agent sends ClusterID, NodeName and ServiceAccountToken: a projected
// ServiceAccount token with audience "kubemap-portal" that the server checks
// with a TokenReview.
type Register struct {
	Role         Role   `json:"role"`
	AgentVersion string `json:"agentVersion"`

	// ClusterID is the server's ID for the cluster.
	ClusterID string `json:"clusterId"`

	NodeName            string `json:"nodeName"`
	ServiceAccountToken string `json:"serviceAccountToken"`
}

func (*Register) MessageType() Type { return TypeRegister }

func (r *Register) Validate() error {
	if r.AgentVersion == "" {
		return errorf(CodeBadRequest, "register: missing agentVersion")
	}
	if r.Role != RoleNode {
		return errorf(CodeBadRequest, "register: unknown role %q; only node agents register", r.Role)
	}
	if r.ClusterID == "" || r.NodeName == "" || r.ServiceAccountToken == "" {
		return errorf(CodeBadRequest, "register: node agent needs clusterId, nodeName and serviceAccountToken")
	}
	return nil
}

// LogValue keeps the token out of logs.
func (r Register) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("role", string(r.Role)),
		slog.String("agentVersion", r.AgentVersion),
		slog.String("clusterId", r.ClusterID),
		slog.String("nodeName", r.NodeName),
		slog.String("serviceAccountToken", redact(r.ServiceAccountToken)),
	)
}

// RegistrationStatus is the server's state for a registering node agent.
type RegistrationStatus string

// StatusActive: the node agent is accepted and may send data.
const StatusActive RegistrationStatus = "active"

// RegisterAck answers register. Direction: server → agent. Its id is the id
// of the register message.
type RegisterAck struct {
	ClusterID string             `json:"clusterId"`
	Status    RegistrationStatus `json:"status"`
}

func (*RegisterAck) MessageType() Type { return TypeRegisterAck }

func (a *RegisterAck) Validate() error {
	if a.ClusterID == "" {
		return errorf(CodeBadRequest, "register.ack: missing clusterId")
	}
	if a.Status != StatusActive {
		return errorf(CodeBadRequest, "register.ack: unknown status %q", a.Status)
	}
	return nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// Heartbeat is sent by both sides every HeartbeatInterval. A side that has
// not received one for DisconnectAfter counts the peer as Disconnected.
type Heartbeat struct {
	Time time.Time `json:"time"`
}

func (*Heartbeat) MessageType() Type { return TypeHeartbeat }

func (h *Heartbeat) Validate() error {
	if h.Time.IsZero() {
		return errorf(CodeBadRequest, "heartbeat: missing time")
	}
	return nil
}

// Summary is the small cluster state the cluster agent pushes every few
// seconds. Direction: cluster agent → portal. Details are fetched on demand
// with request.
type Summary struct {
	Time              time.Time  `json:"time"`
	KubernetesVersion string     `json:"kubernetesVersion"`
	Nodes             NodeCounts `json:"nodes"`
	// NodeList has Kubernetes Ready per node, at most MaxSummaryNodes, so the
	// portal can tell "node down" from "node agent down".
	NodeList   []NodeStatus   `json:"nodeList,omitempty"`
	Namespaces int            `json:"namespaces"`
	Pods       PodCounts      `json:"pods"`
	Workloads  WorkloadCounts `json:"workloads"`
	// Problems lists objects in a bad state, worst first, at most 50.
	Problems []Problem `json:"problems,omitempty"`
}

// MaxProblems is the most problems one summary carries.
const MaxProblems = 50

// MaxSummaryNodes is the most nodes one summary lists by name.
const MaxSummaryNodes = 500

// NodeStatus is one node's Kubernetes Ready condition.
type NodeStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

type NodeCounts struct {
	Total int `json:"total"`
	Ready int `json:"ready"`
}

type PodCounts struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Unknown   int `json:"unknown"`
}

type WorkloadCounts struct {
	Deployments         int `json:"deployments"`
	DeploymentsDegraded int `json:"deploymentsDegraded"`
	StatefulSets        int `json:"statefulSets"`
	DaemonSets          int `json:"daemonSets"`
}

// Problem is one object in a bad state, such as a pod in CrashLoopBackOff.
type Problem struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Reason    string    `json:"reason"` // e.g. CrashLoopBackOff, OOMKilled, Unschedulable
	Message   string    `json:"message,omitempty"`
	Since     time.Time `json:"since,omitzero"`
}

func (*Summary) MessageType() Type { return TypeSummary }

func (s *Summary) Validate() error {
	if s.Time.IsZero() {
		return errorf(CodeBadRequest, "summary: missing time")
	}
	if len(s.NodeList) > MaxSummaryNodes {
		return errorf(CodeBadRequest, "summary: %d nodes, limit %d", len(s.NodeList), MaxSummaryNodes)
	}
	if len(s.Problems) > MaxProblems {
		return errorf(CodeBadRequest, "summary: %d problems, limit %d", len(s.Problems), MaxProblems)
	}
	for _, p := range s.Problems {
		if p.Kind == "" || p.Name == "" || p.Reason == "" {
			return errorf(CodeBadRequest, "summary: problem needs kind, name and reason")
		}
	}
	return nil
}

// Metrics is one node's health sample. Direction: node agent → portal, on
// the node agent's own connection. Byte values are bytes; percentages are 0-100.
type Metrics struct {
	Time     time.Time `json:"time"`
	NodeName string    `json:"nodeName"`

	CPU    CPUStats    `json:"cpu"`
	Memory MemoryStats `json:"memory"`
	Swap   SwapStats   `json:"swap"`
	Load   LoadStats   `json:"load"`
	// PSI is kernel pressure stall information; nil if the kernel lacks it.
	PSI         *PSIStats         `json:"psi,omitempty"`
	Filesystems []FilesystemStats `json:"filesystems,omitempty"`
	Network     []NetworkStats    `json:"network,omitempty"`
	// TopProcesses lists the busiest host processes, at most MaxTopProcesses.
	TopProcesses []ProcessStats `json:"topProcesses,omitempty"`
}

// MaxTopProcesses is the most processes one metrics message carries.
const MaxTopProcesses = 20

type CPUStats struct {
	Cores        int     `json:"cores"`
	UsagePercent float64 `json:"usagePercent"`
}

type MemoryStats struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
}

type SwapStats struct {
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
}

type LoadStats struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// PSIStats holds /proc/pressure/{cpu,memory,io}.
type PSIStats struct {
	CPU    Pressure `json:"cpu"`
	Memory Pressure `json:"memory"`
	IO     Pressure `json:"io"`
}

// Pressure is one /proc/pressure file. "some" is the share of time at least
// one task waited; "full" is the share of time all non-idle tasks waited.
// Values are percentages averaged over 10, 60 and 300 seconds.
type Pressure struct {
	Some PressureAvg  `json:"some"`
	Full *PressureAvg `json:"full,omitempty"` // absent for cpu on older kernels
}

type PressureAvg struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
}

// FilesystemStats is one filesystem, from statfs. UsedBytes is total minus
// free; AvailableBytes excludes the blocks reserved for root, so
// UsedBytes+AvailableBytes can be less than TotalBytes.
type FilesystemStats struct {
	MountPoint     string `json:"mountPoint"`
	Device         string `json:"device"`
	FSType         string `json:"fsType"`
	TotalBytes     uint64 `json:"totalBytes"`
	UsedBytes      uint64 `json:"usedBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	InodesTotal    uint64 `json:"inodesTotal"`
	InodesFree     uint64 `json:"inodesFree"`
}

type NetworkStats struct {
	Interface string `json:"interface"`
	RxBytes   uint64 `json:"rxBytes"`
	TxBytes   uint64 `json:"txBytes"`
	RxErrors  uint64 `json:"rxErrors"`
	TxErrors  uint64 `json:"txErrors"`
}

// ProcessStats is one host process, found through the host PID namespace.
// Command is the kernel's short name (/proc/<pid>/comm, at most 15 bytes);
// the command line is never sent, because it can hold passwords.
//
// CPUPercent is the process's share of the whole node's CPU time since the
// previous sample (0-100, like cpu.usagePercent), not a share of one core.
//
// Origin says where the process runs, from its cgroup. For OriginPod the
// node agent sets PodUID and ContainerID; PodNamespace, PodName and
// Container are filled in by whoever can map the UID (the portal, from the
// cluster agent's cache), because the node agent has no Kubernetes API access.
type ProcessStats struct {
	PID          int           `json:"pid"`
	Command      string        `json:"command"`
	CPUPercent   float64       `json:"cpuPercent"`
	RSSBytes     uint64        `json:"rssBytes"`
	Origin       ProcessOrigin `json:"origin"`
	ContainerID  string        `json:"containerId,omitempty"` // pod or non-Kubernetes container
	Unit         string        `json:"unit,omitempty"`        // systemd unit, for OriginService
	PodUID       string        `json:"podUid,omitempty"`
	PodNamespace string        `json:"podNamespace,omitempty"`
	PodName      string        `json:"podName,omitempty"`
	Container    string        `json:"container,omitempty"`
}

// ProcessOrigin says where a process runs.
type ProcessOrigin string

const (
	OriginPod       ProcessOrigin = "pod"       // a Kubernetes pod container
	OriginContainer ProcessOrigin = "container" // a container outside Kubernetes (Docker, Podman)
	OriginService   ProcessOrigin = "service"   // a systemd service such as k3s.service
	OriginUser      ProcessOrigin = "user"      // a login session
	OriginKernel    ProcessOrigin = "kernel"    // a kernel thread such as kswapd0
	OriginHost      ProcessOrigin = "host"      // anything else on the host
)

func (*Metrics) MessageType() Type { return TypeMetrics }

func (m *Metrics) Validate() error {
	if m.Time.IsZero() || m.NodeName == "" {
		return errorf(CodeBadRequest, "metrics: missing time or nodeName")
	}
	if len(m.TopProcesses) > MaxTopProcesses {
		return errorf(CodeBadRequest, "metrics: %d processes, limit %d", len(m.TopProcesses), MaxTopProcesses)
	}
	return nil
}

// Op is a request operation.
type Op string

const (
	OpList Op = "list" // list objects of one resource
	OpGet  Op = "get"  // get one object
	OpLogs Op = "logs" // read a finished chunk of pod logs; use stream.open to follow
	// OpTokenReview asks the cluster agent to check a node agent's
	// ServiceAccount token with the Kubernetes TokenReview API.
	OpTokenReview Op = "tokenreview"
	// OpGraph asks for the application map: apps, links between them,
	// routes, data stores and the relationship graph, for the whole cluster
	// or one namespace (Namespace), or around one object (Graph.Around).
	OpGraph Op = "graph"
	// OpAppMap asks for the application map ready to draw (KM-53): apps
	// with their components in tiers, pod health, and the links between
	// components with their evidence (see AppMap). Narrowed by Namespace or
	// AppMap.App.
	OpAppMap Op = "appmap"
	// OpNetpolCheck asks whether NetworkPolicies allow a connection, and
	// which policy decides (KM-33). See NetpolCheckRequest.
	OpNetpolCheck Op = "netpol.check"
	// OpPodDiagnose collects what explains a crashing pod (KM-56):
	// Namespace and Name name the pod. See PodDiagnosis.
	OpPodDiagnose Op = "pod.diagnose"
	// OpNodePressure combines kubelet node conditions with kernel pressure
	// stall information (KM-60): all nodes, or the one in Name. See
	// NodePressureReport.
	OpNodePressure Op = "node.pressure"
	// OpServiceCheck checks Services for missing or unready endpoints and
	// ports that lead nowhere (KM-77). Namespace and Name narrow it.
	OpServiceCheck Op = "service.check"
	// OpPodDebug walks the SRE checklist for one pod (KM-76): Namespace
	// and Name name the pod. See PodDebug.
	OpPodDebug Op = "pod.debug"
	// OpHealthCheck runs the built-in health rules (KM-61, KM-62);
	// Namespace optionally limits the pod rules. See HealthReport.
	OpHealthCheck Op = "health.check"
	// OpPodUsage returns per-pod and per-container usage from the metrics
	// API (pods.metrics.k8s.io; no kubelet call), with requests and limits
	// (KM-28). See PodUsageReport.
	OpPodUsage Op = "pod.usage"
	// OpRBACWhoCan answers RBAC questions from the cached RBAC objects
	// (KM-34): a subject's binding chain, a can-i check, or who can do an
	// action. See RBACQuery.
	OpRBACWhoCan Op = "rbac.whocan"
	// OpLogSearch searches the logs of every pod of an app by text, level
	// and time, with stack traces joined to their line (KM-57). See
	// LogSearchRequest and LogSearchResult.
	OpLogSearch Op = "logs.search"
	// OpChain returns the whole chain of one object, or finds objects by
	// name (KM-35). See ChainRequest and ObjectChain.
	OpChain Op = "object.chain"
)

// GraphOptions narrow a graph request.
type GraphOptions struct {
	// Around is a graph node ID ("Deployment/shop/web"); only objects
	// within Depth edges of it are returned.
	Around string `json:"around,omitempty"`
	Depth  int    `json:"depth,omitempty"` // 1..MaxGraphDepth, default 2
}

// MaxGraphDepth bounds GraphOptions.Depth.
const MaxGraphDepth = 5

// Request asks the cluster agent for data. Direction: portal → cluster agent.
// The agent answers with exactly one response with the same id.
type Request struct {
	Op Op `json:"op"`

	// list and get: the resource, as in the Kubernetes API path. Group is
	// empty for the core group; Resource is the lowercase plural ("pods").
	Group    string `json:"group,omitempty"`
	Version  string `json:"version,omitempty"`
	Resource string `json:"resource,omitempty"`

	// list, get and logs. Namespace is empty for cluster-scoped objects.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`

	// list only.
	LabelSelector string `json:"labelSelector,omitempty"`
	FieldSelector string `json:"fieldSelector,omitempty"`
	Limit         int64  `json:"limit,omitempty"`
	Continue      string `json:"continue,omitempty"`

	// logs only: Name is the pod.
	Logs *LogOptions `json:"logs,omitempty"`

	// tokenreview only.
	TokenReview *TokenReviewRequest `json:"tokenReview,omitempty"`

	// graph only (optional).
	Graph *GraphOptions `json:"graph,omitempty"`

	// appmap only (optional).
	AppMap *AppMapOptions `json:"appmap,omitempty"`

	// netpol.check only.
	NetpolCheck *NetpolCheckRequest `json:"netpolCheck,omitempty"`

	// node.pressure only: the latest metrics message of each node agent,
	// as the portal received it (KM-60). The cluster agent makes no kubelet
	// calls; node pressure comes from these reports and the node
	// conditions in its cache. TopProcesses and Network are ignored and
	// should be left out.
	NodeMetrics []Metrics `json:"nodeMetrics,omitempty"`

	// rbac.whocan only.
	RBAC *RBACQuery `json:"rbac,omitempty"`

	// logs.search only.
	LogSearch *LogSearchRequest `json:"logSearch,omitempty"`

	// object.chain only.
	Chain *ChainRequest `json:"chain,omitempty"`
}

// LogOptions select pod log lines, like kubectl logs.
type LogOptions struct {
	Container    string `json:"container,omitempty"`
	TailLines    int64  `json:"tailLines,omitempty"`
	SinceSeconds int64  `json:"sinceSeconds,omitempty"`
	Previous     bool   `json:"previous,omitempty"`
	Timestamps   bool   `json:"timestamps,omitempty"`
}

// TokenReviewRequest carries the token a node agent presented in register.
type TokenReviewRequest struct {
	Token    string `json:"token"`
	Audience string `json:"audience"` // must be NodeTokenAudience
}

// Node agent identity (A8). The install manifests (KM-22, KM-63) and the
// portal's check (KM-18) both use these, so they cannot drift apart.
const (
	// NodeTokenAudience is the audience of node agents' projected tokens.
	NodeTokenAudience       = "kubemap-portal"
	NodeAgentNamespace      = "kubemap-node"
	NodeAgentServiceAccount = "kubemap-node-agent"
	NodeAgentUsername       = "system:serviceaccount:" + NodeAgentNamespace + ":" + NodeAgentServiceAccount
)

// LogValue keeps the token out of logs.
func (t TokenReviewRequest) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", redact(t.Token)), slog.String("audience", t.Audience))
}

func (*Request) MessageType() Type { return TypeRequest }

// LogValue keeps a tokenreview token out of logs. slog does not look inside
// nested values, so Request needs its own LogValue.
func (r Request) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("op", string(r.Op)),
		slog.String("resource", r.Resource),
		slog.String("namespace", r.Namespace),
		slog.String("name", r.Name),
	}
	if r.TokenReview != nil {
		attrs = append(attrs, slog.Any("tokenReview", *r.TokenReview))
	}
	return slog.GroupValue(attrs...)
}

// validName is a loose check for namespaces and object names: they end up in
// Kubernetes API paths, so a "/" must never get through.
func validName(s string) bool {
	return len(s) <= 253 && !strings.ContainsAny(s, "/ \t\n") && s != "." && s != ".."
}

// Group, version and resource also end up in API paths
// (/apis/<group>/<version>/…/<resource>). They must have Kubernetes' own
// shapes, so no ".." or "/" can turn the path into another one
// (review-km-44 M1: version "../../api/v1" read the agent's credential
// Secret through the dynamic client).
var (
	versionPattern  = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)
	groupPattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	resourcePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// validGVR checks the group, version and resource a request names (each
// may be empty; the op decides which are required).
func validGVR(group, version, resource string) bool {
	return (group == "" || (len(group) <= 253 && groupPattern.MatchString(group))) &&
		(version == "" || versionPattern.MatchString(version)) &&
		(resource == "" || (len(resource) <= 63 && resourcePattern.MatchString(resource)))
}

func (r *Request) Validate() error {
	if r.Limit < 0 || r.Limit > MaxListLimit {
		return errorf(CodeBadRequest, "request: limit must be 0..%d", MaxListLimit)
	}
	if r.Logs != nil && (r.Logs.TailLines < 0 || r.Logs.SinceSeconds < 0) {
		return errorf(CodeBadRequest, "request: negative tailLines or sinceSeconds")
	}
	if !validName(r.Namespace) || !validName(r.Name) || !validGVR(r.Group, r.Version, r.Resource) {
		return errorf(CodeBadRequest, "request: invalid group, version, resource, namespace or name")
	}
	// Defence in depth for A4: the agent's RBAC cannot read Secrets anyway
	// (except its own credential Secret, by name). Any group: a Secret is
	// never served, whatever path names it.
	if r.Resource == "secrets" {
		return errorf(CodeForbidden, "request: secrets are never served")
	}
	if len(r.NodeMetrics) > 0 && r.Op != OpNodePressure && r.Op != OpHealthCheck {
		return errorf(CodeBadRequest, "request %s: nodeMetrics is for node.pressure and health.check only", r.Op)
	}
	switch r.Op {
	case OpList:
		if r.Version == "" || r.Resource == "" {
			return errorf(CodeBadRequest, "request list: needs version and resource")
		}
	case OpGet:
		if r.Version == "" || r.Resource == "" || r.Name == "" {
			return errorf(CodeBadRequest, "request get: needs version, resource and name")
		}
	case OpLogs:
		if r.Namespace == "" || r.Name == "" {
			return errorf(CodeBadRequest, "request logs: needs namespace and name")
		}
	case OpTokenReview:
		if r.TokenReview == nil || r.TokenReview.Token == "" {
			return errorf(CodeBadRequest, "request tokenreview: needs a token")
		}
		// No other audience is legal: the portal must not become a
		// TokenReview oracle for other audiences.
		if r.TokenReview.Audience != NodeTokenAudience {
			return errorf(CodeBadRequest, "request tokenreview: audience must be %q", NodeTokenAudience)
		}
	case OpGraph:
		if g := r.Graph; g != nil && (g.Depth < 0 || g.Depth > MaxGraphDepth || len(g.Around) > 512) {
			return errorf(CodeBadRequest, "request graph: depth must be 0..%d", MaxGraphDepth)
		}
	case OpAppMap:
		if a := r.AppMap; a != nil && (len(a.App) > maxAppIDLen || strings.ContainsAny(a.App, " \t\n")) {
			return errorf(CodeBadRequest, "request appmap: invalid app ID")
		}
	case OpNetpolCheck:
		return r.NetpolCheck.validate()
	case OpServiceCheck:
		if r.Name != "" && r.Namespace == "" {
			return errorf(CodeBadRequest, "request service.check: a name needs a namespace")
		}
	case OpPodDiagnose, OpPodDebug:
		if r.Namespace == "" || r.Name == "" {
			return errorf(CodeBadRequest, "request %s: needs namespace and name", r.Op)
		}
	case OpNodePressure:
		if r.Namespace != "" {
			return errorf(CodeBadRequest, "request node.pressure: nodes have no namespace")
		}
		return validNodeMetrics(r)
	case OpHealthCheck:
		return validNodeMetrics(r)
	case OpPodUsage:
		if r.Name != "" && r.Namespace == "" {
			return errorf(CodeBadRequest, "request pod.usage: a pod name needs a namespace")
		}
	case OpRBACWhoCan:
		return r.RBAC.validate()
	case OpLogSearch:
		return r.LogSearch.validate()
	case OpChain:
		return r.Chain.validate()
	default:
		return errorf(CodeUnsupported, "request: unknown op %q", r.Op)
	}
	return nil
}

// Response answers one request. Direction: cluster agent → portal. Either
// Error is set, or at most one kind of result (an empty list has none).
//
// The agent pages list results so each response stays under MaxMessageSize:
// it uses a default page size and, when the encoded page is too big, returns
// fewer items with a Continue token.
type Response struct {
	Error *Error `json:"error,omitempty"`

	// list: raw Kubernetes objects, plus the continue token for the next page.
	Items    []json.RawMessage `json:"items,omitempty"`
	Continue string            `json:"continue,omitempty"`
	// get: one raw Kubernetes object.
	Object json.RawMessage `json:"object,omitempty"`
	// logs: the raw log bytes, at most MaxLogBytes, base64 on the wire so any
	// content (colour codes, invalid UTF-8) arrives unchanged. Truncated is
	// true if older lines were cut to fit.
	Logs      []byte `json:"logs,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// tokenreview: the result.
	TokenReview *TokenReviewResult `json:"tokenReview,omitempty"`
}

// TokenReviewResult is what the Kubernetes API said about a node token.
// PodName, PodUID and NodeName come from the token's bound-object claims
// (Kubernetes 1.30 or newer). Audiences is status.audiences: an authenticator
// that ignores audiences can answer authenticated with no audiences, so the
// portal must check it (see AcceptNode).
type TokenReviewResult struct {
	Authenticated bool     `json:"authenticated"`
	Audiences     []string `json:"audiences,omitempty"`
	Username      string   `json:"username,omitempty"`
	PodName       string   `json:"podName,omitempty"`
	PodUID        string   `json:"podUid,omitempty"`
	NodeName      string   `json:"nodeName,omitempty"`
	// ExpiresAt is when the reviewed token expires (its exp claim, read by
	// the cluster agent after the API server authenticated it). The portal
	// ends the node agent's session then (review-km-44 L7).
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// AcceptNode is the portal's rule for a node agent registration (A8). It
// accepts only when the token is authenticated, was checked for
// NodeTokenAudience, belongs to the node agent's ServiceAccount, and is bound
// to the node the agent claims in register.
func (t *TokenReviewResult) AcceptNode(nodeName string) error {
	switch {
	case t == nil || !t.Authenticated:
		return errorf(CodeUnauthorized, "node token not authenticated")
	case !slices.Contains(t.Audiences, NodeTokenAudience):
		return errorf(CodeUnauthorized, "node token was not checked for audience %q", NodeTokenAudience)
	case t.Username != NodeAgentUsername:
		return errorf(CodeUnauthorized, "node token belongs to %q, not the node agent", t.Username)
	case nodeName == "" || t.NodeName != nodeName:
		return errorf(CodeUnauthorized, "node token is bound to node %q, register says %q", t.NodeName, nodeName)
	}
	return nil
}

func (*Response) MessageType() Type { return TypeResponse }

func (r *Response) Validate() error {
	results := 0
	for _, set := range []bool{len(r.Items) > 0 || r.Continue != "", len(r.Object) > 0, len(r.Logs) > 0 || r.Truncated, r.TokenReview != nil} {
		if set {
			results++
		}
	}
	if r.Error != nil {
		if results > 0 {
			return errorf(CodeBadRequest, "response: error and result both set")
		}
		return r.Error.Validate()
	}
	if results > 1 {
		return errorf(CodeBadRequest, "response: more than one kind of result")
	}
	if len(r.Logs) > MaxLogBytes {
		return errorf(CodeTooLarge, "response: logs are %d bytes, limit %d", len(r.Logs), MaxLogBytes)
	}
	return nil
}

// StreamKind is what a stream carries.
type StreamKind string

// StreamLogs follows pod logs, like kubectl logs -f.
const StreamLogs StreamKind = "logs"

// StreamOpen starts a stream. Direction: portal → cluster agent. The envelope
// id becomes the stream id for every stream.data and stream.close of it.
type StreamOpen struct {
	Kind      StreamKind  `json:"kind"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"` // the pod
	Logs      *LogOptions `json:"logs,omitempty"`
}

func (*StreamOpen) MessageType() Type { return TypeStreamOpen }

func (s *StreamOpen) Validate() error {
	if s.Kind != StreamLogs {
		return errorf(CodeUnsupported, "stream.open: unknown kind %q", s.Kind)
	}
	if s.Namespace == "" || s.Name == "" {
		return errorf(CodeBadRequest, "stream.open: needs namespace and name")
	}
	return nil
}

// StreamData is one chunk of a stream. Direction: cluster agent → portal.
// Seq starts at 0 and grows by one per chunk, so gaps are visible. Data is
// raw bytes, base64 on the wire, at most MaxStreamChunk.
type StreamData struct {
	Seq  uint64 `json:"seq"`
	Data []byte `json:"data"`
}

func (*StreamData) MessageType() Type { return TypeStreamData }

func (s *StreamData) Validate() error {
	if len(s.Data) > MaxStreamChunk {
		return errorf(CodeTooLarge, "stream.data: chunk is %d bytes, limit %d", len(s.Data), MaxStreamChunk)
	}
	return nil
}

// StreamClose ends a stream. Either side may send it: the portal to cancel,
// the agent when the source ends or fails. Error is set when it failed.
type StreamClose struct {
	Reason string `json:"reason,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

func (*StreamClose) MessageType() Type { return TypeStreamClose }

func (s *StreamClose) Validate() error {
	if s.Error != nil {
		return s.Error.Validate()
	}
	return nil
}

// validNodeMetrics checks Request.NodeMetrics (node.pressure, health.check).
func validNodeMetrics(r *Request) error {
	if len(r.NodeMetrics) > MaxNodeReports {
		return errorf(CodeBadRequest, "request %s: at most %d node reports", r.Op, MaxNodeReports)
	}
	seen := map[string]bool{}
	for _, m := range r.NodeMetrics {
		if m.NodeName == "" || !validName(m.NodeName) || seen[m.NodeName] {
			return errorf(CodeBadRequest, "request %s: node reports need a unique, valid nodeName", r.Op)
		}
		seen[m.NodeName] = true
	}
	return nil
}
