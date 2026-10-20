// Package routes reads how outside traffic reaches Services: Kubernetes
// Ingress rules and Traefik IngressRoutes, such as
//
//	flask-vue.albertycliu.xyz /api → flask-vue-backend, / → flask-vue-frontend
//
// It builds one route table for both, in the order the ingress controller
// evaluates them, and checks each route's backend: a Service that does not
// exist, a Service with no ready endpoints (the request gets a 503), and two
// routes that claim the same host and path.
//
// The package does not import the graph package; the graph uses it to add
// "routes" edges for IngressRoutes.
package routes

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Route is one host + path → backend rule.
type Route struct {
	Source string `json:"source"` // "Ingress/ns/name" or "IngressRoute.traefik.io/ns/name"
	Host   string `json:"host"`   // "" matches any host
	Path   string `json:"path"`   // "" matches every path
	// PathKind: Ingress "Prefix", "Exact", "ImplementationSpecific";
	// Traefik "PathPrefix", "Path", "PathRegexp"; "default" for an
	// Ingress default backend.
	PathKind    string   `json:"pathKind,omitempty"`
	Other       []string `json:"other,omitempty"` // other Traefik matchers, e.g. Method(`GET`)
	EntryPoints []string `json:"entryPoints,omitempty"`
	TLS         bool     `json:"tls"`
	// Priority is Traefik's effective priority: the explicit one, or the
	// length of the rule. Higher is tried first. 0 for Ingress rules, which
	// the controller orders by host and path length instead.
	Priority    int      `json:"priority,omitempty"`
	Middlewares []string `json:"middlewares,omitempty"`
	Service     string   `json:"service"` // "Service/ns/name" or "TraefikService.traefik.io/ns/name"
	Port        string   `json:"port,omitempty"`

	// Filled by Table.
	ServiceMissing bool `json:"serviceMissing,omitempty"`
	// ReadyEndpoints counts ready endpoints behind the Service, from its
	// EndpointSlices; -1 when unknown (EndpointSlices not cached, or a
	// TraefikService backend).
	ReadyEndpoints int `json:"readyEndpoints"`
}

// Detail is a short "host/path" label for graph edges and UIs.
func (r Route) Detail() string {
	host := r.Host
	if host == "" {
		host = "*"
	}
	if r.PathKind == "default" {
		return host + " (default backend)"
	}
	path := r.Path
	if path == "" {
		path = "/"
	}
	return host + path
}

// Source is the cache (or a test fake).
type Source interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

var (
	ingressesGVR      = gvr("networking.k8s.io", "v1", "ingresses")
	servicesGVR       = gvr("", "v1", "services")
	endpointSlicesGVR = gvr("discovery.k8s.io", "v1", "endpointslices")
	// IngressRouteKinds maps each Traefik API group to the graph kind of its
	// IngressRoutes.
	IngressRouteKinds = map[schema.GroupVersionResource]string{
		gvr("traefik.io", "v1alpha1", "ingressroutes"):          "IngressRoute.traefik.io",
		gvr("traefik.containo.us", "v1alpha1", "ingressroutes"): "IngressRoute.traefik.containo.us",
	}
)

// FromIngress returns the routes of one Ingress.
func FromIngress(in *networkingv1.Ingress) []Route {
	src := "Ingress/" + in.Namespace + "/" + in.Name
	tls := map[string]bool{}
	for _, t := range in.Spec.TLS {
		for _, h := range t.Hosts {
			tls[h] = true
		}
	}
	backend := func(b *networkingv1.IngressBackend) (string, string, bool) {
		if b == nil || b.Service == nil {
			return "", "", false
		}
		port := b.Service.Port.Name
		if b.Service.Port.Number != 0 {
			port = strconv.Itoa(int(b.Service.Port.Number))
		}
		return "Service/" + in.Namespace + "/" + b.Service.Name, port, true
	}
	var out []Route
	if svc, port, ok := backend(in.Spec.DefaultBackend); ok {
		out = append(out, Route{Source: src, PathKind: "default", Service: svc, Port: port})
	}
	for _, rule := range in.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			svc, port, ok := backend(&p.Backend)
			if !ok {
				continue
			}
			kind := "ImplementationSpecific"
			if p.PathType != nil {
				kind = string(*p.PathType)
			}
			out = append(out, Route{Source: src, Host: rule.Host, Path: p.Path, PathKind: kind,
				TLS: tls[rule.Host], Service: svc, Port: port})
		}
	}
	return out
}

// FromIngressRoute returns the routes of one Traefik IngressRoute; kind is
// its graph kind (see IngressRouteKinds). Rules that do not parse are
// returned as one route with the raw rule in Other, so nothing is hidden.
func FromIngressRoute(u *unstructured.Unstructured, kind string) []Route {
	ns := u.GetNamespace()
	src := kind + "/" + ns + "/" + u.GetName()
	entryPoints, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "entryPoints")
	_, hasTLS, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "tls")
	group := strings.TrimPrefix(kind, "IngressRoute.")
	specRoutes, _, _ := unstructured.NestedSlice(u.Object, "spec", "routes")
	var out []Route
	for _, raw := range specRoutes {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rule, _ := r["match"].(string)
		priority := len(rule) // Traefik's default: the rule's length
		if p, ok := asInt(r["priority"]); ok && p > 0 {
			priority = p
		}
		var mws []string
		if list, ok := r["middlewares"].([]any); ok {
			for _, m := range list {
				if mm, ok := m.(map[string]any); ok {
					name, _ := mm["name"].(string)
					if mns, _ := mm["namespace"].(string); mns != "" && mns != ns {
						name = mns + "/" + name
					}
					mws = append(mws, name)
				}
			}
		}
		matches, err := ParseRule(rule)
		if err != nil {
			matches = []Match{{Other: []string{"unparsed rule: " + rule}}}
		}
		services, _ := r["services"].([]any)
		for _, s := range services {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			name, _ := sm["name"].(string)
			sns, _ := sm["namespace"].(string)
			if sns == "" {
				sns = ns
			}
			backend := "Service/" + sns + "/" + name
			if k, _ := sm["kind"].(string); k == "TraefikService" {
				backend = "TraefikService." + group + "/" + sns + "/" + name
			}
			port := ""
			if p, ok := asInt(sm["port"]); ok {
				port = strconv.Itoa(p)
			} else if p, ok := sm["port"].(string); ok {
				port = p
			}
			for _, m := range matches {
				hosts := m.Hosts
				if len(hosts) == 0 {
					hosts = []string{""}
				}
				for _, h := range hosts {
					out = append(out, Route{
						Source: src, Host: h, Path: m.Path, PathKind: m.PathKind, Other: m.Other,
						EntryPoints: entryPoints, TLS: hasTLS, Priority: priority,
						Middlewares: mws, Service: backend, Port: port,
					})
				}
			}
		}
	}
	return out
}

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	case float64:
		return int(x), true
	}
	return 0, false
}

// Table returns every route in the cluster, checked against the Services and
// EndpointSlices, in evaluation order: by host, then by priority (Traefik)
// or path length (Ingress), longest first.
func Table(src Source) []Route {
	var out []Route
	if list, err := src.List(ingressesGVR, "", nil); err == nil {
		for _, o := range list {
			out = append(out, FromIngress(o.(*networkingv1.Ingress))...)
		}
	}
	for g, kind := range IngressRouteKinds {
		if list, err := src.List(g, "", nil); err == nil {
			for _, o := range list {
				if u, ok := o.(*unstructured.Unstructured); ok {
					out = append(out, FromIngressRoute(u, kind)...)
				}
			}
		}
	}
	services, haveServices := map[string]bool{}, false
	if list, err := src.List(servicesGVR, "", nil); err == nil {
		haveServices = true
		for _, o := range list {
			s := o.(*corev1.Service)
			services["Service/"+s.Namespace+"/"+s.Name] = true
		}
	}
	ready, haveSlices := map[string]int{}, false
	if list, err := src.List(endpointSlicesGVR, "", nil); err == nil {
		haveSlices = true
		for _, o := range list {
			es := o.(*discoveryv1.EndpointSlice)
			svc := es.Labels[discoveryv1.LabelServiceName]
			if svc == "" {
				continue
			}
			key := "Service/" + es.Namespace + "/" + svc
			for _, ep := range es.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					ready[key]++
				}
			}
		}
	}
	for i := range out {
		r := &out[i]
		r.ReadyEndpoints = -1
		if !strings.HasPrefix(r.Service, "Service/") {
			continue
		}
		if haveServices && !services[r.Service] {
			r.ServiceMissing = true
			r.ReadyEndpoints = 0
			continue
		}
		if haveSlices {
			r.ReadyEndpoints = ready[r.Service]
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if len(a.Path) != len(b.Path) {
			return len(a.Path) > len(b.Path)
		}
		return a.Source < b.Source
	})
	return out
}

// Finding is one problem in the route table.
type Finding struct {
	Route   Route  `json:"route"`
	Problem string `json:"problem"` // "service missing", "no ready endpoints", "conflict with ..."
}

// Check returns the problems in a route table: routes to a missing Service,
// routes to a Service with no ready endpoints, routes that claim the same
// host, path and entry points as another route with a different backend, and
// plain-HTTP routes next to an HTTPS one with no redirect.
func Check(table []Route) []Finding {
	var out []Finding
	claimed := map[string]Route{}
	for _, r := range table {
		switch {
		case r.ServiceMissing:
			out = append(out, Finding{r, "service missing"})
		case r.ReadyEndpoints == 0:
			out = append(out, Finding{r, "no ready endpoints (requests get 503)"})
		}
		if len(r.Other) > 0 {
			continue // extra conditions (method, headers): not a plain clash
		}
		key := fmt.Sprintf("%s|%s|%s|%s|%d", r.Host, r.PathKind, r.Path, strings.Join(r.EntryPoints, ","), r.Priority)
		if prev, ok := claimed[key]; ok && prev.Service != r.Service {
			out = append(out, Finding{r, "conflict with " + prev.Source + " → " + prev.Service})
		} else if !ok {
			claimed[key] = r
		}
	}
	out = append(out, plainHTTP(table)...)
	return out
}

// plainHTTP finds routes served without TLS while the same host and path
// also has a TLS route: the site works over HTTPS, but an http:// link still
// serves it in clear text unless a redirect middleware is attached.
func plainHTTP(table []Route) []Finding {
	secure := map[string]bool{}
	for _, r := range table {
		if r.TLS {
			secure[r.Host+"|"+r.Path] = true
		}
	}
	var out []Finding
	for _, r := range table {
		if r.TLS || r.Host == "" || !secure[r.Host+"|"+r.Path] {
			continue
		}
		redirect := false
		for _, m := range r.Middlewares {
			if strings.Contains(strings.ToLower(m), "redirect") {
				redirect = true
			}
		}
		if !redirect {
			out = append(out, Finding{r, "also served over plain HTTP without a redirect to HTTPS"})
		}
	}
	return out
}
