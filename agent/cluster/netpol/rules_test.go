package netpol

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubemap/pkg/protocol"
)

func mkPod(ns, name, ip string, lbl map[string]string, ports ...corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: ports}}},
		Status: corev1.PodStatus{PodIP: ip}}
}

func pol(ns, name string, spec networkingv1.NetworkPolicySpec) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
}

func sel(kv ...string) *metav1.LabelSelector {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return &metav1.LabelSelector{MatchLabels: m}
}

func np(port intstr.IntOrString, proto corev1.Protocol, end int32) networkingv1.NetworkPolicyPort {
	p := networkingv1.NetworkPolicyPort{Port: &port}
	if proto != "" {
		p.Protocol = &proto
	}
	if end != 0 {
		p.EndPort = &end
	}
	return p
}

// synthetic has one app namespace with a web and a db pod.
func synthetic(policies ...*networkingv1.NetworkPolicy) *Snapshot {
	return &Snapshot{
		Policies:   policies,
		Namespaces: map[string]map[string]string{"app": {"kubernetes.io/metadata.name": "app", "team": "a"}, "other": {"kubernetes.io/metadata.name": "other"}},
		Pods: []*corev1.Pod{
			mkPod("app", "web", "10.0.0.1", map[string]string{"app": "web"}),
			mkPod("app", "db", "10.0.0.2", map[string]string{"app": "db"}, corev1.ContainerPort{Name: "pg", ContainerPort: 5432}),
			mkPod("other", "tool", "10.0.1.1", map[string]string{"app": "tool"}),
			mkPod("app", "new", "", map[string]string{"app": "web"}), // no IP yet
		},
	}
}

func req(from, to protocol.NetpolEndpoint, port int32, proto string) *protocol.NetpolCheckRequest {
	return &protocol.NetpolCheckRequest{From: from, To: to, Port: port, Protocol: proto}
}

func mustCheck(t *testing.T, s *Snapshot, r *protocol.NetpolCheckRequest) *protocol.NetpolResult {
	t.Helper()
	res, err := s.Check(r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRuleSemantics(t *testing.T) {
	dbOnly := networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "db")}
	named := dbOnly
	named.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		From:  []networkingv1.NetworkPolicyPeer{{PodSelector: sel("app", "web")}},
		Ports: []networkingv1.NetworkPolicyPort{np(intstr.FromString("pg"), "", 0)},
	}}
	ranged := dbOnly
	ranged.Ingress = []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{np(intstr.FromInt32(5000), "", 5500)}}}
	udp := dbOnly
	udp.Ingress = []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{np(intstr.FromInt32(5432), corev1.ProtocolUDP, 0)}}}
	nsSel := dbOnly
	nsSel.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: sel("team", "a")}}}}
	both := dbOnly // namespaceSelector AND podSelector in one peer
	both.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}, PodSelector: sel("app", "tool")}}}}
	block := dbOnly
	block.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/16", Except: []string{"10.0.1.0/24"}}}}}}
	// Egress rules and no policyTypes: Egress is implied.
	implied := networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "web"),
		Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}}}
	badSel := networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "Bogus"}}}}

	web, db, tool := protocol.NetpolEndpoint{Namespace: "app", Pod: "web"}, protocol.NetpolEndpoint{Namespace: "app", Pod: "db"}, protocol.NetpolEndpoint{Namespace: "other", Pod: "tool"}
	for _, tc := range []struct {
		name    string
		spec    networkingv1.NetworkPolicySpec
		r       *protocol.NetpolCheckRequest
		allowed bool
	}{
		{"default deny (no rules)", dbOnly, req(web, db, 5432, ""), false},
		{"named port resolved on the destination", named, req(web, db, 5432, ""), true},
		{"named port, other number", named, req(web, db, 5433, ""), false},
		{"named port, wrong source", named, req(tool, db, 5432, ""), false},
		{"endPort range, inside", ranged, req(tool, db, 5432, ""), true},
		{"endPort range, outside", ranged, req(tool, db, 5501, ""), false},
		{"UDP rule, TCP traffic", udp, req(web, db, 5432, "TCP"), false},
		{"UDP rule, UDP traffic", udp, req(web, db, 5432, "UDP"), true},
		{"namespaceSelector matches the labelled namespace", nsSel, req(web, db, 1, ""), true},
		{"namespaceSelector, other namespace", nsSel, req(tool, db, 1, ""), false},
		{"namespace AND pod selector in one peer", both, req(tool, db, 1, ""), true},
		{"namespace AND pod selector, pod does not match", both, req(web, db, 1, ""), false},
		{"ipBlock matches a pod IP (CNI-dependent, Calico does)", block, req(web, db, 1, ""), true},
		{"ipBlock except", block, req(tool, db, 1, ""), false},
		{"ipBlock from an outside IP", block, req(protocol.NetpolEndpoint{IP: "10.0.9.9"}, db, 1, ""), true},
		{"implied Egress policyType", implied, req(web, db, 1, ""), true}, // 0.0.0.0/0 includes the db pod's IP
		{"implied Egress: IPv6 outside the block", implied, req(web, protocol.NetpolEndpoint{IP: "2001:db8::1"}, 1, ""), false},
		{"invalid selector selects nothing", badSel, req(web, db, 1, ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := synthetic(pol("app", "p", tc.spec))
			r := mustCheck(t, s, tc.r)
			if r.Allowed != tc.allowed {
				t.Errorf("allowed = %v, want %v: %s", r.Allowed, tc.allowed, r.Summary)
			}
		})
	}
}

func TestRulesAddUpAndDescribe(t *testing.T) {
	deny := pol("app", "deny-all", networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}})
	allow := pol("app", "allow-web", networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "db"),
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From:  []networkingv1.NetworkPolicyPeer{{PodSelector: sel("app", "web")}, {IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.0/24", Except: []string{"192.0.2.1/32"}}}},
			Ports: []networkingv1.NetworkPolicyPort{np(intstr.FromInt32(5432), "", 0), {}},
		}}})
	s := synthetic(allow, deny)
	s.Policies = []*networkingv1.NetworkPolicy{allow, deny}
	r := mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "app", Pod: "web"}, protocol.NetpolEndpoint{Namespace: "app", Pod: "db"}, 5432, ""))
	in := r.Paths[0].Ingress
	if !r.Allowed || len(in.Selecting) != 2 || in.AllowedBy[0].Policy != "app/allow-web" {
		t.Fatalf("ingress = %+v", in)
	}
	d := in.AllowedBy[0].Detail
	for _, want := range []string{"pods app=web", "ipBlock 192.0.2.0/24 except 192.0.2.1/32", "5432/TCP", "all TCP"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	// Egress description and the "(all)" selector.
	eg := pol("app", "eg", networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "web"),
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}, Ports: []networkingv1.NetworkPolicyPort{np(intstr.FromString("pg"), "", 0)}}}})
	r = mustCheck(t, synthetic(eg), req(protocol.NetpolEndpoint{Namespace: "app", Pod: "web"}, protocol.NetpolEndpoint{Namespace: "app", Pod: "db"}, 5432, ""))
	if e := r.Paths[0].Egress; !e.Allowed || !strings.HasPrefix(e.AllowedBy[0].Detail, "to namespaces (all) on pg/TCP") {
		t.Errorf("egress = %+v", e)
	}
	// Named egress port to a bare IP cannot match.
	r = mustCheck(t, synthetic(eg), req(protocol.NetpolEndpoint{Namespace: "app", Pod: "web"}, protocol.NetpolEndpoint{IP: "198.51.100.1"}, 5432, ""))
	if r.Allowed {
		t.Error("a named port must not match a bare IP")
	}
}

func TestPodWithoutIPAndEmptyService(t *testing.T) {
	block := pol("app", "b", networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "db"),
		Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}}})
	s := synthetic(block)
	// A pod without an IP yet cannot match an ipBlock.
	r := mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "app", Pod: "new"}, protocol.NetpolEndpoint{Namespace: "app", Pod: "db"}, 1, ""))
	if r.Allowed {
		t.Error("a pod without an IP matched an ipBlock")
	}
	s.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "empty"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "none"}, Ports: []corev1.ServicePort{{Port: 80}}}}}
	r = mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "app", Pod: "web"}, protocol.NetpolEndpoint{Namespace: "app", Service: "empty"}, 80, ""))
	if r.Allowed || len(r.Paths) != 0 || r.Summary != "no destination to check" {
		t.Errorf("empty Service: %+v", r)
	}
}

func TestFinishedPodsIgnored(t *testing.T) {
	s := synthetic()
	old := mkPod("app", "old", "10.0.0.5", map[string]string{"app": "web"})
	old.Status.Phase = corev1.PodFailed // evicted
	s.Pods = append(s.Pods, old)
	s.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80}}}}}
	r := mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "other", Pod: "tool"}, protocol.NetpolEndpoint{Namespace: "app", Service: "web"}, 80, ""))
	for _, p := range r.Paths {
		if p.To == "Pod/app/old" {
			t.Error("an evicted pod is not a Service backend")
		}
	}
	r = mustCheck(t, s, req(protocol.NetpolEndpoint{IP: "10.0.0.5"}, protocol.NetpolEndpoint{Namespace: "app", Pod: "db"}, 1, ""))
	if r.From != "IP 10.0.0.5" {
		t.Errorf("a finished pod still owns its IP: %s", r.From)
	}
}

func TestPartlyBlockedSummary(t *testing.T) {
	// Only one of two backends is reachable.
	p := pol("app", "only-db", networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "web"),
		Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: sel("app", "db")}}}}})
	s := synthetic(p)
	s.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "all"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{}, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}}}}}
	s.Services[0].Spec.Selector = nil
	// Selector-less and no slices: no destinations.
	r := mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "other", Pod: "tool"}, protocol.NetpolEndpoint{Namespace: "app", Service: "all"}, 80, ""))
	if len(r.Paths) != 0 {
		t.Errorf("paths = %+v", r.Paths)
	}
	s.Services[0].Spec.Selector = map[string]string{"app": "web"}
	s.Pods = append(s.Pods, mkPod("app", "web2", "10.0.0.9", map[string]string{"app": "web", "x": "y"}))
	p.Spec.PodSelector = *sel("x", "y")
	r = mustCheck(t, s, req(protocol.NetpolEndpoint{Namespace: "other", Pod: "tool"}, protocol.NetpolEndpoint{Namespace: "app", Service: "all"}, 80, ""))
	if r.Allowed || !strings.HasPrefix(r.Summary, "partly blocked (1 of 3 destinations)") || r.Paths[0].Port != 8080 {
		t.Errorf("summary = %q paths %+v", r.Summary, r.Paths)
	}
}

func TestIsolating(t *testing.T) {
	p := pol("app", "db-in", networkingv1.NetworkPolicySpec{PodSelector: *sel("app", "db")})
	s := synthetic(p)
	db, web := s.pod("app", "db"), s.pod("app", "web")
	if got := s.Isolating(db, networkingv1.PolicyTypeIngress); len(got) != 1 || got[0] != "app/db-in" {
		t.Errorf("db ingress = %v", got)
	}
	if got := s.Isolating(db, networkingv1.PolicyTypeEgress); got != nil {
		t.Errorf("db egress = %v", got)
	}
	if got := s.Isolating(web, networkingv1.PolicyTypeIngress); got != nil {
		t.Errorf("web ingress = %v", got)
	}
}
