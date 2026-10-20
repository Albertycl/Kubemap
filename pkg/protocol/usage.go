package protocol

import "time"

// The pod.usage op (KM-28) returns what each pod really uses, from the
// metrics API (pods.metrics.k8s.io, served by metrics-server; read-only,
// approved 2026-09-28): CPU and memory working set per pod and per
// container, next to the containers' requests and limits. The cluster agent
// makes no kubelet calls. Request.Namespace limits it to one namespace;
// Request.Name (with a namespace) to one pod.

// MaxUsagePods bounds one pod.usage answer; ask for one namespace for more.
const MaxUsagePods = 500

// PodUsageReport is the response object of a pod.usage request.
type PodUsageReport struct {
	Time time.Time `json:"time"`
	// Pods, sorted by memory working set, largest first.
	Pods []PodUsage `json:"pods"`
	// Notes say when pods were cut, or had no metrics yet.
	Notes []string `json:"notes,omitempty"`
}

// PodUsage is one pod: the sum of its containers.
type PodUsage struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"` // from the pod spec
	// SampledAt and WindowSeconds say which interval metrics-server
	// averaged the CPU over.
	SampledAt     time.Time `json:"sampledAt,omitzero"`
	WindowSeconds float64   `json:"windowSeconds,omitempty"`
	CPUMillis     int64     `json:"cpuMillis"`
	// MemoryBytes is the working set: what kubectl top, eviction and the
	// OOM killer look at.
	MemoryBytes int64            `json:"memoryBytes"`
	Containers  []ContainerUsage `json:"containers"`
}

// ContainerUsage is one container, with its requests and limits.
type ContainerUsage struct {
	Name        string `json:"name"`
	CPUMillis   int64  `json:"cpuMillis"`
	MemoryBytes int64  `json:"memoryBytes"` // working set
	// Requests and limits from the pod spec, 0 when not set.
	CPURequestMillis   int64 `json:"cpuRequestMillis,omitempty"`
	CPULimitMillis     int64 `json:"cpuLimitMillis,omitempty"`
	MemoryRequestBytes int64 `json:"memoryRequestBytes,omitempty"`
	MemoryLimitBytes   int64 `json:"memoryLimitBytes,omitempty"`
	// Usage as a percentage of the request and the limit; 0 when not set.
	CPUOfRequest    float64 `json:"cpuOfRequest,omitempty"`
	CPUOfLimit      float64 `json:"cpuOfLimit,omitempty"`
	MemoryOfRequest float64 `json:"memoryOfRequest,omitempty"`
	MemoryOfLimit   float64 `json:"memoryOfLimit,omitempty"`
}
