package svccheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubemap/pkg/protocol"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	l, ok := f[g]
	if !ok {
		return nil, os.ErrNotExist
	}
	return l, nil
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

// lab is the lab cluster's kubemap-demo, sre-lab-app, default, kube-system
// and monitoring namespaces, exported read-only on 2026-09-28.
func lab(t *testing.T) *Snapshot {
	t.Helper()
	s, err := Load(fakeSource{
		servicesGVR: load[corev1.Service](t, "services.json"),
		podsGVR:     load[corev1.Pod](t, "pods.json"),
		slicesGVR:   load[discoveryv1.EndpointSlice](t, "endpointslices.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func find(t *testing.T, r *protocol.ServiceCheckResult, id string) protocol.ServiceStatus {
	t.Helper()
	for _, s := range r.Services {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no %s in result", id)
	return protocol.ServiceStatus{}
}

func TestLabCluster(t *testing.T) {
	r := lab(t).Check("", "")
	for _, s := range r.Services {
		t.Logf("%-60s healthy=%v matched=%d ready=%d eps=%d %v", s.ID, s.Healthy, s.PodsMatched, s.PodsReady, s.ReadyEndpoints, s.Findings)
	}
	// The demo's badsvc: a selector typo.
	c := find(t, r, "Service/kubemap-demo/catalog")
	if c.Healthy || len(c.Findings) != 1 || c.Findings[0].Problem != "selector matches no pods" {
		t.Fatalf("catalog = %+v", c)
	}
	if d := c.Findings[0].Detail; !strings.Contains(d, `a typo? "catalog-api" vs "catalog"`) {
		t.Errorf("catalog detail = %s", d)
	}
	if cmd := c.Findings[0].Command; !strings.Contains(cmd, "kubectl -n kubemap-demo get pods -l app.kubernetes.io/name=catalog-api") {
		t.Errorf("catalog command = %s", cmd)
	}
	// The netpol scenario's api Service is fine: the policy is the problem,
	// not the Service.
	if a := find(t, r, "Service/kubemap-demo/api"); !a.Healthy || a.PodsReady != 1 || a.ReadyEndpoints != 1 {
		t.Errorf("api = %+v", a)
	}
	// The orphaned flask-vue Services in default: their pods run in
	// sre-lab-app.
	f := find(t, r, "Service/default/flask-vue-backend")
	if f.Healthy || !strings.Contains(f.Findings[0].Detail, "Pods in other namespaces match (sre-lab-app/flask-vue-backend-") {
		t.Errorf("default/flask-vue-backend = %+v", f)
	}
	// url-shortener: named targetPort resolves; healthy.
	if u := find(t, r, "Service/sre-lab-app/url-shortener"); !u.Healthy || u.PodsReady != 2 {
		t.Errorf("url-shortener = %+v", u)
	}
	// Problems sort first.
	if r.Services[0].Healthy || r.Problems == 0 {
		t.Errorf("first = %+v, problems %d", r.Services[0], r.Problems)
	}
}

func TestNamespaceAndName(t *testing.T) {
	s := lab(t)
	r := s.Check("kubemap-demo", "")
	for _, x := range r.Services {
		if x.Namespace != "kubemap-demo" {
			t.Errorf("other namespace: %s", x.ID)
		}
	}
	if r := s.Check("kubemap-demo", "catalog"); len(r.Services) != 1 || r.Problems != 1 {
		t.Errorf("one Service: %+v", r)
	}
}

func pod(name string, lbl map[string]string, ready bool, ports ...corev1.ContainerPort) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: lbl},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: ports}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}

func svc(name string, sel map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name}, Spec: corev1.ServiceSpec{Selector: sel, Ports: ports}}
}

func slice(svc string, ready ...bool) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: svc + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: svc}}}
	for _, r := range ready {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &r}})
	}
	return es
}

func problems(s protocol.ServiceStatus) []string {
	var out []string
	for _, f := range s.Findings {
		out = append(out, f.Severity+" "+f.Problem)
	}
	return out
}

func TestRules(t *testing.T) {
	web := map[string]string{"app": "web"}
	http := corev1.ContainerPort{Name: "http", ContainerPort: 8080}
	for _, tc := range []struct {
		name   string
		svc    *corev1.Service
		pods   []*corev1.Pod
		slices []*discoveryv1.EndpointSlice
		want   []string
	}{
		{"healthy, named port", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")}),
			[]*corev1.Pod{pod("a", web, true, http)}, []*discoveryv1.EndpointSlice{slice("s", true)}, nil},
		{"no ready pods", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")}),
			[]*corev1.Pod{pod("a", web, false, http), pod("b", web, false, http)}, []*discoveryv1.EndpointSlice{slice("s", false, false)},
			[]string{"critical no ready pods"}},
		{"some not ready", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")}),
			[]*corev1.Pod{pod("a", web, true, http), pod("b", web, false, http)}, []*discoveryv1.EndpointSlice{slice("s", true, false)},
			[]string{"warning some pods not ready"}},
		{"named targetPort nowhere", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("web")}),
			[]*corev1.Pod{pod("a", web, true, http)}, []*discoveryv1.EndpointSlice{slice("s")},
			[]string{"critical named targetPort not declared", "warning EndpointSlices show no ready endpoint"}},
		{"named targetPort on some pods", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")}),
			[]*corev1.Pod{pod("a", web, true, http), pod("b", web, true, corev1.ContainerPort{ContainerPort: 8080})}, []*discoveryv1.EndpointSlice{slice("s", true)},
			[]string{"warning named targetPort missing on some pods"}},
		{"numeric targetPort not declared", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(9090)}),
			[]*corev1.Pod{pod("a", web, true, http)}, []*discoveryv1.EndpointSlice{slice("s", true)},
			[]string{"warning targetPort not declared"}},
		{"targetPort defaults to port, declared", svc("s", web, corev1.ServicePort{Port: 8080}),
			[]*corev1.Pod{pod("a", web, true, http)}, []*discoveryv1.EndpointSlice{slice("s", true)}, nil},
		{"UDP port against TCP declaration", svc("s", web, corev1.ServicePort{Port: 8080, Protocol: corev1.ProtocolUDP}),
			[]*corev1.Pod{pod("a", web, true, http)}, []*discoveryv1.EndpointSlice{slice("s", true)},
			[]string{"warning targetPort not declared"}},
		{"no ports declared: unknown, no finding", svc("s", web, corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(9090)}),
			[]*corev1.Pod{pod("a", web, true)}, []*discoveryv1.EndpointSlice{slice("s", true)}, nil},
		{"selector-less without endpoints", svc("s", nil, corev1.ServicePort{Port: 80}), nil, nil,
			[]string{"critical no endpoints"}},
		{"selector-less with endpoints", svc("s", nil, corev1.ServicePort{Port: 80}), nil, []*discoveryv1.EndpointSlice{slice("s", true)}, nil},
		{"ExternalName is not checked", &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "s"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "db.example.com"}}, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{Services: []*corev1.Service{tc.svc}, Pods: tc.pods, Slices: tc.slices, SlicesKnown: true}
			r := s.Check("", "")
			got := problems(r.Services[0])
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("findings = %v, want %v (%+v)", got, tc.want, r.Services[0].Findings)
			}
			if r.Services[0].Healthy != (len(tc.want) == 0) {
				t.Errorf("healthy = %v", r.Services[0].Healthy)
			}
			for _, f := range r.Services[0].Findings {
				if f.Detail == "" || f.Fix == "" || f.Command == "" {
					t.Errorf("incomplete finding %+v", f)
				}
			}
		})
	}
}

func TestSlicesUnknownAndFinishedPods(t *testing.T) {
	web := map[string]string{"app": "web"}
	done := pod("old", web, false)
	done.Status.Phase = corev1.PodSucceeded
	s := &Snapshot{Services: []*corev1.Service{svc("s", web, corev1.ServicePort{Port: 80})}, Pods: []*corev1.Pod{done}}
	r := s.Check("", "")
	if st := r.Services[0]; st.ReadyEndpoints != -1 || st.PodsMatched != 0 || problems(st)[0] != "critical selector matches no pods" {
		t.Errorf("status = %+v", st)
	}
	// Load without EndpointSlices cached.
	s2, err := Load(fakeSource{servicesGVR: nil, podsGVR: nil})
	if err != nil || s2.SlicesKnown {
		t.Errorf("load = %+v, %v", s2, err)
	}
	if _, err := Load(fakeSource{}); err == nil {
		t.Error("no Services cached must fail")
	}
}

func TestSimilar(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"catalog", "catalog-api", true}, {"web", "webb", true}, {"api", "db", false}, {"x", "x", false}, {"frontend", "backend", false}} {
		if got := similar(tc.a, tc.b); got != tc.want {
			t.Errorf("similar(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// review-km-77 N-1: a Service that publishes not-ready addresses still has
// endpoints; unready pods are a pod problem, a warning for the Service.
func TestPublishNotReadyAddresses(t *testing.T) {
	web := map[string]string{"app": "db"}
	peers := svc("db-headless", web, corev1.ServicePort{Port: 5432})
	peers.Spec.PublishNotReadyAddresses = true
	s := &Snapshot{Services: []*corev1.Service{peers}, Pods: []*corev1.Pod{pod("db-0", web, false), pod("db-1", web, false)},
		Slices: []*discoveryv1.EndpointSlice{slice("db-headless", false, false)}, SlicesKnown: true}
	st := s.Check("", "").Services[0]
	if len(st.Findings) != 1 || st.Findings[0].Severity != "warning" || st.Findings[0].Problem != "no ready pods" ||
		!strings.Contains(st.Findings[0].Detail, "publishNotReadyAddresses") || st.Healthy {
		t.Errorf("all unready = %+v", st)
	}
	s.Pods[0] = pod("db-0", web, true)
	st = s.Check("", "").Services[0]
	if st.Findings[0].Problem != "some pods not ready" || !strings.Contains(st.Findings[0].Detail, "publishNotReadyAddresses") {
		t.Errorf("some unready = %+v", st)
	}
	// Without it, no ready pods is critical.
	peers.Spec.PublishNotReadyAddresses = false
	s.Pods[0] = pod("db-0", web, false)
	if st := s.Check("", "").Services[0]; st.Findings[0].Severity != "critical" || !strings.Contains(st.Findings[0].Detail, "The Service has no endpoints, so requests fail.") {
		t.Errorf("plain Service = %+v", st.Findings)
	}
}
