package traces

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

// Lister is the part of the agent's cache this package reads.
type Lister interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

var (
	podsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	nodesGVR       = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	servicesGVR    = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	replicaSetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
)

// TempoPort is Tempo's HTTP port (the Helm chart names it
// tempo-prom-metrics), which serves the search API.
const TempoPort = 3200

// Discover finds Tempo in the cache: a Service named tempo, or labelled
// app.kubernetes.io/name=tempo, with TCP port 3200. It returns "" when
// there is none.
func Discover(c Lister) string {
	objs, err := c.List(servicesGVR, "", nil)
	if err != nil {
		return ""
	}
	var found []string
	for _, o := range objs {
		s, ok := o.(*corev1.Service)
		if !ok || (s.Name != "tempo" && s.Labels["app.kubernetes.io/name"] != "tempo") {
			continue
		}
		for _, p := range s.Spec.Ports {
			if p.Port == TempoPort && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
				found = append(found, fmt.Sprintf("http://%s.%s.svc:%d", s.Name, s.Namespace, TempoPort))
			}
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// Index resolves addresses and names from the cache.
type Index struct {
	podOwner map[string]string // pod IP → workload ID ("Deployment/ns/name")
	nodeIPs  map[string]bool
	svcIP    map[string]string // ClusterIP → "Service/ns/name"
	services map[string]bool   // "Service/ns/name"
}

// NewIndex reads pods, ReplicaSets, nodes and Services from the cache.
func NewIndex(c Lister) (*Index, error) {
	x := &Index{podOwner: map[string]string{}, nodeIPs: map[string]bool{},
		svcIP: map[string]string{}, services: map[string]bool{}}
	rsOwner := map[string]string{}
	rss, err := c.List(replicaSetsGVR, "", nil)
	if err != nil {
		return nil, err
	}
	for _, o := range rss {
		rs := o.(*appsv1.ReplicaSet)
		for _, r := range rs.OwnerReferences {
			if r.Controller != nil && *r.Controller {
				rsOwner[rs.Namespace+"/"+rs.Name] = r.Kind + "/" + rs.Namespace + "/" + r.Name
			}
		}
	}
	pods, err := c.List(podsGVR, "", nil)
	if err != nil {
		return nil, err
	}
	for _, o := range pods {
		p := o.(*corev1.Pod)
		if p.Status.PodIP == "" || p.Spec.HostNetwork {
			continue // a host-network pod has the node's address
		}
		for _, r := range p.OwnerReferences {
			if r.Controller == nil || !*r.Controller {
				continue
			}
			id := r.Kind + "/" + p.Namespace + "/" + r.Name
			if r.Kind == "ReplicaSet" {
				if d, ok := rsOwner[p.Namespace+"/"+r.Name]; ok {
					id = d
				}
			}
			x.podOwner[p.Status.PodIP] = id
		}
	}
	nodes, err := c.List(nodesGVR, "", nil)
	if err != nil {
		return nil, err
	}
	for _, o := range nodes {
		for _, a := range o.(*corev1.Node).Status.Addresses {
			x.nodeIPs[a.Address] = true
		}
	}
	svcs, err := c.List(servicesGVR, "", nil)
	if err != nil {
		return nil, err
	}
	for _, o := range svcs {
		s := o.(*corev1.Service)
		id := "Service/" + s.Namespace + "/" + s.Name
		x.services[id] = true
		for _, ip := range s.Spec.ClusterIPs {
			if ip != "" && ip != corev1.ClusterIPNone {
				x.svcIP[ip] = id
			}
		}
	}
	return x, nil
}

// probe reports a kubelet health probe: from a node's own address, or with
// the kube-probe user agent.
func (x *Index) probe(s Span) bool {
	return x.nodeIPs[s.PeerIP] || strings.HasPrefix(s.UserAgent, "kube-probe/")
}

// service resolves a client span's target to "Service/ns/name": a Service
// name in its short or DNS forms (the caller's namespace for a bare name),
// or a ClusterIP.
func (x *Index) service(target, callerNS string) string {
	if id, ok := x.svcIP[target]; ok {
		return id
	}
	parts := strings.Split(strings.TrimSuffix(target, "."), ".")
	var id string
	switch {
	case len(parts) == 1:
		id = "Service/" + callerNS + "/" + parts[0]
	case len(parts) == 2 || (len(parts) >= 3 && parts[2] == "svc"):
		id = "Service/" + parts[1] + "/" + parts[0]
	default:
		return ""
	}
	if x.services[id] {
		return id
	}
	return ""
}

// maxUnmatched bounds the list of unexplained calls.
const maxUnmatched = 20

// Confirm marks the links of m that the spans show, and sets m.Traces.
func Confirm(m *protocol.AppMap, spans []Span, x *Index, source string, window time.Duration, now time.Time) {
	sum := &protocol.MapTraceSummary{Source: source, Window: window.String(), At: now}
	m.Traces = sum
	links := map[string]*protocol.MapLink{}
	byFrom := map[string][]*protocol.MapLink{}
	for i := range m.Links {
		l := &m.Links[i]
		links[l.From+"|"+l.To] = l
		byFrom[l.From] = append(byFrom[l.From], l)
	}
	// Workloads on the map by name, for a span's service.name; entries by
	// route host.
	byName := map[string][]protocol.MapComponent{}
	byHost := map[string][]string{}
	add := func(c protocol.MapComponent) {
		switch c.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			byName[c.Name] = append(byName[c.Name], c)
		}
		for _, r := range c.Routes {
			// One entry per host, even when several rules share it (/api
			// and /), so a span counts once.
			if r.Host != "" && !contains(byHost[r.Host], c.ID) {
				byHost[r.Host] = append(byHost[r.Host], c.ID)
			}
		}
	}
	for _, a := range m.Apps {
		for _, c := range a.Components {
			add(c)
		}
	}
	for _, c := range append(append([]protocol.MapComponent{}, m.Peers...), m.Unassigned...) {
		add(c)
	}
	resolve := func(s Span) (protocol.MapComponent, bool) {
		var hit []protocol.MapComponent
		for _, c := range byName[first(s.Deployment, s.Service)] {
			if s.Namespace == "" || c.Namespace == s.Namespace {
				hit = append(hit, c)
			}
		}
		if len(hit) != 1 {
			return protocol.MapComponent{}, false // unknown or ambiguous: no guess
		}
		return hit[0], true
	}
	type tally struct {
		n      int
		last   time.Time
		detail string
	}
	hits := map[*protocol.MapLink]*tally{}
	confirm := func(l *protocol.MapLink, s Span, detail string) {
		t := hits[l]
		if t == nil {
			t = &tally{detail: detail}
			hits[l] = t
		}
		t.n++
		if s.Start.After(t.last) {
			t.last = s.Start
		}
	}
	unmatched := map[string]bool{}
	for _, s := range spans {
		sum.Spans++
		switch s.Kind {
		case "server":
			if x.probe(s) {
				sum.Probes++
				continue
			}
			callee, ok := resolve(s)
			if !ok {
				continue
			}
			done := false
			for _, entry := range byHost[s.Host] {
				if l := links[entry+"|"+callee.ID]; l != nil {
					confirm(l, s, fmt.Sprintf("server spans in %s for Host %s", s.Service, s.Host))
					done = true
				}
			}
			if caller := x.podOwner[s.PeerIP]; caller != "" && !done {
				if l := links[caller+"|"+callee.ID]; l != nil {
					confirm(l, s, fmt.Sprintf("server spans in %s from pods of %s", s.Service, caller))
					done = true
				} else if len(byHost[s.Host]) == 0 && callerOnMap(byName, caller) {
					unmatched[fmt.Sprintf("%s calls %s (server spans): no link on the map", caller, callee.ID)] = true
				}
			}
		case "client":
			caller, ok := resolve(s)
			if !ok || s.Target == "" {
				continue
			}
			svc := x.service(s.Target, caller.Namespace)
			done := false
			for _, l := range byFrom[caller.ID] {
				if (svc != "" && contains(l.Via, svc)) || strings.HasSuffix(l.To, "/"+caller.Namespace+"/"+s.Target) {
					confirm(l, s, fmt.Sprintf("client spans in %s to %s", s.Service, s.Target))
					done = true
				}
			}
			if !done {
				unmatched[fmt.Sprintf("%s calls %s (client spans): no link on the map", caller.ID, s.Target)] = true
			}
		}
	}
	for l, t := range hits {
		l.Traces = &protocol.MapTraces{Spans: t.n, LastSeen: t.last, Detail: fmt.Sprintf("%d %s", t.n, t.detail)}
		sum.Confirmed++
	}
	for u := range unmatched {
		sum.Unmatched = append(sum.Unmatched, u)
	}
	sort.Strings(sum.Unmatched)
	if len(sum.Unmatched) > maxUnmatched {
		sum.Unmatched = append(sum.Unmatched[:maxUnmatched], fmt.Sprintf("… and %d more", len(unmatched)-maxUnmatched))
	}
}

// callerOnMap: only callers that are workloads on the map count as
// unexplained; an ingress controller outside the map is not news.
func callerOnMap(byName map[string][]protocol.MapComponent, id string) bool {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 {
		return false
	}
	for _, c := range byName[parts[2]] {
		if c.ID == id {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Narrowed returns the summary for a narrowed map: the confirmed links it
// keeps, and the unexplained calls that name one of its components.
func Narrowed(sum *protocol.MapTraceSummary, out *protocol.AppMap) *protocol.MapTraceSummary {
	if sum == nil {
		return nil
	}
	n := *sum
	n.Confirmed, n.Unmatched = 0, nil
	ids := map[string]bool{}
	for _, l := range out.Links {
		ids[l.From], ids[l.To] = true, true
		if l.Traces != nil {
			n.Confirmed++
		}
	}
	for _, a := range out.Apps {
		for _, c := range a.Components {
			ids[c.ID] = true
		}
	}
	for _, u := range sum.Unmatched {
		if id, _, ok := strings.Cut(u, " calls "); ok && ids[id] {
			n.Unmatched = append(n.Unmatched, u)
		} else if _, callee, ok := strings.Cut(u, " calls "); ok && ids[strings.SplitN(callee, " ", 2)[0]] {
			n.Unmatched = append(n.Unmatched, u)
		}
	}
	return &n
}
