// Package graph builds the relationship graph of a cluster from the agent's
// object cache: which object owns, selects, routes to, scales, configures,
// mounts or runs which other object. It is the base of the application map
// (KM-49 to KM-53) and runs inside the cluster agent, so only the graph, not
// the objects, ever needs to reach the portal.
//
// References to objects that do not exist (an Ingress backend Service that
// was deleted, a ConfigMap a pod mounts but nobody created) become nodes with
// Missing set, because those broken links are what an SRE looks for.
package graph

import (
	"encoding/json"
	"sort"
	"strings"
)

// EdgeType names a relationship. Edges point from the object that holds the
// reference to the object it refers to.
type EdgeType string

const (
	EdgeOwns      EdgeType = "owns"       // ownerReferences: Deployment → ReplicaSet → Pod
	EdgeSelects   EdgeType = "selects"    // Service → Pod, by label selector
	EdgeRoutes    EdgeType = "routes"     // Ingress → Service; Detail is host+path
	EdgeScales    EdgeType = "scales"     // HorizontalPodAutoscaler → Deployment/StatefulSet
	EdgeConfig    EdgeType = "config"     // Pod → ConfigMap (volume, env or envFrom)
	EdgeSecretRef EdgeType = "secret-ref" // Pod → Secret (name only; values are never read)
	EdgeMounts    EdgeType = "mounts"     // Pod → PersistentVolumeClaim
	EdgeBinds     EdgeType = "binds"      // PersistentVolumeClaim → PersistentVolume
	EdgeRunsAs    EdgeType = "runs-as"    // Pod → ServiceAccount
	EdgeRunsOn    EdgeType = "runs-on"    // Pod → Node
)

// Node is one object. ID is "Kind/namespace/name" ("Kind//name" for
// cluster-scoped objects); custom resources use "Kind.group" as the kind,
// for example "Cluster.postgresql.cnpg.io/db/url-shortener-db".
type Node struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Status is a short health word for the UI: "Running", "CrashLoopBackOff",
	// "2/3 ready", "Bound" ... Empty when there is nothing useful to say.
	Status string `json:"status,omitempty"`
	// Missing is true when something refers to this object but the cache
	// does not have it: deleted, never created, or (for Secrets) not cached.
	Missing bool `json:"missing,omitempty"`
}

// Edge is one relationship.
type Edge struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Type   EdgeType `json:"type"`
	Detail string   `json:"detail,omitempty"` // e.g. "shop.example.com/api" or "env DB_HOST"
}

// Graph is an immutable relationship graph. Build a new one when the cache
// changes (see Builder).
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
	// Incomplete lists resources the cache could not provide, so the graph
	// may lack their nodes and edges (for example "forbidden" RBAC).
	Incomplete []string `json:"incomplete,omitempty"`

	byID map[string]int
	out  map[string][]int
	in   map[string][]int
}

// UnmarshalJSON decodes a graph and rebuilds its lookup index, so a graph
// received over the tunnel answers Node, Out and In like the original.
func (g *Graph) UnmarshalJSON(b []byte) error {
	type plain Graph // no methods: avoids recursion
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*g = Graph(p)
	g.index()
	return nil
}

// ID returns the node ID for a kind, namespace and name.
func ID(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

func (g *Graph) index() {
	g.byID = make(map[string]int, len(g.Nodes))
	g.out = map[string][]int{}
	g.in = map[string][]int{}
	for i, n := range g.Nodes {
		g.byID[n.ID] = i
	}
	for i, e := range g.Edges {
		g.out[e.From] = append(g.out[e.From], i)
		g.in[e.To] = append(g.in[e.To], i)
	}
}

// Node returns the node with the given ID.
func (g *Graph) Node(id string) (Node, bool) {
	i, ok := g.byID[id]
	if !ok {
		return Node{}, false
	}
	return g.Nodes[i], true
}

// Out returns the edges that start at id, and In the edges that end there.
func (g *Graph) Out(id string) []Edge { return g.pick(g.out[id]) }
func (g *Graph) In(id string) []Edge  { return g.pick(g.in[id]) }

func (g *Graph) pick(idx []int) []Edge {
	out := make([]Edge, len(idx))
	for i, j := range idx {
		out[i] = g.Edges[j]
	}
	return out
}

// Owner returns the top controller of an object by following "owns" edges
// upwards: for a Pod, its Deployment (through the ReplicaSet), StatefulSet,
// DaemonSet, CronJob or operator resource. An object with no owner is its own
// top owner.
func (g *Graph) Owner(id string) string {
	seen := map[string]bool{}
	for !seen[id] {
		seen[id] = true
		next := ""
		for _, e := range g.In(id) {
			if e.Type == EdgeOwns {
				next = e.From
				break
			}
		}
		if next == "" {
			return id
		}
		id = next
	}
	return id // an ownership loop; stop where it closed
}

// Around returns the part of the graph within depth edges of id, following
// edges in both directions. It is the "what is connected to this?" view.
func (g *Graph) Around(id string, depth int) *Graph {
	keep := map[string]bool{id: true}
	frontier := []string{id}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, n := range frontier {
			for _, e := range append(g.Out(n), g.In(n)...) {
				for _, m := range []string{e.From, e.To} {
					if !keep[m] {
						keep[m] = true
						next = append(next, m)
					}
				}
			}
		}
		frontier = next
	}
	return g.filter(func(n Node) bool { return keep[n.ID] })
}

// Namespace returns the nodes of one namespace plus the cluster-scoped
// nodes they connect to (Nodes, PersistentVolumes), with the edges among them.
func (g *Graph) Namespace(ns string) *Graph {
	keep := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Namespace == ns {
			keep[n.ID] = true
		}
	}
	for _, e := range g.Edges {
		for _, pair := range [][2]string{{e.From, e.To}, {e.To, e.From}} {
			if keep[pair[0]] {
				if n, ok := g.Node(pair[1]); ok && n.Namespace == "" {
					keep[n.ID] = true
				}
			}
		}
	}
	return g.filter(func(n Node) bool { return keep[n.ID] })
}

func (g *Graph) filter(keep func(Node) bool) *Graph {
	out := &Graph{Incomplete: g.Incomplete}
	for _, n := range g.Nodes {
		if keep(n) {
			out.Nodes = append(out.Nodes, n)
		}
	}
	ids := map[string]bool{}
	for _, n := range out.Nodes {
		ids[n.ID] = true
	}
	for _, e := range g.Edges {
		if ids[e.From] && ids[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	out.index()
	return out
}

// sortAndIndex puts nodes and edges in a stable order, so equal clusters
// give byte-equal JSON, and builds the lookup maps.
func (g *Graph) sortAndIndex() {
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Detail < b.Detail
	})
	sort.Strings(g.Incomplete)
	g.index()
}

// KindOf returns the graph kind for an apiVersion and kind: the plain kind for
// Kubernetes built-in groups, "Kind.group" for everything else, so two CRDs
// named "Cluster" cannot collide.
func KindOf(apiVersion, kind string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		group = "" // "v1"
	}
	if builtinGroups[group] {
		return kind
	}
	return kind + "." + group
}

var builtinGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "networking.k8s.io": true,
	"autoscaling": true, "policy": true, "rbac.authorization.k8s.io": true,
	"discovery.k8s.io": true, "storage.k8s.io": true,
}
