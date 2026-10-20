package routes

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseRule(t *testing.T) {
	cases := []struct {
		rule string
		want []string // "hosts|kind path|other"
	}{
		{"Host(`a.example.com`)", []string{"a.example.com||"}},
		{"Host(`flask-vue.albertycliu.xyz`) && PathPrefix(`/api`)", []string{"flask-vue.albertycliu.xyz|PathPrefix /api|"}},
		{"Host(`a`, `b`)", []string{"a,b||"}}, // Traefik v2: several hosts
		{"Host(`a`) || Host(`b`)", []string{"a||", "b||"}},
		{"Host(`a`) && (PathPrefix(`/v1`) || PathPrefix(`/v2`))", []string{"a|PathPrefix /v1|", "a|PathPrefix /v2|"}},
		{"PathPrefix(`/metrics`)", []string{"|PathPrefix /metrics|"}},
		{"Host(`a`) && Path(`/exact`) && Method(`GET`)", []string{"a|Path /exact|Method(`GET`)"}},
		{"Host(`a`) && !PathPrefix(`/admin`)", []string{"a||!PathPrefix(`/admin`)"}},
		{"Host(\"a\") && PathRegexp(`^/v[0-9]+`)", []string{"a|PathRegexp ^/v[0-9]+|"}},
		{"Host(`a`) && !(Method(`POST`) || Method(`PUT`))", []string{"a||Not(`compound`)"}},
		{"HostRegexp(`{sub:[a-z]+}.example.com`)", []string{"||HostRegexp(`{sub:[a-z]+}.example.com`)"}},
	}
	for _, c := range cases {
		ms, err := ParseRule(c.rule)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", c.rule, err)
			continue
		}
		var got []string
		for _, m := range ms {
			path := ""
			if m.Path != "" {
				path = m.PathKind + " " + m.Path
			}
			got = append(got, strings.Join(m.Hosts, ",")+"|"+path+"|"+strings.Join(m.Other, ","))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("ParseRule(%q) = %q, want %q", c.rule, got, c.want)
		}
	}
	for _, bad := range []string{"Host(`a`", "Host(`a`) &&", "Host(a)", "Host(`a`) Host(`b`)", "`x", "Host(`a`) # b", "&& Host(`a`)", "Host"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q) accepted a broken rule", bad)
		}
	}
	// A rule that would expand to thousands of alternatives is refused.
	var parts []string
	for i := range 8 {
		parts = append(parts, fmt.Sprintf("(Host(`a%d`) || Host(`b%d`))", i, i))
	}
	if _, err := ParseRule(strings.Join(parts, " && ")); err == nil {
		t.Error("exponential rule accepted")
	}
}

func FuzzParseRule(f *testing.F) {
	f.Add("Host(`a`) && (PathPrefix(`/v1`) || !Method(`GET`))")
	f.Add("((((")
	f.Fuzz(func(t *testing.T, rule string) {
		_, _ = ParseRule(rule) // must never panic
	})
}

func ingressRoute(ns, name string, tls bool, routes ...map[string]any) *unstructured.Unstructured {
	rs := make([]any, len(routes))
	for i, r := range routes {
		rs[i] = r
	}
	spec := map[string]any{"entryPoints": []any{"web"}, "routes": rs}
	if tls {
		spec["entryPoints"] = []any{"websecure"}
		spec["tls"] = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"namespace": ns, "name": name}, "spec": spec}}
}

func svcRoute(match string, priority int64, svc string, port any, extra ...map[string]any) map[string]any {
	s := map[string]any{"name": svc, "port": port}
	for _, e := range extra {
		for k, v := range e {
			s[k] = v
		}
	}
	r := map[string]any{"match": match, "kind": "Rule", "services": []any{s}}
	if priority > 0 {
		r["priority"] = priority
	}
	return r
}

func TestFromIngressRoute(t *testing.T) {
	// The flask-vue route of the dev cluster.
	ir := ingressRoute("sre-lab-app", "flask-vue-http", false,
		svcRoute("Host(`flask-vue.albertycliu.xyz`) && PathPrefix(`/api`)", 100, "flask-vue-backend", int64(80)),
		svcRoute("Host(`flask-vue.albertycliu.xyz`)", 10, "flask-vue-frontend", int64(80)))
	rs := FromIngressRoute(ir, "IngressRoute.traefik.io")
	if len(rs) != 2 {
		t.Fatalf("routes = %+v", rs)
	}
	api, root := rs[0], rs[1]
	if api.Detail() != "flask-vue.albertycliu.xyz/api" || api.Service != "Service/sre-lab-app/flask-vue-backend" ||
		api.Port != "80" || api.Priority != 100 || api.TLS || !slices.Equal(api.EntryPoints, []string{"web"}) {
		t.Errorf("api route = %+v", api)
	}
	if root.Detail() != "flask-vue.albertycliu.xyz/" || root.Service != "Service/sre-lab-app/flask-vue-frontend" || root.Priority != 10 {
		t.Errorf("root route = %+v", root)
	}
	// Default priority is the rule length; middlewares, TLS, other
	// namespaces, TraefikService backends and named ports.
	ir = ingressRoute("a", "r", true,
		svcRoute("Host(`x`)", 0, "web", "http", map[string]any{"namespace": "b"}),
		svcRoute("Host(`y`)", 0, "canary", int64(80), map[string]any{"kind": "TraefikService"}))
	ir.Object["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)["middlewares"] = []any{
		map[string]any{"name": "redirect-to-https"}, map[string]any{"name": "auth", "namespace": "security"}}
	rs = FromIngressRoute(ir, "IngressRoute.traefik.io")
	x := rs[0]
	if x.Priority != len("Host(`x`)") || !x.TLS || x.Service != "Service/b/web" || x.Port != "http" ||
		!slices.Equal(x.Middlewares, []string{"redirect-to-https", "security/auth"}) {
		t.Errorf("x route = %+v", x)
	}
	if rs[1].Service != "TraefikService.traefik.io/a/canary" {
		t.Errorf("TraefikService backend = %s", rs[1].Service)
	}
	// A broken rule is kept, not dropped.
	ir = ingressRoute("a", "bad", false, svcRoute("Host(`x`", 0, "web", int64(80)))
	rs = FromIngressRoute(ir, "IngressRoute.traefik.io")
	if len(rs) != 1 || !strings.HasPrefix(rs[0].Other[0], "unparsed rule") {
		t.Errorf("broken rule = %+v", rs)
	}
}

func TestFromIngress(t *testing.T) {
	prefix, exact := networkingv1.PathTypePrefix, networkingv1.PathTypeExact
	in := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop"}, Spec: networkingv1.IngressSpec{
		TLS:            []networkingv1.IngressTLS{{Hosts: []string{"shop.example.com"}}},
		DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "fallback", Port: networkingv1.ServiceBackendPort{Name: "http"}}},
		Rules: []networkingv1.IngressRule{{Host: "shop.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{
				{Path: "/api", PathType: &prefix, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Number: 8080}}}},
				{Path: "/health", PathType: &exact, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Number: 8080}}}},
				{Path: "/files", Backend: networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "Bucket", Name: "b"}}},
			}}}}, {Host: "no-http.example.com"}},
	}}
	rs := FromIngress(in)
	var got []string
	for _, r := range rs {
		got = append(got, fmt.Sprintf("%s %s %s:%s tls=%v", r.Detail(), r.PathKind, r.Service, r.Port, r.TLS))
	}
	want := []string{
		"* (default backend) default Service/shop/fallback:http tls=false",
		"shop.example.com/api Prefix Service/shop/api:8080 tls=true",
		"shop.example.com/health Exact Service/shop/api:8080 tls=true",
	}
	if !slices.Equal(got, want) {
		t.Errorf("routes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	if objs, ok := f[g]; ok {
		return objs, nil
	}
	return nil, fmt.Errorf("not cached")
}

func slice(ns, svc string, ready ...bool) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svc + "-x",
		Labels: map[string]string{discoveryv1.LabelServiceName: svc}}}
	for _, r := range ready {
		r := r
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: &r}})
	}
	return es
}

func TestTableAndCheck(t *testing.T) {
	const ns = "sre-lab-app"
	f := fakeSource{
		irGVR(): {
			ingressRoute(ns, "flask-vue-http", false,
				svcRoute("Host(`flask-vue.albertycliu.xyz`) && PathPrefix(`/api`)", 100, "flask-vue-backend", int64(80)),
				svcRoute("Host(`flask-vue.albertycliu.xyz`)", 10, "flask-vue-frontend", int64(80))),
			// Two routes for the same host and path with different backends.
			ingressRoute(ns, "whoami-http", false, svcRoute("Host(`sre-lab.albertycliu.xyz`) && PathPrefix(`/whoami`)", 0, "whoami", int64(80))),
			ingressRoute(ns, "whoami-copy", false, svcRoute("Host(`sre-lab.albertycliu.xyz`) && PathPrefix(`/whoami`)", 0, "whoami-v2", int64(80))),
		},
		servicesGVR: {
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flask-vue-backend"}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flask-vue-frontend"}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "whoami"}},
		},
		endpointSlicesGVR: {
			slice(ns, "flask-vue-backend", true, false), // one ready, one not
			slice(ns, "flask-vue-frontend", false),      // none ready: 503
			slice(ns, "whoami", true),
			&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unlabelled"}},
		},
		ingressesGVR: {},
	}
	table := Table(f)
	var order []string
	for _, r := range table {
		order = append(order, r.Detail()+" → "+r.Service)
	}
	want := []string{
		"flask-vue.albertycliu.xyz/api → Service/sre-lab-app/flask-vue-backend",
		"flask-vue.albertycliu.xyz/ → Service/sre-lab-app/flask-vue-frontend",
		// Equal priority: ordered by source name, as Traefik breaks ties by
		// router name.
		"sre-lab.albertycliu.xyz/whoami → Service/sre-lab-app/whoami-v2",
		"sre-lab.albertycliu.xyz/whoami → Service/sre-lab-app/whoami",
	}
	if !slices.Equal(order, want) {
		t.Errorf("table order =\n%s", strings.Join(order, "\n"))
	}
	if table[0].ReadyEndpoints != 1 || table[1].ReadyEndpoints != 0 || !table[2].ServiceMissing || table[3].ReadyEndpoints != 1 {
		t.Errorf("checks: %+v", table)
	}
	var problems []string
	for _, fd := range Check(table) {
		problems = append(problems, fd.Route.Service+": "+fd.Problem)
	}
	wantP := []string{
		"Service/sre-lab-app/flask-vue-frontend: no ready endpoints (requests get 503)",
		"Service/sre-lab-app/whoami-v2: service missing",
		"Service/sre-lab-app/whoami: conflict with IngressRoute.traefik.io/sre-lab-app/whoami-copy → Service/sre-lab-app/whoami-v2",
	}
	if !slices.Equal(problems, wantP) {
		t.Errorf("findings =\n%s", strings.Join(problems, "\n"))
	}
}

func TestTableUnknownEndpoints(t *testing.T) {
	// Without EndpointSlices (not cached) readiness is unknown, not zero.
	f := fakeSource{
		irGVR(): {ingressRoute("a", "r", false, svcRoute("Host(`x`)", 0, "web", int64(80)),
			svcRoute("Host(`y`)", 0, "c", int64(80), map[string]any{"kind": "TraefikService"}))},
		servicesGVR: {&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "web"}}},
	}
	table := Table(f)
	for _, r := range table {
		if r.ReadyEndpoints != -1 || r.ServiceMissing {
			t.Errorf("route %+v", r)
		}
	}
	if len(Check(table)) != 0 {
		t.Errorf("findings without data: %+v", Check(table))
	}
}

// irGVR returns the traefik.io IngressRoute resource, for tests.
func irGVR() schema.GroupVersionResource { return gvr("traefik.io", "v1alpha1", "ingressroutes") }

func TestPlainHTTP(t *testing.T) {
	web := func(name string, mw ...string) *unstructured.Unstructured {
		ir := ingressRoute("a", name, false, svcRoute("Host(`x`) && PathPrefix(`/p`)", 0, "s", int64(80)))
		if len(mw) > 0 {
			ir.Object["spec"].(map[string]any)["routes"].([]any)[0].(map[string]any)["middlewares"] = []any{map[string]any{"name": mw[0]}}
		}
		return ir
	}
	secure := ingressRoute("a", "https", true, svcRoute("Host(`x`) && PathPrefix(`/p`)", 0, "s", int64(80)))
	for name, c := range map[string]struct {
		objs []runtime.Object
		want int
	}{
		"http and https, no redirect": {[]runtime.Object{web("http"), secure}, 1},
		"http with redirect":          {[]runtime.Object{web("http", "redirect-to-https"), secure}, 0},
		"http only":                   {[]runtime.Object{web("http")}, 0},
	} {
		f := fakeSource{irGVR(): c.objs}
		n := 0
		for _, fd := range Check(Table(f)) {
			if strings.Contains(fd.Problem, "plain HTTP") {
				n++
			}
		}
		if n != c.want {
			t.Errorf("%s: %d plain-HTTP findings, want %d", name, n, c.want)
		}
	}
}
