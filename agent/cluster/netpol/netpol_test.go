package netpol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return f[g], nil
}

func load[T any, P interface {
	*T
	runtime.Object
}](t *testing.T, file string) []runtime.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []T }
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := make([]runtime.Object, len(list.Items))
	for i := range list.Items {
		out[i] = P(&list.Items[i])
	}
	return out
}

// lab is this cluster as it was exported read-only (2026-09-28): its 18
// NetworkPolicies, 14 namespaces, the running pods, Services and
// EndpointSlices, trimmed to the fields the engine reads.
func lab(t *testing.T) *Snapshot {
	t.Helper()
	src := fakeSource{
		policiesGVR:   load[networkingv1.NetworkPolicy](t, "networkpolicies.json"),
		namespacesGVR: load[corev1.Namespace](t, "namespaces.json"),
		podsGVR:       load[corev1.Pod](t, "pods.json"),
		servicesGVR:   load[corev1.Service](t, "services.json"),
		slicesGVR:     load[discoveryv1.EndpointSlice](t, "endpointslices.json"),
	}
	s, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Policies) != 18 {
		t.Fatalf("%d policies in testdata, want the cluster's 18", len(s.Policies))
	}
	return s
}

const (
	traefik   = "traefik-5546cdbbd4-pgl5g"
	backend   = "flask-vue-backend-6688dd7cbf-6tspt"
	frontend  = "flask-vue-frontend-7c9cf766d4-fdfk5"
	shortener = "url-shortener-6f89bdc657-rgwsv"
	redis     = "redis-master-0"
	db        = "url-shortener-db-2"
	whoami    = "whoami-76bcdb9fb7-4l48g"
	argoSrv   = "argocd-server-6799b86d7-vzjzf"
	coredns   = "coredns-5594bbd8f5-mbcjg"
	prom      = "prometheus-kube-prometheus-stack-prometheus-0"
	lab_ns    = "sre-lab-app"
)

func pod(ns, name string) protocol.NetpolEndpoint {
	return protocol.NetpolEndpoint{Namespace: ns, Pod: name}
}
func svc(ns, name string) protocol.NetpolEndpoint {
	return protocol.NetpolEndpoint{Namespace: ns, Service: name}
}
func ip(a string) protocol.NetpolEndpoint { return protocol.NetpolEndpoint{IP: a} }

func check(t *testing.T, s *Snapshot, from, to protocol.NetpolEndpoint, port int32, proto string) *protocol.NetpolResult {
	t.Helper()
	req := &protocol.NetpolCheckRequest{From: from, To: to, Port: port, Protocol: proto}
	r, err := s.Check(req)
	if err != nil {
		t.Fatalf("check %+v: %v", req, err)
	}
	return r
}

func TestLabCluster(t *testing.T) {
	s := lab(t)
	for _, tc := range []struct {
		name     string
		from, to protocol.NetpolEndpoint
		port     int32
		proto    string
		allowed  bool
		// want is a substring of the deciding side's Decision.
		side string // "egress" or "ingress": the side that decides
		want string
	}{
		{"traefik to flask-vue backend on 5000", pod("kube-system", traefik), pod(lab_ns, backend), 5000, "", true,
			"ingress", "allowed by sre-lab-app/allow-traefik-to-flask-vue-backend (ingress rule 0)"},
		{"traefik to backend on the wrong port", pod("kube-system", traefik), pod(lab_ns, backend), 80, "", false,
			"ingress", "blocked: isolated by sre-lab-app/allow-traefik-to-flask-vue-backend, sre-lab-app/default-deny-ingress"},
		{"frontend pod straight to backend: only Traefik may", pod(lab_ns, frontend), pod(lab_ns, backend), 5000, "", false,
			"ingress", "blocked"},
		{"url-shortener to Postgres", pod(lab_ns, shortener), pod(lab_ns, db), 5432, "", true,
			"ingress", "allowed by sre-lab-app/cnpg-allow-ingress (ingress rule 0)"},
		{"url-shortener to Redis through its Service (named targetPort)", pod(lab_ns, shortener), svc(lab_ns, "redis-master"), 6379, "", true,
			"ingress", "allowed by sre-lab-app/redis (ingress rule 0)"},
		{"url-shortener to argocd-server: deny-egress-to-argocd", pod(lab_ns, shortener), pod("argocd", argoSrv), 8080, "", false,
			"egress", "blocked: isolated by sre-lab-app/deny-egress-to-argocd"},
		{"url-shortener to DNS still works", pod(lab_ns, shortener), svc("kube-system", "kube-dns"), 53, "UDP", true,
			"egress", "allowed by sre-lab-app/deny-egress-to-argocd (egress rule 0)"},
		{"Prometheus scrapes url-shortener", pod("monitoring", prom), pod(lab_ns, shortener), 8080, "", true,
			"ingress", "allowed by sre-lab-app/allow-monitoring-to-url-shortener"},
		{"whoami from outside the cluster", ip("203.0.113.7"), pod(lab_ns, whoami), 80, "", false,
			"ingress", "blocked"},
		{"anyone to argocd-server (ingress: [{}])", pod(lab_ns, redis), pod("argocd", argoSrv), 8080, "", true,
			"ingress", "allowed by argocd/argocd-server-network-policy"},
		{"coredns has no policy", pod("argocd", argoSrv), pod("kube-system", coredns), 53, "UDP", true,
			"ingress", "no policy selects this pod for ingress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(t, s, tc.from, tc.to, tc.port, tc.proto)
			if r.Allowed != tc.allowed {
				t.Errorf("allowed = %v, want %v: %s", r.Allowed, tc.allowed, r.Summary)
			}
			for _, p := range r.Paths {
				d := p.Ingress.Decision
				if tc.side == "egress" {
					d = p.Egress.Decision
				}
				if !strings.Contains(d, tc.want) {
					t.Errorf("%s %s decision = %q, want %q", p.To, tc.side, d, tc.want)
				}
			}
			if r.Summary == "" || len(r.Paths) == 0 {
				t.Errorf("result = %+v", r)
			}
		})
	}
}

// The finding for the demo: deny-egress-to-argocd was meant to block one
// namespace, but its only egress rule is a namespaceSelector, and selectors
// match pods only. So every pod in sre-lab-app without another egress
// policy also loses the internet and the Kubernetes API server.
func TestDenyEgressToArgocdBlocksNonPodIPs(t *testing.T) {
	s := lab(t)
	for _, to := range []protocol.NetpolEndpoint{
		ip("1.1.1.1"),                // the internet
		svc("default", "kubernetes"), // the API server, a node IP behind a selector-less Service
		ip("10.43.0.1"),              // the same, by ClusterIP
	} {
		for _, from := range []string{shortener, backend, frontend, whoami} {
			r := check(t, s, pod(lab_ns, from), to, 443, "")
			if r.Allowed {
				t.Errorf("%s → %+v allowed; deny-egress-to-argocd should block it", from, to)
				continue
			}
			e := r.Paths[0].Egress
			if e.Allowed || len(e.Selecting) != 1 || e.Selecting[0] != "sre-lab-app/deny-egress-to-argocd" {
				t.Errorf("%s → %+v egress = %+v", from, to, e)
			}
		}
	}
	// Pods with their own allow-all egress policy are not affected.
	for _, from := range []string{redis, db} {
		if r := check(t, s, pod(lab_ns, from), ip("1.1.1.1"), 443, ""); !r.Allowed {
			t.Errorf("%s to the internet: %s", from, r.Summary)
		}
	}
	// The API server path is resolved through the selector-less Service.
	r := check(t, s, pod(lab_ns, backend), svc("default", "kubernetes"), 443, "")
	if len(r.Paths) != 1 || !strings.HasPrefix(r.Paths[0].To, "IP ") || r.Paths[0].Port != 6443 {
		t.Errorf("kubernetes Service paths = %+v", r.Paths)
	}
}

func TestServiceTargets(t *testing.T) {
	s := lab(t)
	// A Service port maps to the backends' targetPort: url-shortener 80 → 8080.
	r := check(t, s, pod("kube-system", traefik), svc(lab_ns, "url-shortener"), 80, "")
	if len(r.Paths) != 2 || r.Paths[0].Port != 8080 || !r.Allowed {
		t.Errorf("url-shortener Service: %s %+v", r.Summary, r.Paths)
	}
	// A ClusterIP resolves to the Service's backends, with a note.
	r = check(t, s, pod("kube-system", traefik), ip("10.43.219.240"), 80, "")
	if len(r.Paths) != 2 || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "ClusterIP of Service sre-lab-app/url-shortener") {
		t.Errorf("ClusterIP: %+v", r)
	}
	// A pod IP resolves to its pod.
	r = check(t, s, ip("192.168.41.145"), pod(lab_ns, backend), 5000, "")
	if r.From != "Pod/kube-system/"+traefik || !r.Allowed {
		t.Errorf("pod by IP: %+v", r)
	}
	// kube-dns 53/UDP and 53/TCP are different Service ports.
	r = check(t, s, pod(lab_ns, shortener), svc("kube-system", "kube-dns"), 53, "TCP")
	if len(r.Notes) != 0 || r.Protocol != "TCP" {
		t.Errorf("kube-dns TCP notes = %v", r.Notes)
	}
}

func TestHostNetworkAndErrors(t *testing.T) {
	s := lab(t)
	r := check(t, s, pod("monitoring", "kube-prometheus-stack-prometheus-node-exporter-zt5lv"), pod(lab_ns, shortener), 8080, "")
	if r.Allowed || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "host network") {
		t.Errorf("host-network source: %+v", r)
	}
	for _, req := range []*protocol.NetpolCheckRequest{
		{From: pod(lab_ns, "nope"), To: pod(lab_ns, backend), Port: 80},
		{From: pod(lab_ns, backend), To: svc(lab_ns, "nope"), Port: 80},
		{From: svc(lab_ns, "redis-master"), To: pod(lab_ns, backend), Port: 80},
	} {
		if _, err := s.Check(req); err == nil {
			t.Errorf("%+v: no error", req)
		}
	}
}
