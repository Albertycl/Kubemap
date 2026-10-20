package protocol

import "time"

// The node.pressure op (KM-60) tells, per node, whether workloads are really
// waiting for a resource. Kubernetes alone is not enough: its node
// conditions cover memory, disk and PIDs only, flip late (at the eviction
// threshold) and say nothing about CPU or I/O. Kernel pressure stall
// information (PSI) says how much of the time tasks were stalled. The
// cluster agent combines the conditions from its cache with the PSI in the
// node agents' reports, which the portal passes in Request.NodeMetrics: the
// cluster agent never calls a kubelet, so its cluster-wide read token is
// never sent to a node. The request may name one node with Request.Name.

// MaxNodeReports bounds Request.NodeMetrics.
const MaxNodeReports = 1000

// NodeReportMaxAge is how old a node agent report may be (by its own clock)
// before its PSI counts as not available. Node agents report every 10 s.
const NodeReportMaxAge = 2 * time.Minute

// Pressure verdicts, from best to worst. VerdictUnknown means the evidence is
// missing (no PSI and no condition, or a condition in state Unknown).
const (
	VerdictOK       = "ok"
	VerdictElevated = "elevated" // tasks wait noticeably; not yet harmful
	VerdictPressure = "pressure" // tasks wait a lot, or the kubelet is acting
	VerdictUnknown  = "unknown"
)

// PSI thresholds on the 60-second average, in percent of wall time.
const (
	// PSISomeElevated and PSISomePressure apply to "some": at least one
	// task was stalled.
	PSISomeElevated = 10.0
	PSISomePressure = 25.0
	// PSIFullPressure applies to "full" (memory and io): all non-idle tasks
	// were stalled at once, so no progress was made.
	PSIFullPressure = 5.0
)

// NodePressureReport is the response object of a node.pressure request.
type NodePressureReport struct {
	Time  time.Time      `json:"time"`
	Nodes []NodePressure `json:"nodes"`
	// Notes say which nodes had no fresh node agent report.
	Notes []string `json:"notes,omitempty"`
}

// NodePressure is one node.
type NodePressure struct {
	Node  string `json:"node"`
	Ready bool   `json:"ready"`
	// PSIAvailable is false when there is no fresh node agent report with
	// PSI: no node agent on the node, a report older than NodeReportMaxAge,
	// or a kernel without PSI.
	PSIAvailable bool `json:"psiAvailable"`
	// ReportTime is when the node agent took the sample used (its clock);
	// zero when there was no report.
	ReportTime time.Time `json:"reportTime,omitzero"`
	// Verdict is the worst resource verdict; Summary says it in one line.
	Verdict   string             `json:"verdict"`
	Summary   string             `json:"summary"`
	Resources []ResourcePressure `json:"resources"`
}

// ResourcePressure is one resource: cpu, memory, io, disk or pids.
type ResourcePressure struct {
	Resource string `json:"resource"`
	// Condition is the kubelet's node condition for this resource
	// ("MemoryPressure"); empty for cpu and io, which have none.
	Condition string `json:"condition,omitempty"`
	// ConditionStatus is "True", "False" or "Unknown"; empty without a
	// condition or when the Node does not report it.
	ConditionStatus string    `json:"conditionStatus,omitempty"`
	ConditionSince  time.Time `json:"conditionSince,omitzero"`
	// PSI is nil when not available: then the verdict rests on the
	// condition alone (memory), or is unknown (cpu, io). Disk and pids have
	// no PSI.
	PSI     *Pressure `json:"psi,omitempty"`
	Verdict string    `json:"verdict"`
	// Detail is the evidence in one line: "tasks waited for CPU 27% of the
	// last minute; Kubernetes has no CPU pressure condition".
	Detail string `json:"detail"`
}
