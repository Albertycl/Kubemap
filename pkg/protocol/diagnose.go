package protocol

import "time"

// The pod.diagnose op (KM-56) collects, in one request, what an SRE looks at
// first when a pod crashes: status and reason, restarts, the last exit code
// and what it means, the previous container's last log lines, recent events,
// and resource requests and limits next to current usage. The agent also
// writes plain findings ("Killed for memory (exit 137)"). The
// request names the pod with Request.Namespace and Request.Name.

// MaxDiagnoseLogLines is how many log lines each container contributes.
const MaxDiagnoseLogLines = 100

// PodDiagnosis is the response object of a pod.diagnose request.
type PodDiagnosis struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	Owner     string `json:"owner,omitempty"` // top controller, "Deployment/ns/name"
	Phase     string `json:"phase"`
	// Status is what "kubectl get pods" shows: "CrashLoopBackOff",
	// "ErrImageNeverPull", "Evicted", "Running".
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`  // pod-level: "Evicted"
	Message string `json:"message,omitempty"` // pod-level message
	QOS     string `json:"qosClass,omitempty"`
	// Findings are the agent's conclusions, most important first.
	Findings   []DiagFinding   `json:"findings"`
	Containers []DiagContainer `json:"containers"`
	Events     []DiagEvent     `json:"events"`
	// Notes say what could not be read (no metrics API, logs forbidden).
	Notes []string `json:"notes,omitempty"`
}

// DiagFinding is one conclusion.
type DiagFinding struct {
	Severity  string `json:"severity"` // "critical", "warning", "info"
	Container string `json:"container,omitempty"`
	Title     string `json:"title"`  // "Killed for memory (exit 137)"
	Detail    string `json:"detail"` // evidence and the next step
}

// DiagContainer is one container (init containers included, marked).
type DiagContainer struct {
	Name     string `json:"name"`
	Init     bool   `json:"init,omitempty"`
	Image    string `json:"image"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	// State is the current state in words: "running since …",
	// "waiting: CrashLoopBackOff", "terminated: Error (exit 1)".
	State string `json:"state"`
	// Last is the previous run, when it ended: the reason of a crash.
	Last *DiagTermination `json:"last,omitempty"`
	// Requests and Limits by resource ("cpu": "50m", "memory": "64Mi").
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
	// Usage now, from the metrics API; nil when it is not available.
	Usage  *DiagUsage `json:"usage,omitempty"`
	Probes []string   `json:"probes,omitempty"` // "liveness: GET /healthz :http every 10s, timeout 1s, 3 failures"
	// Logs are the last lines of the previous run (LogsFrom "previous") or,
	// when there was none, of the current one ("current").
	Logs          string `json:"logs,omitempty"`
	LogsFrom      string `json:"logsFrom,omitempty"`
	LogsTruncated bool   `json:"logsTruncated,omitempty"`
}

// DiagTermination is how a container run ended.
type DiagTermination struct {
	Reason   string    `json:"reason"` // "OOMKilled", "Error", "Completed"
	ExitCode int32     `json:"exitCode"`
	Signal   int32     `json:"signal,omitempty"`
	Meaning  string    `json:"meaning"` // "SIGKILL (137): killed, usually the OOM killer"
	Started  time.Time `json:"startedAt,omitzero"`
	Finished time.Time `json:"finishedAt,omitzero"`
	Message  string    `json:"message,omitempty"`
}

// DiagUsage is a container's current usage, with the share of its limit.
type DiagUsage struct {
	CPUMillis   int64   `json:"cpuMillis"`
	MemoryBytes int64   `json:"memoryBytes"`
	CPUOfLimit  float64 `json:"cpuOfLimit,omitempty"`    // percent; 0 without a limit
	MemOfLimit  float64 `json:"memoryOfLimit,omitempty"` // percent; 0 without a limit
}

// DiagEvent is one event about the pod, newest first.
type DiagEvent struct {
	Type    string    `json:"type"` // "Normal" or "Warning"
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Count   int32     `json:"count"`
	Last    time.Time `json:"lastSeen,omitzero"`
}
