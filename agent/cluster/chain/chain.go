// Package chain answers "what is this object connected to?" for any object
// in the relationship graph (KM-35). The pods are the centre: every object
// comes down to a set of pods (a Deployment through its ReplicaSets, a
// Service through its selector, a ConfigMap through the pods that use it,
// a NetworkPolicy through the pods it selects ...), and around those pods
// are the Services, routes, configuration, volumes, ServiceAccounts,
// policies and nodes. Everything comes from the agent's cache, the
// relationship graph and the analyses built on it; nothing is read live.
package chain

import (
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/netpol"
	"kubemap/agent/cluster/routes"
	"kubemap/pkg/protocol"
)

// Inputs are the analyses the chain is built from.
type Inputs struct {
	Graph  *graph.Graph
	Routes []routes.Route
	Deps   []deps.Dependency
	Apps   []apps.App
	// Netpol has the policies and pods; nil leaves the policies out (with
	// a note).
	Netpol *netpol.Snapshot
}

// policyNode returns a NetworkPolicy from the snapshot as a graph node.
func policyNode(s *netpol.Snapshot, id string) (graph.Node, bool) {
	if s == nil {
		return graph.Node{}, false
	}
	for _, p := range s.Policies {
		if graph.ID("NetworkPolicy", p.Namespace, p.Name) == id {
			return graph.Node{ID: id, Kind: "NetworkPolicy", Namespace: p.Namespace, Name: p.Name}, true
		}
	}
	return graph.Node{}, false
}

// Find returns the objects whose ID contains q (ignoring case), best first:
// a name that is q, then one that starts with q, then shorter IDs.
// NetworkPolicies from the snapshot are searched too.
func Find(g *graph.Graph, s *netpol.Snapshot, q string) []protocol.ChainItem {
	q = strings.ToLower(strings.TrimSpace(q))
	type hit struct {
		n     graph.Node
		score int
	}
	var hits []hit
	nodes := g.Nodes
	if s != nil {
		for _, p := range s.Policies {
			nodes = append(nodes[:len(nodes):len(nodes)], graph.Node{ID: graph.ID("NetworkPolicy", p.Namespace, p.Name), Kind: "NetworkPolicy", Namespace: p.Namespace, Name: p.Name})
		}
	}
	for _, n := range nodes {
		id := strings.ToLower(n.ID)
		if !strings.Contains(id, q) {
			continue
		}
		s := 2
		switch name := strings.ToLower(n.Name); {
		case name == q:
			s = 0
		case strings.HasPrefix(name, q):
			s = 1
		}
		hits = append(hits, hit{n, s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		if len(hits[i].n.ID) != len(hits[j].n.ID) {
			return len(hits[i].n.ID) < len(hits[j].n.ID)
		}
		return hits[i].n.ID < hits[j].n.ID
	})
	out := []protocol.ChainItem{}
	for _, h := range hits {
		if len(out) == protocol.MaxChainMatches {
			break
		}
		out = append(out, item(h.n, ""))
	}
	return out
}

// Build returns the chain of the object id; ok is false when the graph
// does not have it.
func Build(in Inputs, id string) (*protocol.ObjectChain, bool) {
	g := in.Graph
	self, ok := g.Node(id)
	if !ok {
		// NetworkPolicies are not nodes of the relationship graph; they
		// come from the policy snapshot.
		if self, ok = policyNode(in.Netpol, id); !ok {
			return nil, false
		}
	}
	b := &builder{g: g, sections: map[string]*section{}, self: id}
	selfItem := item(self, "")
	out := &protocol.ObjectChain{Object: &selfItem}

	// Owners, top first; and what the object owns, all the way down.
	var owners []graph.Node
	for cur := id; ; {
		up := ""
		for _, e := range g.In(cur) {
			if e.Type == graph.EdgeOwns {
				up = e.From
				break
			}
		}
		if up == "" || up == id || b.seenOwner(up) {
			break
		}
		if n, ok := g.Node(up); ok {
			owners = append([]graph.Node{n}, owners...)
		}
		cur = up
	}
	for _, n := range owners {
		b.add(protocol.ChainOwners, n, "")
	}
	pods := map[string]graph.Node{}
	var walk func(string)
	seenDown := map[string]bool{id: true}
	walk = func(from string) {
		for _, e := range g.Out(from) {
			if e.Type != graph.EdgeOwns || seenDown[e.To] {
				continue
			}
			seenDown[e.To] = true
			if n, ok := g.Node(e.To); ok {
				if n.Kind == "Pod" {
					pods[n.ID] = n
				} else {
					b.add(protocol.ChainOwned, n, "")
				}
			}
			walk(e.To)
		}
	}
	walk(id)

	// The pods the object comes down to, by what kind of object it is.
	addPod := func(pid string) {
		if n, ok := g.Node(pid); ok && n.Kind == "Pod" {
			pods[n.ID] = n
		}
	}
	services := map[string]graph.Node{}
	switch self.Kind {
	case "Pod":
		pods[self.ID] = self
	case "Service":
		services[self.ID] = self
		for _, e := range g.Out(id) {
			if e.Type == graph.EdgeSelects {
				addPod(e.To)
			}
		}
	case "PersistentVolume":
		for _, e := range g.In(id) {
			if e.Type == graph.EdgeBinds {
				b.addID(protocol.ChainVolumes, e.From, "claims it")
				for _, m := range g.In(e.From) {
					if m.Type == graph.EdgeMounts {
						addPod(m.From)
					}
				}
			}
		}
	case "HorizontalPodAutoscaler":
		for _, e := range g.Out(id) {
			if e.Type == graph.EdgeScales {
				b.addID(protocol.ChainOwned, e.To, "scales it")
				for _, p := range descendantPods(g, e.To) {
					addPod(p)
				}
			}
		}
	case "NetworkPolicy":
		if in.Netpol != nil {
			for _, pol := range in.Netpol.Policies {
				if pol.Namespace == self.Namespace && pol.Name == self.Name {
					for _, p := range in.Netpol.Selected(pol) {
						addPod(graph.ID("Pod", p.Namespace, p.Name))
					}
				}
			}
		}
	}
	// Anything pods refer to (a ConfigMap, Secret, claim, ServiceAccount,
	// node) leads back to those pods.
	for _, e := range g.In(id) {
		switch e.Type {
		case graph.EdgeConfig, graph.EdgeSecretRef, graph.EdgeMounts, graph.EdgeRunsAs, graph.EdgeRunsOn:
			addPod(e.From)
		}
	}
	// A route leads to its Services and their pods.
	for _, r := range in.Routes {
		if r.Source == id {
			if n, ok := g.Node(r.Service); ok {
				services[n.ID] = n
				for _, e := range g.Out(n.ID) {
					if e.Type == graph.EdgeSelects {
						addPod(e.To)
					}
				}
			}
		}
	}

	// Around the pods.
	workloads := map[string]bool{}
	for _, pn := range sortedNodes(pods) {
		pid := pn.ID
		workloads[g.Owner(pid)] = true
		for _, e := range g.In(pid) {
			if e.Type == graph.EdgeSelects {
				if n, ok := g.Node(e.From); ok {
					services[n.ID] = n
				}
			}
		}
		for _, e := range g.Out(pid) {
			switch e.Type {
			case graph.EdgeConfig:
				b.addID(protocol.ChainConfig, e.To, e.Detail)
			case graph.EdgeSecretRef:
				b.addID(protocol.ChainSecrets, e.To, e.Detail)
			case graph.EdgeMounts:
				b.addID(protocol.ChainVolumes, e.To, e.Detail)
				for _, v := range g.Out(e.To) {
					if v.Type == graph.EdgeBinds {
						b.addID(protocol.ChainVolumes, v.To, "bound to the claim")
					}
				}
			case graph.EdgeRunsAs:
				b.addID(protocol.ChainServiceAccounts, e.To, "")
			case graph.EdgeRunsOn:
				b.addID(protocol.ChainNodes, e.To, "")
			}
		}
	}
	if self.Kind != "Pod" || len(owners) > 0 {
		workloads[g.Owner(id)] = true
	}
	for _, n := range sortedNodes(pods) {
		if n.ID != id {
			b.add(protocol.ChainPods, n, "")
		}
	}
	for _, n := range sortedNodes(services) {
		if n.ID != id {
			b.add(protocol.ChainServices, n, "")
		}
	}
	for _, r := range in.Routes {
		if _, ok := services[r.Service]; ok && r.Source != id {
			detail := r.Detail()
			if r.Port != "" {
				detail += " → " + strings.TrimPrefix(r.Service, "Service/") + ":" + r.Port
			}
			b.addID(protocol.ChainRoutes, r.Source, detail)
		}
	}
	for _, w := range sortedKeys(workloads) {
		for _, e := range g.In(w) {
			if e.Type == graph.EdgeScales {
				b.addID(protocol.ChainAutoscalers, e.From, "")
			}
		}
	}

	// NetworkPolicies that isolate the pods.
	if in.Netpol == nil {
		out.Notes = append(out.Notes, "NetworkPolicies were not read (not in the agent's cache)")
	} else {
		dirs := map[string][]string{}
		for _, p := range in.Netpol.Pods {
			if _, ok := pods[graph.ID("Pod", p.Namespace, p.Name)]; !ok {
				continue
			}
			for _, d := range []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress} {
				for _, pol := range in.Netpol.Isolating(p, d) {
					if !contains(dirs[pol], strings.ToLower(string(d))) {
						dirs[pol] = append(dirs[pol], strings.ToLower(string(d)))
					}
				}
			}
		}
		names := make([]string, 0, len(dirs))
		for k := range dirs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			ns, name, _ := strings.Cut(k, "/")
			pid := graph.ID("NetworkPolicy", ns, name)
			if pid == id {
				continue
			}
			b.addItem(protocol.ChainPolicies, protocol.ChainItem{ID: pid, Kind: "NetworkPolicy", Namespace: ns, Name: name,
				Detail: "isolates " + strings.Join(dirs[k], " and ")})
		}
	}

	// Calls from the workloads, and callers of the Services.
	for _, d := range in.Deps {
		if workloads[d.From] || d.From == id {
			b.addID(protocol.ChainCalls, d.To, d.Via)
		}
		if _, ok := services[d.To]; (ok || d.To == id) && !workloads[d.From] {
			b.addID(protocol.ChainCalledBy, d.From, d.Via)
		}
	}

	// The apps it belongs to.
	for _, a := range in.Apps {
		for _, m := range a.Members {
			if m == id || workloads[m] {
				out.Apps = append(out.Apps, a.ID)
				break
			}
		}
	}
	if len(pods) == 0 {
		switch self.Kind {
		case "Service":
			out.Notes = append(out.Notes, "no pod matches this Service's selector: it has no endpoints")
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob":
			out.Notes = append(out.Notes, "no pods run for it now")
		}
	}
	out.Sections = b.list()
	return out, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// descendantPods returns the pods an object owns, all the way down.
func descendantPods(g *graph.Graph, id string) []string {
	var out []string
	seen := map[string]bool{id: true}
	var walk func(string)
	walk = func(from string) {
		for _, e := range g.Out(from) {
			if e.Type == graph.EdgeOwns && !seen[e.To] {
				seen[e.To] = true
				if n, ok := g.Node(e.To); ok && n.Kind == "Pod" {
					out = append(out, n.ID)
				}
				walk(e.To)
			}
		}
	}
	walk(id)
	return out
}

func sortedNodes(m map[string]graph.Node) []graph.Node {
	out := make([]graph.Node, 0, len(m))
	for _, n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func item(n graph.Node, detail string) protocol.ChainItem {
	return protocol.ChainItem{ID: n.ID, Kind: n.Kind, Namespace: n.Namespace, Name: n.Name, Status: n.Status, Missing: n.Missing, Detail: detail}
}

// sectionOrder is the order sections are returned in.
var sectionOrder = []string{
	protocol.ChainOwners, protocol.ChainOwned, protocol.ChainPods, protocol.ChainServices, protocol.ChainRoutes,
	protocol.ChainConfig, protocol.ChainSecrets, protocol.ChainVolumes, protocol.ChainServiceAccounts,
	protocol.ChainPolicies, protocol.ChainAutoscalers, protocol.ChainNodes, protocol.ChainCalls, protocol.ChainCalledBy,
}

type section struct {
	items []protocol.ChainItem
	seen  map[string]bool
	more  int
}

type builder struct {
	self     string // the object itself is never listed in its own sections
	g        *graph.Graph
	sections map[string]*section
	owners   map[string]bool
}

func (b *builder) seenOwner(id string) bool {
	if b.owners == nil {
		b.owners = map[string]bool{}
	}
	if b.owners[id] {
		return true
	}
	b.owners[id] = true
	return false
}

func (b *builder) add(key string, n graph.Node, detail string) { b.addItem(key, item(n, detail)) }

// addID adds a node by ID; an ID the graph does not have is shown as
// missing (a reference to something that does not exist).
func (b *builder) addID(key, id, detail string) {
	if n, ok := b.g.Node(id); ok {
		b.add(key, n, detail)
		return
	}
	kind, rest, _ := strings.Cut(id, "/")
	ns, name, _ := strings.Cut(rest, "/")
	b.addItem(key, protocol.ChainItem{ID: id, Kind: kind, Namespace: ns, Name: name, Missing: true, Detail: detail})
}

func (b *builder) addItem(key string, it protocol.ChainItem) {
	if it.ID == b.self {
		return
	}
	s := b.sections[key]
	if s == nil {
		s = &section{seen: map[string]bool{}}
		b.sections[key] = s
	}
	if s.seen[it.ID] {
		return
	}
	s.seen[it.ID] = true
	if len(s.items) >= protocol.MaxChainItems {
		s.more++
		return
	}
	s.items = append(s.items, it)
}

func (b *builder) list() []protocol.ChainSection {
	var out []protocol.ChainSection
	for _, k := range sectionOrder {
		if s := b.sections[k]; s != nil && len(s.items) > 0 {
			out = append(out, protocol.ChainSection{Key: k, Items: s.items, More: s.more})
		}
	}
	return out
}
