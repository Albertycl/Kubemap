package protocol

// The pod.debug op (KM-76) walks the SRE checklist for one pod, in the
// order a person would: status and reason → events → logs → exit code →
// requests and limits against usage → node → Service endpoints →
// NetworkPolicy. Each step has evidence, a verdict and the kubectl command a
// person would type to see the same thing, so the portal can teach the
// method, not only show the answer. The request names the pod with
// Request.Namespace and Request.Name.

// Debug step keys, in checklist order.
const (
	StepStatus    = "status"
	StepEvents    = "events"
	StepLogs      = "logs"
	StepExitCode  = "exit-code"
	StepResources = "resources"
	StepNode      = "node"
	StepService   = "service"
	StepNetpol    = "netpol"
)

// Debug verdicts: VerdictOK ("ok") and VerdictUnknown ("unknown", the data
// was not available) are shared with node.pressure (pressure.go); a step
// adds VerdictProblem.
const VerdictProblem = "problem"

// PodDebug is the response object of a pod.debug request.
type PodDebug struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Owner     string `json:"owner,omitempty"`
	Status    string `json:"status"`
	// Cause is the most specific problem found, as one line ("NetworkPolicy
	// kubemap-demo/api-allow-monitoring-only blocks web → api:80"); empty
	// when every step is ok. CauseStep is its step key.
	Cause     string      `json:"cause,omitempty"`
	CauseStep string      `json:"causeStep,omitempty"`
	Steps     []DebugStep `json:"steps"` // always all eight, in order
	Notes     []string    `json:"notes,omitempty"`
}

// DebugStep is one step of the checklist.
type DebugStep struct {
	Key     string `json:"key"`   // StepStatus …
	Title   string `json:"title"` // "Status and reason"
	Verdict string `json:"verdict"`
	// Finding is the step's conclusion in one line.
	Finding  string   `json:"finding"`
	Evidence []string `json:"evidence,omitempty"`
	// Commands are what a person types to check this step by hand.
	Commands []string `json:"commands"`
}
