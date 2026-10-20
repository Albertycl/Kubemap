package protocol

// The service.check op (KM-77) checks Services for the problems that make a
// Service reachable but useless: a selector that matches no pods, pods that
// are not ready, and ports that lead nowhere. Request.Namespace narrows it to
// one namespace and Request.Name to one Service (Name needs Namespace).

// ServiceCheckResult is the response object of a service.check request.
type ServiceCheckResult struct {
	// Services are sorted with problems first (critical, then warning),
	// then by ID.
	Services []ServiceStatus `json:"services"`
	Problems int             `json:"problems"` // Services with a critical or warning finding
}

// ServiceStatus is one Service's verdict.
type ServiceStatus struct {
	ID        string            `json:"id"` // "Service/ns/name"
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Type      string            `json:"type"` // ClusterIP, NodePort, LoadBalancer, ExternalName
	Selector  map[string]string `json:"selector,omitempty"`
	// PodsMatched counts running or pending pods the selector matches;
	// PodsReady those that are ready.
	PodsMatched int `json:"podsMatched"`
	PodsReady   int `json:"podsReady"`
	// ReadyEndpoints from the EndpointSlices; -1 when they are not known.
	ReadyEndpoints int              `json:"readyEndpoints"`
	Healthy        bool             `json:"healthy"`
	Findings       []ServiceFinding `json:"findings,omitempty"`
}

// ServiceFinding is one problem with a Service.
type ServiceFinding struct {
	Severity string `json:"severity"` // "critical", "warning" or "info"
	Problem  string `json:"problem"`  // "selector matches no pods", "no ready pods", "targetPort not declared"
	Port     string `json:"port,omitempty"`
	Detail   string `json:"detail"` // the evidence
	Fix      string `json:"fix"`    // what to change
	// Command is what a person types to see the evidence themselves.
	Command string `json:"command"`
}
