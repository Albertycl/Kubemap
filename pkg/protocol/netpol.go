package protocol

import "net/netip"

// The netpol.check op (KM-33) asks the cluster agent whether NetworkPolicies
// allow a connection, and which policy decides. The agent evaluates the
// policies from its cache; nothing is sent to the cluster.

// NetpolCheckRequest is Request.NetpolCheck.
type NetpolCheckRequest struct {
	From NetpolEndpoint `json:"from"`
	To   NetpolEndpoint `json:"to"`
	// Port is the destination port: the Service port when To is a Service
	// (the agent maps it to each backend's targetPort), else the pod or IP
	// port. 1..65535.
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"` // TCP (default), UDP or SCTP
}

// NetpolEndpoint is one end: a pod (namespace + pod), a Service (namespace
// + service, destination only) or an IP address. Exactly one of pod,
// service and ip is set.
type NetpolEndpoint struct {
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Service   string `json:"service,omitempty"`
	IP        string `json:"ip,omitempty"`
}

func (e NetpolEndpoint) validate(what string) error {
	set := 0
	for _, v := range []string{e.Pod, e.Service, e.IP} {
		if v != "" {
			set++
		}
	}
	switch {
	case set != 1:
		return errorf(CodeBadRequest, "request netpol.check: %s needs exactly one of pod, service and ip", what)
	case (e.Pod != "" || e.Service != "") && (e.Namespace == "" || !validName(e.Namespace) || !validName(e.Pod) || !validName(e.Service)):
		return errorf(CodeBadRequest, "request netpol.check: %s needs a valid namespace and name", what)
	case e.IP != "":
		if _, err := netip.ParseAddr(e.IP); err != nil {
			return errorf(CodeBadRequest, "request netpol.check: %s ip %q is not an IP address", what, e.IP)
		}
	}
	return nil
}

func (r *NetpolCheckRequest) validate() error {
	switch {
	case r == nil:
		return errorf(CodeBadRequest, "request netpol.check: needs netpolCheck")
	case r.Port < 1 || r.Port > 65535:
		return errorf(CodeBadRequest, "request netpol.check: port must be 1..65535")
	case r.Protocol != "" && r.Protocol != "TCP" && r.Protocol != "UDP" && r.Protocol != "SCTP":
		return errorf(CodeBadRequest, "request netpol.check: protocol must be TCP, UDP or SCTP")
	case r.From.Service != "":
		return errorf(CodeBadRequest, "request netpol.check: the source must be a pod or an IP")
	}
	if err := r.From.validate("from"); err != nil {
		return err
	}
	return r.To.validate("to")
}

// NetpolResult is the response object of a netpol.check request.
type NetpolResult struct {
	// Allowed is true when every path is allowed. A Service with several
	// backends has one path per backend.
	Allowed  bool         `json:"allowed"`
	Summary  string       `json:"summary"` // one line for the UI
	From     string       `json:"from"`    // "Pod/ns/name" or "IP 1.2.3.4"
	Port     int32        `json:"port"`
	Protocol string       `json:"protocol"`
	Paths    []NetpolPath `json:"paths"`
	// Notes explain things the verdict depends on: a ClusterIP resolved to
	// its backends, a host-network pod, a Service without a selector.
	Notes []string `json:"notes,omitempty"`
}

// NetpolPath is the verdict for one source → destination pair.
type NetpolPath struct {
	To      string     `json:"to"`   // "Pod/ns/name" or "IP 1.2.3.4"
	Port    int32      `json:"port"` // the destination port after targetPort mapping
	Allowed bool       `json:"allowed"`
	Egress  NetpolSide `json:"egress"`  // at the source
	Ingress NetpolSide `json:"ingress"` // at the destination
}

// NetpolSide is one end's verdict.
type NetpolSide struct {
	Direction string `json:"direction"` // "egress" or "ingress"
	// Isolated is true when some policy selects the pod for this direction;
	// then only a matching rule allows traffic.
	Isolated bool `json:"isolated"`
	Allowed  bool `json:"allowed"`
	// Decision is one line: "allowed by ns/policy (ingress rule 0)",
	// "blocked: isolated by ns/a, ns/b and no ingress rule matches".
	Decision string `json:"decision"`
	// Selecting are the policies that select the pod for this direction
	// ("namespace/name"); when blocked, they are the deciding policies.
	Selecting []string `json:"selecting,omitempty"`
	// AllowedBy are the rules that match, the first one being the decision.
	AllowedBy []NetpolRule `json:"allowedBy,omitempty"`
}

// NetpolRule names one rule of a policy.
type NetpolRule struct {
	Policy string `json:"policy"` // "namespace/name"
	Rule   int    `json:"rule"`   // index in spec.ingress or spec.egress
	Detail string `json:"detail"` // "from namespaces kubernetes.io/metadata.name=kube-system / pods app.kubernetes.io/name=traefik on 5000/TCP"
}
