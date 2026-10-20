// Package appmap turns the agent's analysis (the KM-32 graph, KM-49 apps,
// KM-50 dependencies, KM-51 routes and KM-52 data stores) into the
// application map the portal draws (KM-53, protocol.AppMap): each app's
// components in tiers, entry → frontend → backend → database / cache, with
// their pods and health, and the links between components with the
// configuration that proves each link.
//
// The portal only lays the map out. Every judgement (which tier, is it
// healthy, why are these two linked) is made here, next to the data, and is
// explained in the response (TierReason, Evidence), so an SRE can check it.
package appmap

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/datastores"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/routes"
	"kubemap/pkg/protocol"
)

// Input is the analysis of one cluster.
type Input struct {
	Graph         *graph.Graph
	Apps          []apps.App
	Dependencies  []deps.Dependency
	Routes        []routes.Route
	RouteFindings []routes.Finding
	Datastores    []datastores.Datastore
	// Pod looks up a pod for its restarts, readiness and node; nil, or a
	// nil result, falls back to the graph's status word.
	Pod func(namespace, name string) *corev1.Pod
}

// Build returns the map of the whole cluster. Use Narrow for one namespace
// or one app.
func Build(in Input) *protocol.AppMap {
	b := &builder{in: in, g: in.Graph, comps: map[string]*protocol.MapComponent{},
		appOf: map[string]string{}, stores: map[string]*datastores.Datastore{}, links: map[[2]string]*protocol.MapLink{}}
	if b.g == nil {
		b.g = &graph.Graph{}
	}
	for i := range in.Datastores {
		b.stores[in.Datastores[i].ID] = &in.Datastores[i]
	}
	for _, a := range in.Apps {
		for _, m := range a.Members {
			if _, taken := b.appOf[m]; !taken {
				b.appOf[m] = a.ID
			}
		}
		for _, w := range a.Workloads {
			b.component(w).App = a.ID
		}
	}
	// A Service whose selector matches no pod is a classic outage (a label
	// typo); show it even when nothing links to it.
	for _, n := range b.g.Nodes {
		if n.Kind == "Service" && n.Status == "no pods match selector" {
			b.component(n.ID)
		}
	}
	b.routeLinks()
	b.dependencyLinks()
	b.operatorLinks()
	for _, c := range b.comps {
		b.classify(c)
	}
	return b.assemble()
}

type builder struct {
	in     Input
	g      *graph.Graph
	comps  map[string]*protocol.MapComponent
	appOf  map[string]string // graph node ID → app ID
	stores map[string]*datastores.Datastore
	links  map[[2]string]*protocol.MapLink
}

// component returns the component for a graph node ID, creating it with
// its pods, Services and health on first use.
func (b *builder) component(id string) *protocol.MapComponent {
	if c, ok := b.comps[id]; ok {
		return c
	}
	n, ok := b.g.Node(id)
	if !ok {
		kind, ns, name := split(id)
		n = graph.Node{ID: id, Kind: kind, Namespace: ns, Name: name, Missing: true}
	}
	c := &protocol.MapComponent{ID: id, Kind: n.Kind, Namespace: n.Namespace, Name: n.Name,
		App: b.appOf[id], Status: n.Status, Missing: n.Missing}
	b.comps[id] = c
	switch {
	case isEntry(n.Kind):
		// Routes are added by routeLinks; health is decided there.
	case n.Kind == "Service":
		// A link target with no workload behind it: gone, selecting no
		// pods, or with endpoints managed by hand (ExternalName).
		if c.Status == "" && c.Missing {
			c.Status = "missing"
		}
		c.Healthy = !b.broken(id)
	default:
		b.fillPods(c)
	}
	return c
}

// fillPods adds the pods the component owns (directly, or through
// ReplicaSets and operator-created workloads) and the Services that select
// them, and decides the component's health.
func (b *builder) fillPods(c *protocol.MapComponent) {
	store := b.stores[c.ID]
	if store != nil {
		c.Engine, c.Version = store.Engine, store.Version
		if c.Status == "" {
			c.Status = store.Status
		}
	}
	services := map[string]bool{}
	seen := map[string]bool{c.ID: true}
	var walk func(id string)
	walk = func(id string) {
		for _, e := range b.g.Out(id) {
			if e.Type != graph.EdgeOwns || seen[e.To] {
				continue
			}
			seen[e.To] = true
			n, _ := b.g.Node(e.To)
			if n.Kind == "Pod" {
				c.Pods = append(c.Pods, b.pod(n, store))
				for _, s := range b.g.In(n.ID) {
					if s.Type == graph.EdgeSelects {
						services[s.From] = true
					}
				}
			}
			walk(e.To)
		}
	}
	walk(c.ID)
	slices.SortFunc(c.Pods, func(x, y protocol.MapPod) int { return cmp.Compare(x.Name, y.Name) })
	for s := range services {
		c.Services = append(c.Services, s)
	}
	slices.Sort(c.Services)

	c.Healthy = !c.Missing && !notAllReady(c.Status)
	if store != nil && store.Desired > 0 && store.Ready < store.Desired {
		c.Healthy = false
	}
	for _, p := range c.Pods {
		if !p.Ready && p.Status != string(corev1.PodSucceeded) {
			c.Healthy = false
		}
	}
}

func (b *builder) pod(n graph.Node, store *datastores.Datastore) protocol.MapPod {
	p := protocol.MapPod{Name: n.Name, Status: n.Status, Ready: n.Status == string(corev1.PodRunning)}
	for _, e := range b.g.Out(n.ID) {
		if e.Type == graph.EdgeRunsOn {
			_, _, p.Node = split(e.To)
		}
	}
	if store != nil && store.Primary == n.Name {
		p.Primary = true
	}
	if b.in.Pod == nil {
		return p
	}
	obj := b.in.Pod(n.Namespace, n.Name)
	if obj == nil {
		return p
	}
	if obj.Spec.NodeName != "" {
		p.Node = obj.Spec.NodeName
	}
	p.Ready = false
	for _, cond := range obj.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			p.Ready = true
		}
	}
	for _, cs := range obj.Status.ContainerStatuses {
		p.Restarts += cs.RestartCount
	}
	return p
}

// backends returns the components behind a Service: the top owners of the
// pods it selects.
func (b *builder) backends(service string) []string {
	var out []string
	for _, e := range b.g.Out(service) {
		if e.Type == graph.EdgeSelects {
			if o := b.g.Owner(e.To); !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	slices.Sort(out)
	return out
}

// broken is true when traffic to a Service cannot arrive: the Service does
// not exist, or its selector matches no pod.
func (b *builder) broken(service string) bool {
	n, ok := b.g.Node(service)
	return !ok || n.Missing || n.Status == "no pods match selector"
}

// routeLinks makes an entry component per Ingress or IngressRoute and links
// it to the workloads behind each rule's Service.
func (b *builder) routeLinks() {
	problems := map[string][]string{}
	for _, f := range b.in.RouteFindings {
		k := routeKey(f.Route)
		problems[k] = append(problems[k], f.Problem)
	}
	for _, r := range b.in.Routes {
		entry := b.component(r.Source)
		mr := protocol.MapRoute{Host: r.Host, Path: r.Path, TLS: r.TLS, EntryPoints: r.EntryPoints,
			Service: r.Service, Port: r.Port, ReadyEndpoints: r.ReadyEndpoints, Problems: problems[routeKey(r)]}
		if r.PathKind == "default" {
			mr.Path = "" // a default backend matches every path
		}
		entry.Routes = append(entry.Routes, mr)
		ev := protocol.MapEvidence{Type: protocol.EvidenceRoute, Object: r.Source, Name: r.Detail(), Port: r.Port,
			Detail: r.Detail() + " → " + r.Service + portSuffix(r.Port)}
		targets := b.backends(r.Service)
		if len(targets) == 0 {
			b.link(r.Source, r.Service, r.Service, b.broken(r.Service), ev)
			continue
		}
		for _, t := range targets {
			b.link(r.Source, t, r.Service, false, ev)
		}
	}
	for _, c := range b.comps {
		if !isEntry(c.Kind) {
			continue
		}
		if c.App == "" {
			c.App = b.entryApp(c)
		}
		c.Healthy = !c.Missing
		for _, r := range c.Routes {
			if slices.ContainsFunc(r.Problems, func(p string) bool {
				return p == "service missing" || strings.HasPrefix(p, "no ready endpoints")
			}) {
				c.Healthy = false
			}
		}
	}
}

// entryApp picks the app of an entry that no app lists as a member: the app
// of the first workload it routes to.
func (b *builder) entryApp(c *protocol.MapComponent) string {
	for _, r := range c.Routes {
		if a := b.appOf[r.Service]; a != "" {
			return a
		}
		for _, t := range b.backends(r.Service) {
			if a := b.appOf[t]; a != "" {
				return a
			}
		}
	}
	return ""
}

// dependencyLinks links a workload to the workloads behind each Service its
// configuration names (KM-50).
func (b *builder) dependencyLinks() {
	for _, d := range b.in.Dependencies {
		from := b.g.Owner(d.From)
		_, ns, _ := split(d.From)
		ev := protocol.MapEvidence{Type: protocol.EvidenceType(d.Kind), Object: d.From, Name: d.Name, Port: d.Port,
			Detail: d.Via + " → " + d.To + portSuffix(d.Port)}
		if d.ConfigMap != "" {
			ev.Object = graph.ID("ConfigMap", ns, d.ConfigMap)
			if d.Key != d.Name {
				ev.Key = d.Key
			}
		}
		if ev.Type == "" {
			ev.Type = protocol.EvidenceEnv
		}
		targets := b.backends(d.To)
		if len(targets) == 0 {
			b.link(from, d.To, d.To, d.Missing || b.broken(d.To), ev)
			continue
		}
		for _, t := range targets {
			if t != from {
				b.link(from, t, d.To, false, ev)
			}
		}
	}
}

// operatorLinks links a pooler to the database it pools (KM-52).
func (b *builder) operatorLinks() {
	for _, s := range b.in.Datastores {
		if s.Target == "" {
			continue
		}
		_, _, name := split(s.Target)
		what := "pooler"
		if len(s.Evidence) > 0 {
			what = s.Evidence[0]
		}
		ev := protocol.MapEvidence{Type: protocol.EvidenceOperator, Object: s.ID, Name: name,
			Detail: what + " " + s.Engine + " pools " + name}
		_, exists := b.g.Node(s.Target)
		b.link(s.ID, s.Target, "", !exists, ev)
	}
}

// link adds one piece of evidence to the link between two components,
// creating both ends when needed.
func (b *builder) link(from, to, via string, missing bool, ev protocol.MapEvidence) {
	b.component(from)
	b.component(to)
	k := [2]string{from, to}
	l := b.links[k]
	if l == nil {
		l = &protocol.MapLink{From: from, To: to}
		b.links[k] = l
	}
	l.Missing = l.Missing || missing
	if via != "" && !slices.Contains(l.Via, via) {
		l.Via = append(l.Via, via)
	}
	if !slices.Contains(l.Evidence, ev) {
		l.Evidence = append(l.Evidence, ev)
	}
}

// Name tokens and label values that say which tier a workload is in.
var (
	frontendWords = []string{"frontend", "front", "ui", "web", "www", "client", "portal", "spa"}
	backendWords  = []string{"backend", "back", "api", "server", "worker", "svc", "service", "controller", "consumer"}
	databaseWords = []string{"database", "db", "postgres", "postgresql", "mysql", "mariadb", "mongo", "mongodb", "primary", "replica"}
	cacheWords    = []string{"cache", "redis", "memcached", "valkey"}
	tierLabels    = []string{"app.kubernetes.io/component", "component", "tier", "role"}
)

// classify decides a component's tier, strongest signal first: entry kind,
// data store detection, a component label, the name, then the traffic
// pattern (routed from an entry or not).
func (b *builder) classify(c *protocol.MapComponent) {
	set := func(t protocol.Tier, why string) { c.Tier, c.TierReason = t, why }
	if isEntry(c.Kind) {
		set(protocol.TierEntry, c.Kind+" rules")
		return
	}
	if s := b.stores[c.ID]; s != nil {
		why := s.Engine
		if len(s.Evidence) > 0 {
			why += ": " + strings.Join(s.Evidence, ", ")
		}
		if s.Category == datastores.CategoryCache {
			set(protocol.TierCache, why)
		} else {
			set(protocol.TierDatabase, why)
		}
		return
	}
	n, _ := b.g.Node(c.ID)
	for _, key := range tierLabels {
		if v := strings.ToLower(n.Labels[key]); v != "" {
			if t, ok := tierOfWord(v); ok {
				set(t, "label "+key+"="+n.Labels[key])
				return
			}
		}
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(c.Name), func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if t, ok := tierOfWord(word); ok {
			set(t, "name contains "+word)
			return
		}
	}
	routed, callsStore := false, false
	for _, l := range b.links {
		if l.To == c.ID {
			if f := b.comps[l.From]; f != nil && isEntry(f.Kind) {
				routed = true
			}
		}
		if l.From == c.ID && b.stores[l.To] != nil {
			callsStore = true
		}
	}
	switch {
	case routed && callsStore:
		set(protocol.TierBackend, "receives traffic from an entry and calls a data store")
	case routed:
		set(protocol.TierFrontend, "receives traffic from an entry")
	case c.Kind == "Service":
		set(protocol.TierBackend, "a Service with nothing behind it")
	default:
		set(protocol.TierBackend, "not exposed through an entry")
	}
}

func tierOfWord(w string) (protocol.Tier, bool) {
	switch {
	case slices.Contains(frontendWords, w):
		return protocol.TierFrontend, true
	case slices.Contains(backendWords, w):
		return protocol.TierBackend, true
	case slices.Contains(databaseWords, w):
		return protocol.TierDatabase, true
	case slices.Contains(cacheWords, w):
		return protocol.TierCache, true
	}
	return "", false
}

// assemble puts the components into their apps and sorts everything, so the
// same cluster gives byte-equal JSON.
func (b *builder) assemble() *protocol.AppMap {
	m := &protocol.AppMap{Apps: []protocol.MapApp{}, Links: []protocol.MapLink{}, Incomplete: b.g.Incomplete}
	index := map[string]int{}
	for _, a := range b.in.Apps {
		index[a.ID] = len(m.Apps)
		m.Apps = append(m.Apps, protocol.MapApp{ID: a.ID, Name: a.Name, Namespace: a.Namespace, Source: string(a.Source),
			Health: a.Health, State: state(a.Health), Problems: problems(a.Problems), System: a.System,
			Components: []protocol.MapComponent{}})
	}
	for _, c := range b.comps {
		for _, l := range b.links {
			if l.From == c.ID && l.FromApp == "" {
				l.FromApp = c.App
			}
			if l.To == c.ID && l.ToApp == "" {
				l.ToApp = c.App
			}
		}
		if i, ok := index[c.App]; ok {
			m.Apps[i].Components = append(m.Apps[i].Components, *c)
		} else {
			c.App = ""
			m.Unassigned = append(m.Unassigned, *c)
		}
	}
	for i := range m.Apps {
		sortComponents(m.Apps[i].Components)
	}
	sortComponents(m.Unassigned)
	for _, l := range b.links {
		slices.Sort(l.Via)
		slices.SortFunc(l.Evidence, func(x, y protocol.MapEvidence) int {
			return cmp.Or(cmp.Compare(x.Type, y.Type), cmp.Compare(x.Object, y.Object), cmp.Compare(x.Name, y.Name), cmp.Compare(x.Detail, y.Detail))
		})
		m.Links = append(m.Links, *l)
	}
	slices.SortFunc(m.Links, func(x, y protocol.MapLink) int { return cmp.Or(cmp.Compare(x.From, y.From), cmp.Compare(x.To, y.To)) })
	return m
}

// state turns KM-49's health text into a colour: Degraded fails; a sync
// or progress state, a missing reference or anything unknown warns.
func state(health string) protocol.AppState {
	switch {
	case strings.Contains(health, "Degraded"):
		return protocol.StateFail
	case health == "Healthy" || health == "Healthy, Synced":
		return protocol.StateOK
	}
	return protocol.StateWarn
}

// problems splits KM-49's "<id>: <status>" strings. IDs contain "/" but
// never ": ", so the first ": " ends the ID.
func problems(in []string) []protocol.MapProblem {
	var out []protocol.MapProblem
	for _, p := range in {
		id, status, ok := strings.Cut(p, ": ")
		if !ok {
			id, status = "", p
		}
		out = append(out, protocol.MapProblem{ID: id, Status: status})
	}
	return out
}

func sortComponents(cs []protocol.MapComponent) {
	slices.SortFunc(cs, func(x, y protocol.MapComponent) int {
		return cmp.Or(cmp.Compare(slices.Index(protocol.Tiers, x.Tier), slices.Index(protocol.Tiers, y.Tier)), cmp.Compare(x.ID, y.ID))
	})
}

// Narrow keeps the apps whose main namespace is namespace (all when empty)
// and, when app is set, only that app. Links that touch a kept app stay;
// their far ends outside it become Peers, without pods. ok is false when
// app names no app.
func Narrow(m *protocol.AppMap, namespace, app string) (out *protocol.AppMap, ok bool) {
	if namespace == "" && app == "" {
		return m, true
	}
	out = &protocol.AppMap{Apps: []protocol.MapApp{}, Links: []protocol.MapLink{}, Incomplete: m.Incomplete}
	all := map[string]protocol.MapComponent{}
	kept := map[string]bool{}
	for _, a := range m.Apps {
		for _, c := range a.Components {
			all[c.ID] = c
		}
		if (app == "" || a.ID == app) && (namespace == "" || a.Namespace == namespace) {
			out.Apps = append(out.Apps, a)
			for _, c := range a.Components {
				kept[c.ID] = true
			}
		}
	}
	if app != "" && len(out.Apps) == 0 {
		return nil, false
	}
	for _, c := range m.Unassigned {
		all[c.ID] = c
		if app == "" && c.Namespace == namespace {
			out.Unassigned = append(out.Unassigned, c)
			kept[c.ID] = true
		}
	}
	peers := map[string]bool{}
	for _, l := range m.Links {
		if !kept[l.From] && !kept[l.To] {
			continue
		}
		out.Links = append(out.Links, l)
		for _, id := range []string{l.From, l.To} {
			if !kept[id] && !peers[id] {
				peers[id] = true
				p := all[id]
				p.Pods = nil
				out.Peers = append(out.Peers, p)
			}
		}
	}
	sortComponents(out.Peers)
	return out, true
}

// routeKey identifies a route, to match findings to rules.
func routeKey(r routes.Route) string {
	return strings.Join([]string{r.Source, r.Host, r.PathKind, r.Path, strings.Join(r.EntryPoints, ","), r.Service, r.Port, strings.Join(r.Other, "&")}, "|")
}

func isEntry(kind string) bool {
	return kind == "Ingress" || strings.HasPrefix(kind, "IngressRoute.")
}

// notAllReady is true for a workload status "2/3 ready" with fewer ready
// than wanted, and for a Service with no pods.
func notAllReady(s string) bool {
	if s == "no pods match selector" {
		return true
	}
	have, rest, ok := strings.Cut(s, "/")
	if !ok || !strings.HasSuffix(rest, " ready") {
		return false
	}
	return have != strings.TrimSuffix(rest, " ready")
}

func split(id string) (kind, ns, name string) {
	kind, rest, _ := strings.Cut(id, "/")
	ns, name, _ = strings.Cut(rest, "/")
	return kind, ns, name
}

func portSuffix(p string) string {
	if p == "" {
		return ""
	}
	return ":" + p
}
