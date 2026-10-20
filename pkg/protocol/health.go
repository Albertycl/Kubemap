package protocol

import "time"

// The health.check op (KM-61, KM-62) runs Kubemap's built-in health rules
// in the cluster agent: node rules on the node agents' reports (passed by
// the portal in Request.NodeMetrics, as for node.pressure) and the node
// conditions (disk before eviction, image clean-up, inodes, memory, swap,
// pressure, PIDs) and pod rules on the cache (CrashLoopBackOff, exit 137,
// Pending, no memory limit). Every finding says what it means, why it
// matters, how to fix it and which command proves the fix. The rules run in
// the agent; only the findings leave the cluster. Request.Namespace, when
// set, limits the pod rules to one namespace.

// Finding severities, most severe first.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// MaxHealthFindings bounds one report; the rest are counted in a note.
const MaxHealthFindings = 200

// MaxFindingItems bounds the objects listed in one grouped finding.
const MaxFindingItems = 20

// HealthReport is the response object of a health.check request.
type HealthReport struct {
	Time time.Time `json:"time"`
	// Findings, most severe first, then by object.
	Findings []HealthFinding `json:"findings"`
	// Rules lists every rule that ran, so "no finding" can be told from
	// "not checked".
	Rules []string `json:"rules"`
	// Thresholds are the values the rules used, with where they came from.
	Thresholds HealthThresholds `json:"thresholds"`
	// Notes say what could not be checked (no fresh node agent report).
	Notes []string `json:"notes,omitempty"`
}

// HealthFinding is one problem with its explanation (KM-62).
type HealthFinding struct {
	Rule     string `json:"rule"`     // "node.disk.eviction"
	Severity string `json:"severity"` // critical, warning, info
	// Object is what the finding is about: "Node/k8server", "Pod/ns/name",
	// or "Namespace/ns" for a grouped finding.
	Object string `json:"object"`
	Title  string `json:"title"`
	// Evidence is the measured values in one or two lines.
	Evidence string `json:"evidence"`
	Meaning  string `json:"meaning"` // what it means
	Why      string `json:"why"`     // why it matters
	Fix      string `json:"fix"`     // the suggested fix
	// Verify is the command that proves the fix, for a person to type.
	Verify string `json:"verify"`
	// Items are the objects of a grouped finding (at most
	// MaxFindingItems); Count is how many there are in total.
	Items []string `json:"items,omitempty"`
	Count int      `json:"count,omitempty"`
}

// HealthThresholds are the kubelet thresholds the node rules compare with.
// The agent cannot read the kubelet configuration (that needs nodes/proxy),
// so they are the kubelet defaults unless the agent is told otherwise.
type HealthThresholds struct {
	// EvictionHard is the kubelet's evictionHard, for example
	// {"nodefs.available": "5%", "memory.available": "100Mi"}.
	EvictionHard map[string]string `json:"evictionHard"`
	// EvictionMinimumReclaim is how much more the kubelet frees once it
	// starts evicting, for example {"nodefs.available": "10%"}.
	EvictionMinimumReclaim map[string]string `json:"evictionMinimumReclaim,omitempty"`
	// ImageGCHighPercent is the disk usage at which the kubelet deletes
	// unused images.
	ImageGCHighPercent int `json:"imageGCHighPercent"`
	// Source: "kubelet defaults" or "agent settings".
	Source string `json:"source"`
}
