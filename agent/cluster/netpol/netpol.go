// Package netpol evaluates Kubernetes NetworkPolicies inside the cluster
// agent (KM-33): may this source pod (or IP) reach that destination pod,
// Service or IP on this port? It answers for both sides, egress at the
// source and ingress at the destination, and names the policy and rule that
// decided, because "which policy blocks this?" is the question an SRE asks.
//
// The rules are the Kubernetes API semantics (networking.k8s.io/v1):
//
//   - A pod is isolated for a direction when at least one policy in its
//     namespace selects it with that direction in policyTypes. policyTypes
//     defaults to Ingress, plus Egress when the policy has egress rules.
//   - Traffic is allowed when it is allowed at both ends. An isolated side
//     allows it only if some rule of some selecting policy matches; rules
//     add up, there is no deny rule.
//   - A peer with only podSelector matches pods in the policy's namespace;
//     with namespaceSelector (and optionally podSelector) pods in matching
//     namespaces; ipBlock matches IP addresses (CIDR minus except).
//     Selectors only ever match pods: an IP that is not a pod (the internet,
//     a node, the API server) is matched by ipBlock alone.
//   - An empty from/to list matches every peer, an empty ports list every
//     port. Ports may be named (resolved on the destination pod) and may
//     have an endPort range. The protocol defaults to TCP.
//
// Not modelled: CNI-specific extensions (Calico GlobalNetworkPolicy),
// traffic from the node itself (kubelet probes are usually always allowed),
// and hostNetwork pods, which are treated as their node's IP.
package netpol

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubemap/pkg/protocol"
)

// Source is the agent's cache (or a test fake).
type Source interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

var (
	policiesGVR   = gvr("networking.k8s.io", "v1", "networkpolicies")
	namespacesGVR = gvr("", "v1", "namespaces")
	podsGVR       = gvr("", "v1", "pods")
	servicesGVR   = gvr("", "v1", "services")
	slicesGVR     = gvr("discovery.k8s.io", "v1", "endpointslices")
)

// Snapshot is what the engine reads: the cluster's policies, namespace
// labels, pods and Services at one moment.
type Snapshot struct {
	Policies   []*networkingv1.NetworkPolicy
	Namespaces map[string]map[string]string // name → labels
	Pods       []*corev1.Pod
	Services   []*corev1.Service
	Slices     []*discoveryv1.EndpointSlice
}

// Load reads a snapshot from the cache. Policies, namespaces and pods are
// required: without them any answer would be a guess. Services and
// EndpointSlices are optional (only Service destinations need them).
func Load(src Source) (*Snapshot, error) {
	s := &Snapshot{Namespaces: map[string]map[string]string{}}
	list := func(g schema.GroupVersionResource, add func(runtime.Object)) error {
		objs, err := src.List(g, "", nil)
		if err != nil {
			return fmt.Errorf("netpol: %s: %w", g.Resource, err)
		}
		for _, o := range objs {
			add(o)
		}
		return nil
	}
	if err := list(policiesGVR, func(o runtime.Object) { s.Policies = append(s.Policies, o.(*networkingv1.NetworkPolicy)) }); err != nil {
		return nil, err
	}
	if err := list(namespacesGVR, func(o runtime.Object) {
		ns := o.(*corev1.Namespace)
		s.Namespaces[ns.Name] = namespaceLabels(ns)
	}); err != nil {
		return nil, err
	}
	if err := list(podsGVR, func(o runtime.Object) { s.Pods = append(s.Pods, o.(*corev1.Pod)) }); err != nil {
		return nil, err
	}
	_ = list(servicesGVR, func(o runtime.Object) { s.Services = append(s.Services, o.(*corev1.Service)) })
	_ = list(slicesGVR, func(o runtime.Object) { s.Slices = append(s.Slices, o.(*discoveryv1.EndpointSlice)) })
	slices.SortFunc(s.Policies, func(a, b *networkingv1.NetworkPolicy) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return s, nil
}

// namespaceLabels returns a namespace's labels, with the
// kubernetes.io/metadata.name label every namespace has since 1.21 (added
// when a test fixture lacks it).
func namespaceLabels(ns *corev1.Namespace) map[string]string {
	l := map[string]string{"kubernetes.io/metadata.name": ns.Name}
	for k, v := range ns.Labels {
		l[k] = v
	}
	return l
}

// endpoint is one resolved end of a connection: a pod, or a bare IP.
type endpoint struct {
	pod  *corev1.Pod // nil for an IP
	ip   netip.Addr  // pod IP or the bare IP; invalid when a pod has none yet
	name string      // "Pod/ns/name" or "IP 1.2.3.4"
}

func (e endpoint) isPod() bool { return e.pod != nil }

// Check answers one netpol.check request.
func (s *Snapshot) Check(req *protocol.NetpolCheckRequest) (*protocol.NetpolResult, error) {
	proto := corev1.Protocol(cmp.Or(strings.ToUpper(req.Protocol), "TCP"))
	from, err := s.source(req.From)
	if err != nil {
		return nil, err
	}
	targets, port, notes, err := s.destinations(req.To, req.Port, proto)
	if err != nil {
		return nil, err
	}
	res := &protocol.NetpolResult{Allowed: len(targets) > 0, From: from.name, Port: req.Port, Protocol: string(proto), Notes: notes}
	if from.isPod() && from.pod.Spec.HostNetwork {
		res.Notes = append(res.Notes, from.name+" uses the host network: policies see it as its node's IP, and most CNIs do not apply pod policies to it")
	}
	for _, to := range targets {
		p := protocol.NetpolPath{To: to.name, Port: port(to)}
		p.Egress = s.side(from, to, p.Port, proto, networkingv1.PolicyTypeEgress)
		p.Ingress = s.side(to, from, p.Port, proto, networkingv1.PolicyTypeIngress)
		p.Allowed = p.Egress.Allowed && p.Ingress.Allowed
		res.Allowed = res.Allowed && p.Allowed
		res.Paths = append(res.Paths, p)
	}
	if len(targets) == 0 {
		res.Notes = append(res.Notes, "no destination: the Service has no pods or endpoints")
	}
	res.Summary = summary(res)
	return res, nil
}

// side evaluates one end: egress at self (towards peer) or ingress at self
// (from peer). port is the destination port, already resolved for self or
// peer as the destination.
func (s *Snapshot) side(self, peer endpoint, port int32, proto corev1.Protocol, dir networkingv1.PolicyType) protocol.NetpolSide {
	out := protocol.NetpolSide{Direction: strings.ToLower(string(dir))}
	if !self.isPod() {
		out.Allowed = true
		out.Decision = "not a pod: no policy applies at this end"
		return out
	}
	if self.pod.Spec.HostNetwork {
		out.Allowed = true
		out.Decision = "host-network pod: pod policies do not apply at this end"
		return out
	}
	dst := self // the destination pod, for named ports
	if dir == networkingv1.PolicyTypeEgress {
		dst = peer
	}
	for _, pol := range s.Policies {
		if pol.Namespace != self.pod.Namespace || !hasType(pol, dir) || !selects(pol.Spec.PodSelector, self.pod.Labels) {
			continue
		}
		out.Isolated = true
		name := pol.Namespace + "/" + pol.Name
		out.Selecting = append(out.Selecting, name)
		for i, r := range rules(pol, dir) {
			if !s.peersMatch(r.peers, pol.Namespace, peer) {
				continue
			}
			if !portsMatch(r.ports, port, proto, dst.pod) {
				continue
			}
			out.Allowed = true
			out.AllowedBy = append(out.AllowedBy, protocol.NetpolRule{Policy: name, Rule: i, Detail: r.describe(dir)})
		}
	}
	switch {
	case !out.Isolated:
		out.Allowed = true
		out.Decision = "no policy selects this pod for " + out.Direction + ": allowed"
	case out.Allowed:
		out.Decision = "allowed by " + out.AllowedBy[0].Policy + fmt.Sprintf(" (%s rule %d)", out.Direction, out.AllowedBy[0].Rule)
	default:
		out.Decision = fmt.Sprintf("blocked: isolated by %s and no %s rule matches", strings.Join(out.Selecting, ", "), out.Direction)
	}
	return out
}

// rule is one ingress or egress rule, direction-neutral.
type rule struct {
	peers []networkingv1.NetworkPolicyPeer
	ports []networkingv1.NetworkPolicyPort
}

func (r rule) describe(dir networkingv1.PolicyType) string {
	who := "any peer"
	if len(r.peers) > 0 {
		var ps []string
		for _, p := range r.peers {
			ps = append(ps, describePeer(p))
		}
		who = strings.Join(ps, " or ")
	}
	what := "any port"
	if len(r.ports) > 0 {
		var ps []string
		for _, p := range r.ports {
			ps = append(ps, describePort(p))
		}
		what = strings.Join(ps, ", ")
	}
	if dir == networkingv1.PolicyTypeEgress {
		return "to " + who + " on " + what
	}
	return "from " + who + " on " + what
}

func rules(pol *networkingv1.NetworkPolicy, dir networkingv1.PolicyType) []rule {
	var out []rule
	if dir == networkingv1.PolicyTypeIngress {
		for _, r := range pol.Spec.Ingress {
			out = append(out, rule{r.From, r.Ports})
		}
	} else {
		for _, r := range pol.Spec.Egress {
			out = append(out, rule{r.To, r.Ports})
		}
	}
	return out
}

// hasType applies the policyTypes default: Ingress always, Egress when the
// policy has egress rules.
func hasType(pol *networkingv1.NetworkPolicy, dir networkingv1.PolicyType) bool {
	if len(pol.Spec.PolicyTypes) > 0 {
		return slices.Contains(pol.Spec.PolicyTypes, dir)
	}
	return dir == networkingv1.PolicyTypeIngress || len(pol.Spec.Egress) > 0
}

// selects matches a label selector; a bad selector matches nothing.
func selects(sel metav1.LabelSelector, l map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(&sel)
	return err == nil && s.Matches(labels.Set(l))
}

func (s *Snapshot) peersMatch(peers []networkingv1.NetworkPolicyPeer, policyNS string, peer endpoint) bool {
	if len(peers) == 0 {
		return true
	}
	for _, p := range peers {
		if s.peerMatches(p, policyNS, peer) {
			return true
		}
	}
	return false
}

func (s *Snapshot) peerMatches(p networkingv1.NetworkPolicyPeer, policyNS string, peer endpoint) bool {
	if p.IPBlock != nil {
		return peer.ip.IsValid() && inBlock(p.IPBlock, peer.ip)
	}
	// Selectors match pods only; a host-network pod counts as its node.
	if !peer.isPod() || peer.pod.Spec.HostNetwork {
		return false
	}
	if p.NamespaceSelector == nil {
		if peer.pod.Namespace != policyNS {
			return false
		}
	} else if !selects(*p.NamespaceSelector, s.Namespaces[peer.pod.Namespace]) {
		return false
	}
	return p.PodSelector == nil || selects(*p.PodSelector, peer.pod.Labels)
}

func inBlock(b *networkingv1.IPBlock, ip netip.Addr) bool {
	cidr, err := netip.ParsePrefix(b.CIDR)
	if err != nil || !cidr.Contains(ip) {
		return false
	}
	for _, e := range b.Except {
		if ex, err := netip.ParsePrefix(e); err == nil && ex.Contains(ip) {
			return false
		}
	}
	return true
}

// portsMatch checks the destination port against a rule's ports. A named
// port is looked up on the destination pod; a bare IP has no names.
func portsMatch(ports []networkingv1.NetworkPolicyPort, port int32, proto corev1.Protocol, dst *corev1.Pod) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if cmp.Or(ptrProto(p.Protocol), corev1.ProtocolTCP) != proto {
			continue
		}
		if p.Port == nil {
			return true // all ports of this protocol
		}
		want, ok := int32(0), false
		if p.Port.Type == intstr.Int {
			want, ok = p.Port.IntVal, true
		} else if dst != nil {
			want, ok = namedPort(dst, p.Port.StrVal, proto)
		}
		if !ok {
			continue
		}
		if port == want || (p.EndPort != nil && port >= want && port <= *p.EndPort) {
			return true
		}
	}
	return false
}

func ptrProto(p *corev1.Protocol) corev1.Protocol {
	if p == nil {
		return ""
	}
	return *p
}

func namedPort(pod *corev1.Pod, name string, proto corev1.Protocol) (int32, bool) {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == name && cmp.Or(p.Protocol, corev1.ProtocolTCP) == proto {
				return p.ContainerPort, true
			}
		}
	}
	return 0, false
}

func describePeer(p networkingv1.NetworkPolicyPeer) string {
	var parts []string
	if p.IPBlock != nil {
		s := "ipBlock " + p.IPBlock.CIDR
		if len(p.IPBlock.Except) > 0 {
			s += " except " + strings.Join(p.IPBlock.Except, ",")
		}
		return s
	}
	if p.NamespaceSelector != nil {
		parts = append(parts, "namespaces "+describeSelector(*p.NamespaceSelector))
	}
	if p.PodSelector != nil {
		parts = append(parts, "pods "+describeSelector(*p.PodSelector))
	}
	return strings.Join(parts, " / ")
}

func describeSelector(sel metav1.LabelSelector) string {
	s, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return "(invalid selector)"
	}
	if s.Empty() {
		return "(all)"
	}
	return s.String()
}

func describePort(p networkingv1.NetworkPolicyPort) string {
	proto := cmp.Or(ptrProto(p.Protocol), corev1.ProtocolTCP)
	if p.Port == nil {
		return "all " + string(proto)
	}
	s := p.Port.String()
	if p.EndPort != nil {
		s += fmt.Sprintf("-%d", *p.EndPort)
	}
	return s + "/" + string(proto)
}

func summary(r *protocol.NetpolResult) string {
	var blocked []string
	for _, p := range r.Paths {
		switch {
		case !p.Egress.Allowed:
			blocked = append(blocked, fmt.Sprintf("%s: egress %s", p.To, p.Egress.Decision))
		case !p.Ingress.Allowed:
			blocked = append(blocked, fmt.Sprintf("%s: ingress %s", p.To, p.Ingress.Decision))
		}
	}
	switch {
	case len(r.Paths) == 0:
		return "no destination to check"
	case len(blocked) == 0:
		return fmt.Sprintf("allowed: %s → %d destination(s) on %d/%s", r.From, len(r.Paths), r.Port, r.Protocol)
	case len(blocked) == len(r.Paths):
		return "blocked: " + strings.Join(blocked, "; ")
	}
	return fmt.Sprintf("partly blocked (%d of %d destinations): %s", len(blocked), len(r.Paths), strings.Join(blocked, "; "))
}

// Isolating returns the policies ("namespace/name") that select pod for a
// direction (networkingv1.PolicyTypeIngress or Egress). An empty result
// means the pod is not isolated: all traffic that way is allowed.
func (s *Snapshot) Isolating(pod *corev1.Pod, dir networkingv1.PolicyType) []string {
	var out []string
	for _, pol := range s.Policies {
		if pol.Namespace == pod.Namespace && hasType(pol, dir) && selects(pol.Spec.PodSelector, pod.Labels) {
			out = append(out, pol.Namespace+"/"+pol.Name)
		}
	}
	return out
}

// Selected returns the pods a policy applies to: those in its namespace that
// its pod selector matches (KM-35).
func (s *Snapshot) Selected(pol *networkingv1.NetworkPolicy) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range s.Pods {
		if p.Namespace == pol.Namespace && selects(pol.Spec.PodSelector, p.Labels) {
			out = append(out, p)
		}
	}
	return out
}
