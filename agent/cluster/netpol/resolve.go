package netpol

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubemap/pkg/protocol"
)

func perr(code protocol.ErrorCode, format string, args ...any) error {
	return &protocol.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (s *Snapshot) pod(ns, name string) *corev1.Pod {
	for _, p := range s.Pods {
		if p.Namespace == ns && p.Name == name {
			return p
		}
	}
	return nil
}

func podEndpoint(p *corev1.Pod) endpoint {
	ip, _ := netip.ParseAddr(p.Status.PodIP)
	return endpoint{pod: p, ip: ip, name: "Pod/" + p.Namespace + "/" + p.Name}
}

// finished pods (Succeeded, Failed, evicted) carry no traffic, and their IP
// may already belong to a new pod.
func finished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// ipEndpoint returns the pod that owns ip (not host-network pods, which
// share the node's IP), or a bare IP.
func (s *Snapshot) ipEndpoint(ip netip.Addr) endpoint {
	for _, p := range s.Pods {
		if p.Spec.HostNetwork || finished(p) {
			continue
		}
		for _, pip := range p.Status.PodIPs {
			if a, err := netip.ParseAddr(pip.IP); err == nil && a == ip {
				return podEndpoint(p)
			}
		}
		if a, err := netip.ParseAddr(p.Status.PodIP); err == nil && a == ip {
			return podEndpoint(p)
		}
	}
	return endpoint{ip: ip, name: "IP " + ip.String()}
}

// source resolves the "from" end: a pod or an IP.
func (s *Snapshot) source(e protocol.NetpolEndpoint) (endpoint, error) {
	switch {
	case e.Service != "":
		return endpoint{}, perr(protocol.CodeBadRequest, "netpol.check: the source must be a pod or an IP, not a Service")
	case e.Pod != "":
		p := s.pod(e.Namespace, e.Pod)
		if p == nil {
			return endpoint{}, perr(protocol.CodeNotFound, "netpol.check: no pod %s/%s", e.Namespace, e.Pod)
		}
		return podEndpoint(p), nil
	}
	ip, err := netip.ParseAddr(e.IP)
	if err != nil {
		return endpoint{}, perr(protocol.CodeBadRequest, "netpol.check: %q is not an IP address", e.IP)
	}
	return s.ipEndpoint(ip), nil
}

// destinations resolves the "to" end. A Service becomes its backends: the
// pods its selector matches, or the addresses of its EndpointSlices when it
// has no selector (the API server's "kubernetes" Service). The returned
// function gives the destination port for each target: a Service port is
// mapped to its targetPort, which may be named per pod.
func (s *Snapshot) destinations(e protocol.NetpolEndpoint, port int32, proto corev1.Protocol) ([]endpoint, func(endpoint) int32, []string, error) {
	fixed := func(endpoint) int32 { return port }
	switch {
	case e.Pod != "":
		p := s.pod(e.Namespace, e.Pod)
		if p == nil {
			return nil, nil, nil, perr(protocol.CodeNotFound, "netpol.check: no pod %s/%s", e.Namespace, e.Pod)
		}
		return []endpoint{podEndpoint(p)}, fixed, nil, nil
	case e.Service != "":
		return s.serviceTargets(e.Namespace, e.Service, port, proto)
	}
	ip, err := netip.ParseAddr(e.IP)
	if err != nil {
		return nil, nil, nil, perr(protocol.CodeBadRequest, "netpol.check: %q is not an IP address", e.IP)
	}
	if svc := s.serviceByClusterIP(ip); svc != nil {
		notes := []string{fmt.Sprintf("%s is the ClusterIP of Service %s/%s: policies see its backends after DNAT", ip, svc.Namespace, svc.Name)}
		t, f, n, err := s.serviceTargets(svc.Namespace, svc.Name, port, proto)
		return t, f, append(notes, n...), err
	}
	return []endpoint{s.ipEndpoint(ip)}, fixed, nil, nil
}

func (s *Snapshot) serviceByClusterIP(ip netip.Addr) *corev1.Service {
	for _, svc := range s.Services {
		if a, err := netip.ParseAddr(svc.Spec.ClusterIP); err == nil && a == ip {
			return svc
		}
	}
	return nil
}

func (s *Snapshot) serviceTargets(ns, name string, port int32, proto corev1.Protocol) ([]endpoint, func(endpoint) int32, []string, error) {
	var svc *corev1.Service
	for _, x := range s.Services {
		if x.Namespace == ns && x.Name == name {
			svc = x
		}
	}
	if svc == nil {
		return nil, nil, nil, perr(protocol.CodeNotFound, "netpol.check: no Service %s/%s", ns, name)
	}
	var notes []string
	var sp *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port && cmp.Or(svc.Spec.Ports[i].Protocol, corev1.ProtocolTCP) == proto {
			sp = &svc.Spec.Ports[i]
		}
	}
	if sp == nil {
		notes = append(notes, fmt.Sprintf("Service %s/%s has no port %d/%s; checking the backends on port %d directly", ns, name, port, proto, port))
	}
	var targets []endpoint
	if len(svc.Spec.Selector) > 0 {
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		for _, p := range s.Pods {
			if p.Namespace == ns && !finished(p) && sel.Matches(labels.Set(p.Labels)) {
				targets = append(targets, podEndpoint(p))
			}
		}
	} else {
		for _, es := range s.Slices {
			if es.Namespace != ns || es.Labels[discoveryv1.LabelServiceName] != name {
				continue
			}
			for _, ep := range es.Endpoints {
				for _, a := range ep.Addresses {
					if ip, err := netip.ParseAddr(a); err == nil {
						targets = append(targets, s.ipEndpoint(ip))
					}
				}
			}
		}
		notes = append(notes, fmt.Sprintf("Service %s/%s has no selector: its backends come from its EndpointSlices", ns, name))
	}
	slices.SortFunc(targets, func(a, b endpoint) int { return strings.Compare(a.name, b.name) })
	targets = slices.CompactFunc(targets, func(a, b endpoint) bool { return a.name == b.name })
	portOf := func(t endpoint) int32 {
		if sp == nil {
			return port
		}
		tp := sp.TargetPort
		switch {
		case tp.Type == intstr.Int && tp.IntVal != 0:
			return tp.IntVal
		case tp.Type == intstr.String && tp.StrVal != "" && t.pod != nil:
			if n, ok := namedPort(t.pod, tp.StrVal, cmp.Or(sp.Protocol, corev1.ProtocolTCP)); ok {
				return n
			}
		case tp.Type == intstr.String && tp.StrVal != "":
			if n, ok := s.slicePort(ns, name, tp.StrVal, sp.Name); ok {
				return n
			}
		}
		return sp.Port // targetPort defaults to port
	}
	return targets, portOf, notes, nil
}

// slicePort finds a named port of a selector-less Service in its slices.
func (s *Snapshot) slicePort(ns, svc string, names ...string) (int32, bool) {
	for _, es := range s.Slices {
		if es.Namespace != ns || es.Labels[discoveryv1.LabelServiceName] != svc {
			continue
		}
		for _, p := range es.Ports {
			if p.Name != nil && p.Port != nil && slices.Contains(names, *p.Name) {
				return *p.Port, true
			}
		}
	}
	return 0, false
}
