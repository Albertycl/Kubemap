package traces

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

func fixture(t *testing.T, name string) []Span {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The fixtures are real Tempo 2.9 answers from the lab cluster
// (2026-09-28): 200 kubelet probe spans of flask-vue-backend, one request
// through Traefik, and no client spans (the backend sends none).
func TestParseRealTempo(t *testing.T) {
	probes := fixture(t, "tempo-server-spans.json")
	if len(probes) != 200 {
		t.Fatalf("%d spans", len(probes))
	}
	p := probes[0]
	if p.Kind != "server" || p.Service != "flask-vue-backend" || p.PeerIP != "192.168.32.36" || p.Host != "192.168.41.138" ||
		!strings.HasPrefix(p.UserAgent, "kube-probe/") || p.Start.IsZero() {
		t.Errorf("probe span = %+v", p)
	}
	tr := fixture(t, "tempo-server-traefik.json")
	if len(tr) != 1 || tr[0].PeerIP != "192.168.41.145" || tr[0].Host != "flask-vue.albertycliu.xyz" || tr[0].UserAgent != "curl/8.5.0" {
		t.Errorf("traefik span = %+v", tr)
	}
	if c := fixture(t, "tempo-client-spans.json"); len(c) != 0 {
		t.Errorf("client spans = %+v", c)
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Error("bad JSON must fail")
	}
}

type lister map[schema.GroupVersionResource][]runtime.Object

func (l lister) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return l[g], nil
}

var yes = true

func pod(ns, name, ip, rs string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Status: corev1.PodStatus{PodIP: ip}}
	if rs != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs, Controller: &yes}}
	}
	return p
}

func rs(ns, name, deploy string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deploy, Controller: &yes}}}}
}

func svc(ns, name, ip string, ports ...int32) *corev1.Service {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{ClusterIPs: []string{ip}}}
	for _, p := range ports {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Port: p, Protocol: corev1.ProtocolTCP})
	}
	return s
}

// lab is k8server's objects around flask-vue and url-shortener.
func lab() lister {
	return lister{
		podsGVR: {
			pod("kube-system", "traefik-5546cdbbd4-pgl5g", "192.168.41.145", "traefik-5546cdbbd4"),
			pod("sre-lab-app", "flask-vue-backend-6688dd7cbf-6tspt", "192.168.41.138", "flask-vue-backend-6688dd7cbf"),
			pod("sre-lab-app", "flask-vue-frontend-7c9cf766d4-fdfk5", "192.168.41.183", "flask-vue-frontend-7c9cf766d4"),
			pod("sre-lab-app", "url-shortener-1", "192.168.41.150", "url-shortener-5d9"),
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "calico-system", Name: "calico-node-x"}, Spec: corev1.PodSpec{HostNetwork: true}, Status: corev1.PodStatus{PodIP: "192.168.32.36"}},
		},
		replicaSetsGVR: {
			rs("kube-system", "traefik-5546cdbbd4", "traefik"), rs("sre-lab-app", "flask-vue-backend-6688dd7cbf", "flask-vue-backend"),
			rs("sre-lab-app", "flask-vue-frontend-7c9cf766d4", "flask-vue-frontend"), rs("sre-lab-app", "url-shortener-5d9", "url-shortener"),
		},
		nodesGVR: {&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k8server"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.32.36"}}}}},
		servicesGVR: {
			svc("sre-lab-app", "flask-vue-backend", "10.43.1.1", 5000), svc("sre-lab-app", "flask-vue-frontend", "10.43.1.2", 80),
			svc("sre-lab-app", "url-shortener-pooler", "10.43.1.3", 5432), svc("sre-lab-app", "redis-master", "10.43.1.4", 6379),
			svc("observability", "tempo", "10.43.244.164", 6831, 3200, 4317),
		},
	}
}

func comp(kind, ns, name string, routes ...protocol.MapRoute) protocol.MapComponent {
	return protocol.MapComponent{ID: kind + "/" + ns + "/" + name, Kind: kind, Namespace: ns, Name: name, Routes: routes}
}

// labMap is the map the agent draws for these apps (trimmed).
func labMap() *protocol.AppMap {
	route := "IngressRoute.traefik.io/sre-lab-app/flask-vue"
	return &protocol.AppMap{
		Apps: []protocol.MapApp{
			{ID: "argocd/flask-vue", Components: []protocol.MapComponent{
				comp("IngressRoute.traefik.io", "sre-lab-app", "flask-vue",
					protocol.MapRoute{Host: "flask-vue.albertycliu.xyz", Path: "/api", Service: "Service/sre-lab-app/flask-vue-backend"},
					protocol.MapRoute{Host: "flask-vue.albertycliu.xyz", Path: "/", Service: "Service/sre-lab-app/flask-vue-frontend"}),
				comp("Deployment", "sre-lab-app", "flask-vue-backend"), comp("Deployment", "sre-lab-app", "flask-vue-frontend"),
			}},
			{ID: "argocd/url-shortener", Components: []protocol.MapComponent{
				comp("Deployment", "sre-lab-app", "url-shortener"),
				comp("Pooler.postgresql.cnpg.io", "sre-lab-app", "url-shortener-pooler"),
				comp("StatefulSet", "sre-lab-app", "redis-master"),
			}},
		},
		Links: []protocol.MapLink{
			{From: route, To: "Deployment/sre-lab-app/flask-vue-backend", Via: []string{"Service/sre-lab-app/flask-vue-backend"}},
			{From: route, To: "Deployment/sre-lab-app/flask-vue-frontend", Via: []string{"Service/sre-lab-app/flask-vue-frontend"}},
			{From: "Deployment/sre-lab-app/url-shortener", To: "Pooler.postgresql.cnpg.io/sre-lab-app/url-shortener-pooler",
				Via: []string{"Service/sre-lab-app/url-shortener-pooler"}},
			{From: "Deployment/sre-lab-app/url-shortener", To: "StatefulSet/sre-lab-app/redis-master", Via: []string{"Service/sre-lab-app/redis-master"}},
		},
	}
}

var now = time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

// The lab today: 200 probes and one request through Traefik. The probes
// confirm nothing; the request confirms the /api route to the backend.
func TestConfirmLab(t *testing.T) {
	x, err := NewIndex(lab())
	if err != nil {
		t.Fatal(err)
	}
	m := labMap()
	spans := append(fixture(t, "tempo-server-spans.json"), fixture(t, "tempo-server-traefik.json")...)
	Confirm(m, spans, x, "tempo http://tempo.observability.svc:3200", time.Hour, now)
	s := m.Traces
	if s.Spans != 201 || s.Probes != 200 || s.Confirmed != 1 || len(s.Unmatched) != 0 || s.Window != "1h0m0s" || !s.At.Equal(now) {
		t.Errorf("summary = %+v", s)
	}
	back := m.Links[0].Traces
	if back == nil || back.Spans != 1 || back.Detail != "1 server spans in flask-vue-backend for Host flask-vue.albertycliu.xyz" || back.LastSeen.IsZero() {
		t.Errorf("route → backend = %+v", back)
	}
	for _, l := range m.Links[1:] {
		if l.Traces != nil {
			t.Errorf("%s → %s confirmed without a call: %+v", l.From, l.To, l.Traces)
		}
	}
	// Probes alone confirm nothing.
	m = labMap()
	Confirm(m, fixture(t, "tempo-server-spans.json"), x, "tempo", time.Hour, now)
	if m.Traces.Confirmed != 0 || m.Traces.Probes != 200 {
		t.Errorf("probes only: %+v", m.Traces)
	}
}

// OTel client spans (url-shortener as instrumented apps send them):
// targets by Service name, DNS name or ClusterIP.
func TestConfirmClientSpans(t *testing.T) {
	x, _ := NewIndex(lab())
	m := labMap()
	cl := func(target string) Span {
		return Span{Kind: "client", Service: "url-shortener", Target: target, Start: now.Add(-time.Minute)}
	}
	spans := []Span{
		cl("url-shortener-pooler"), cl("url-shortener-pooler"),
		cl("redis-master.sre-lab-app.svc.cluster.local"),
		cl("10.43.1.3"), // the pooler's ClusterIP
		cl("api.stripe.com"),
		// A server span in the backend from url-shortener's pod: a call the
		// map has no link for.
		{Kind: "server", Service: "flask-vue-backend", PeerIP: "192.168.41.150", Host: "flask-vue-backend", Start: now},
	}
	Confirm(m, spans, x, "tempo", time.Hour, now)
	if l := m.Links[2].Traces; l == nil || l.Spans != 3 || !strings.Contains(l.Detail, "client spans in url-shortener to url-shortener-pooler") {
		t.Errorf("pooler = %+v", l)
	}
	if l := m.Links[3].Traces; l == nil || l.Spans != 1 {
		t.Errorf("redis = %+v", l)
	}
	want := []string{
		"Deployment/sre-lab-app/url-shortener calls Deployment/sre-lab-app/flask-vue-backend (server spans): no link on the map",
		"Deployment/sre-lab-app/url-shortener calls api.stripe.com (client spans): no link on the map",
	}
	if strings.Join(m.Traces.Unmatched, "|") != strings.Join(want, "|") || m.Traces.Confirmed != 2 {
		t.Errorf("summary = %+v", m.Traces)
	}
}

// A service.name that names two workloads, or none, confirms nothing.
func TestConfirmNoGuess(t *testing.T) {
	x, _ := NewIndex(lab())
	m := labMap()
	m.Apps = append(m.Apps, protocol.MapApp{ID: "other", Components: []protocol.MapComponent{comp("Deployment", "prod", "url-shortener")}})
	Confirm(m, []Span{{Kind: "client", Service: "url-shortener", Target: "url-shortener-pooler"},
		{Kind: "client", Service: "unknown-svc", Target: "redis-master"}}, x, "tempo", time.Hour, now)
	if m.Traces.Confirmed != 0 || len(m.Traces.Unmatched) != 0 {
		t.Errorf("ambiguous: %+v", m.Traces)
	}
	// With the namespace from the resource attributes, it is not ambiguous.
	m.Traces = nil
	Confirm(m, []Span{{Kind: "client", Service: "url-shortener", Namespace: "sre-lab-app", Target: "url-shortener-pooler"}}, x, "tempo", time.Hour, now)
	if m.Traces.Confirmed != 1 {
		t.Errorf("with namespace: %+v", m.Traces)
	}
}

func TestDiscover(t *testing.T) {
	if got := Discover(lab()); got != "http://tempo.observability.svc:3200" {
		t.Errorf("discover = %q", got)
	}
	l := lab()
	l[servicesGVR] = []runtime.Object{svc("observability", "tempo", "10.43.0.9", 4317)} // no HTTP port
	if got := Discover(l); got != "" {
		t.Errorf("without port 3200 = %q", got)
	}
	labelled := svc("tracing", "traces", "10.43.0.8", 3200)
	labelled.Labels = map[string]string{"app.kubernetes.io/name": "tempo"}
	l[servicesGVR] = []runtime.Object{labelled, svc("x", "other", "10.43.0.7", 3200)}
	if got := Discover(l); got != "http://traces.tracing.svc:3200" {
		t.Errorf("by label = %q", got)
	}
}

// The client sends GET searches only, keeps answers for a minute, and
// reports Tempo's errors.
func TestClient(t *testing.T) {
	body, _ := os.ReadFile("testdata/tempo-server-traefik.json")
	var calls atomic.Int32
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/search" || q.Get("limit") != "200" || q.Get("start") == "" ||
			!strings.Contains(q.Get("q"), "kind = ") {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		w.WriteHeader(status)
		if strings.Contains(q.Get("q"), "kind = server") {
			w.Write(body)
		} else {
			w.Write([]byte(`{"traces":[]}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL + "/")
	clock := now
	c.Now = func() time.Time { return clock }
	spans, err := c.Spans(context.Background())
	if err != nil || len(spans) != 1 || calls.Load() != 2 {
		t.Fatalf("spans %d, err %v, calls %d", len(spans), err, calls.Load())
	}
	if _, _ = c.Spans(context.Background()); calls.Load() != 2 {
		t.Error("a second call within a minute must use the cache")
	}
	clock = clock.Add(2 * time.Minute)
	status = http.StatusBadGateway
	if _, err := c.Spans(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("tempo error: %v", err)
	}
}

// A narrowed map keeps its own confirmations and unexplained calls.
func TestNarrowed(t *testing.T) {
	x, _ := NewIndex(lab())
	m := labMap()
	spans := append(fixture(t, "tempo-server-traefik.json"),
		Span{Kind: "client", Service: "url-shortener", Target: "api.stripe.com", Start: now})
	Confirm(m, spans, x, "tempo", time.Hour, now)
	flask := &protocol.AppMap{Apps: m.Apps[:1], Links: m.Links[:2]}
	n := Narrowed(m.Traces, flask)
	if n.Confirmed != 1 || len(n.Unmatched) != 0 || n.Spans != m.Traces.Spans {
		t.Errorf("flask-vue: %+v", n)
	}
	url := &protocol.AppMap{Apps: m.Apps[1:], Links: m.Links[2:]}
	if n := Narrowed(m.Traces, url); n.Confirmed != 0 || len(n.Unmatched) != 1 {
		t.Errorf("url-shortener: %+v", n)
	}
	if Narrowed(nil, url) != nil {
		t.Error("no summary stays none")
	}
}
