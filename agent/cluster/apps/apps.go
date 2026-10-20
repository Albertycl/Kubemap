// Package apps groups a cluster's workloads into applications, the unit the
// portal's app map shows (KM-53). No single Kubernetes object says "this is
// an application", so it combines signals, strongest first:
//
//  1. Argo CD: an Application's status.resources lists exactly what it manages.
//  2. Helm: app.kubernetes.io/managed-by=Helm plus app.kubernetes.io/instance
//     (the release name).
//  3. Labels: app.kubernetes.io/part-of, then instance, then name, then "app".
//  4. Otherwise the workload is an application of its own.
//
// Each workload root (Deployment, StatefulSet, DaemonSet, CronJob, a lone Job
// or Pod, or an operator resource such as a CNPG Cluster) joins one app. The
// app then collects, through the relationship graph, the pods, Services,
// Ingresses, HPAs, ConfigMaps, Secrets and volumes of its workloads.
package apps

import (
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/graph"
)

// Source tells how an app was found.
type Source string

const (
	SourceArgoCD   Source = "argocd"
	SourceHelm     Source = "helm"
	SourceLabel    Source = "label"
	SourceWorkload Source = "workload" // no grouping signal: one workload, one app
)

// App is one application.
type App struct {
	// ID is "namespace/name", or "argocd/<application>" for Argo CD apps,
	// which may span namespaces.
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"` // where most of its workloads run
	Source    Source `json:"source"`
	// Workloads are the root objects (graph node IDs), Members everything
	// the app includes, workloads first. Both are sorted.
	Workloads []string `json:"workloads"`
	Members   []string `json:"members"`
	// Health is "Healthy", "Degraded" (a workload or pod is failing),
	// "Missing" (a member refers to something that does not exist) or,
	// for Argo CD apps, Argo's own health and sync status.
	Health   string   `json:"health"`
	Problems []string `json:"problems,omitempty"` // "Pod/ns/x: CrashLoopBackOff"
	// System marks cluster infrastructure (kube-system, CNI, operators),
	// which the UI hides by default.
	System bool `json:"system,omitempty"`
}

var rank = map[Source]int{SourceArgoCD: 0, SourceHelm: 1, SourceLabel: 2, SourceWorkload: 3}

var argoApps = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// workloadKinds can be the root of an app. Operator resources ("Kind.group")
// are roots too.
var workloadKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true,
	"CronJob": true, "Job": true, "Pod": true,
}

// Group finds the applications in g. src is only read for Argo CD
// Applications, whose managed-resource lists are not part of the graph.
func Group(src graph.Source, g *graph.Graph) []App {
	argo := argoMembership(src)
	apps := map[string]*App{}
	for _, root := range roots(g) {
		n, _ := g.Node(root)
		id, name, source := classify(g, n, argo)
		a := apps[id]
		if a == nil {
			a = &App{ID: id, Name: name, Source: source}
			apps[id] = a
		}
		if rank[source] < rank[a.Source] {
			a.Source = source // the strongest signal among the app's workloads
		}
		a.Workloads = append(a.Workloads, root)
	}
	// Argo CD resources that are not workloads (Services, IngressRoutes) and
	// Argo apps whose workloads are all gone still belong to their app.
	for member, app := range argo.byResource {
		id := "argocd/" + app.name
		a := apps[id]
		if a == nil {
			a = &App{ID: id, Name: app.name, Source: SourceArgoCD}
			apps[id] = a
		}
		if _, ok := g.Node(member); ok {
			a.Members = append(a.Members, member)
		}
	}
	adoptOrphanServices(g, apps)
	out := make([]App, 0, len(apps))
	for _, a := range apps {
		finish(g, a, argo)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// adoptOrphanServices attaches a Service whose selector matches no pod to the
// app in its namespace with the same name as the Service or as one of the
// app's workloads. Nothing in the graph links such a Service to anything,
// yet it is usually the reason the app is down (a selector typo, a renamed
// label), so it must show on the app, not disappear.
func adoptOrphanServices(g *graph.Graph, apps map[string]*App) {
	byName := map[string]*App{} // "namespace/name" of an app or workload
	for _, a := range apps {
		for _, w := range a.Workloads {
			if n, ok := g.Node(w); ok {
				byName[n.Namespace+"/"+n.Name] = a
				byName[n.Namespace+"/"+a.Name] = a
			}
		}
	}
	for _, n := range g.Nodes {
		if n.Kind != "Service" || n.Status != "no pods match selector" {
			continue
		}
		if a := byName[n.Namespace+"/"+n.Name]; a != nil && !slices.Contains(a.Members, n.ID) {
			a.Members = append(a.Members, n.ID)
		}
	}
}

// roots returns every workload that is its own top owner: Deployments,
// StatefulSets, DaemonSets, CronJobs, Jobs and Pods without an owner, and
// operator resources that own something.
func roots(g *graph.Graph) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, n := range g.Nodes {
		if n.Missing || n.Namespace == "" {
			continue
		}
		if workloadKinds[n.Kind] || n.Kind == "ReplicaSet" {
			top := g.Owner(n.ID)
			if t, ok := g.Node(top); ok && t.Kind != "ReplicaSet" && t.Kind != "Application.argoproj.io" {
				add(top)
			}
		}
	}
	sort.Strings(out)
	return out
}

// classify returns the app a workload root belongs to.
func classify(g *graph.Graph, n graph.Node, argo argoIndex) (id, name string, source Source) {
	if app, ok := argo.byResource[n.ID]; ok {
		return "argocd/" + app.name, app.name, SourceArgoCD
	}
	lbl := labelsOf(g, n)
	if lbl["app.kubernetes.io/managed-by"] == "Helm" && lbl["app.kubernetes.io/instance"] != "" {
		name = lbl["app.kubernetes.io/instance"]
		return n.Namespace + "/" + name, name, SourceHelm
	}
	for _, key := range groupingKeys {
		if v := lbl[key]; v != "" {
			return n.Namespace + "/" + v, v, SourceLabel
		}
	}
	return n.Namespace + "/" + n.Name, n.Name, SourceWorkload
}

// groupingKeys are the grouping labels, strongest first.
var groupingKeys = []string{"app.kubernetes.io/part-of", "app.kubernetes.io/instance", "app.kubernetes.io/name", "app"}

// labelsOf returns a root's labels. Operator resources often have none, while
// the objects they create do (a CNPG Cluster's Services and pods carry
// app.kubernetes.io/instance=<database>), so it falls back to the owned
// object with the strongest grouping label. Its PVCs, for example, carry only
// app.kubernetes.io/name=postgresql, which must not win.
func labelsOf(g *graph.Graph, n graph.Node) map[string]string {
	if strength(n.Labels) < len(groupingKeys) {
		return n.Labels
	}
	best, bestRank := n.Labels, len(groupingKeys)
	for _, e := range g.Out(n.ID) {
		if e.Type != graph.EdgeOwns {
			continue
		}
		if c, ok := g.Node(e.To); ok {
			if r := strength(c.Labels); r < bestRank {
				best, bestRank = c.Labels, r
			}
		}
	}
	return best
}

// strength is the index of the strongest grouping label present, or
// len(groupingKeys) when there is none.
func strength(l map[string]string) int {
	for i, k := range groupingKeys {
		if l[k] != "" {
			return i
		}
	}
	return len(groupingKeys)
}

// finish collects an app's members, namespace, health and problems.
func finish(g *graph.Graph, a *App, argo argoIndex) {
	members := map[string]bool{}
	for _, m := range a.Members {
		members[m] = true
	}
	var pods []string
	visited := map[string]bool{}
	var walk func(id string)
	walk = func(id string) { // the workload and everything it owns
		if visited[id] {
			return
		}
		visited[id] = true
		members[id] = true
		if n, ok := g.Node(id); ok && n.Kind == "Pod" {
			pods = append(pods, id)
		}
		for _, e := range g.Out(id) {
			if e.Type == graph.EdgeOwns {
				walk(e.To)
			}
		}
	}
	for _, w := range a.Workloads {
		walk(w)
	}
	// Workloads that only Argo CD lists (for example with no pods left).
	for _, m := range a.Members {
		if n, ok := g.Node(m); ok && workloadKinds[n.Kind] {
			walk(m)
		}
	}
	for _, p := range pods {
		for _, e := range g.Out(p) {
			switch e.Type {
			case graph.EdgeConfig, graph.EdgeSecretRef, graph.EdgeMounts, graph.EdgeRunsAs:
				members[e.To] = true
				if e.Type == graph.EdgeMounts {
					for _, b := range g.Out(e.To) {
						if b.Type == graph.EdgeBinds {
							members[b.To] = true
						}
					}
				}
			}
		}
		for _, e := range g.In(p) {
			if e.Type == graph.EdgeSelects {
				members[e.From] = true
			}
		}
	}
	// Ingresses and HPAs that point at members.
	snapshot := make([]string, 0, len(members))
	for m := range members {
		snapshot = append(snapshot, m)
	}
	for _, m := range snapshot {
		for _, e := range g.In(m) {
			if e.Type == graph.EdgeRoutes || e.Type == graph.EdgeScales {
				members[e.From] = true
			}
		}
	}

	a.Workloads = uniqSorted(a.Workloads)
	wl := map[string]bool{}
	for _, w := range a.Workloads {
		wl[w] = true
	}
	a.Members = append([]string(nil), a.Workloads...)
	var rest []string
	for m := range members {
		if !wl[m] {
			rest = append(rest, m)
		}
	}
	sort.Strings(rest)
	a.Members = append(a.Members, rest...)

	a.Namespace = mainNamespace(g, a)
	a.System = isSystem(a.Namespace)
	health(g, a, argo)
}

func mainNamespace(g *graph.Graph, a *App) string {
	count := map[string]int{}
	best := ""
	for _, id := range append(append([]string{}, a.Workloads...), a.Members...) {
		if n, ok := g.Node(id); ok && n.Namespace != "" {
			count[n.Namespace]++
			if count[n.Namespace] > count[best] || (count[n.Namespace] == count[best] && n.Namespace < best) {
				best = n.Namespace
			}
		}
	}
	return best
}

// health is the worst state of the app's members. For Argo CD apps, Argo's
// own health and sync status is used when it is worse or when nothing local
// is wrong.
func health(g *graph.Graph, a *App, argo argoIndex) {
	a.Health = "Healthy"
	roots := map[string]bool{}
	for _, w := range a.Workloads {
		roots[w] = true
	}
	for _, id := range a.Members {
		n, ok := g.Node(id)
		if !ok {
			continue
		}
		switch {
		case n.Missing:
			a.Problems = append(a.Problems, id+": missing")
		case n.Kind == "Pod" && bad(n.Status):
			a.Problems = append(a.Problems, id+": "+n.Status)
		case roots[id] && strings.HasSuffix(n.Status, " ready") && notAllReady(n.Status):
			a.Problems = append(a.Problems, id+": "+n.Status)
		case n.Kind == "Service" && n.Status != "":
			a.Problems = append(a.Problems, id+": "+n.Status)
		}
	}
	if len(a.Problems) > 0 {
		a.Health = "Degraded"
		onlyMissing := true
		for _, p := range a.Problems {
			if !strings.HasSuffix(p, ": missing") {
				onlyMissing = false
			}
		}
		if onlyMissing {
			a.Health = "Missing"
		}
	}
	if a.Source == SourceArgoCD {
		if st := argo.status[a.Name]; st != "" && (a.Health == "Healthy" || !strings.HasPrefix(st, "Healthy")) {
			if a.Health == "Healthy" {
				a.Health = st
			} else {
				a.Health = a.Health + "; Argo CD: " + st
			}
		}
	}
}

func bad(podStatus string) bool {
	switch podStatus {
	case "Running", "Succeeded", "Pending", "":
		return false
	}
	return true
}

// notAllReady reads "2/3 ready".
func notAllReady(s string) bool {
	have, want, ok := strings.Cut(strings.TrimSuffix(s, " ready"), "/")
	return ok && have != want
}

// isSystem marks namespaces of cluster infrastructure.
func isSystem(ns string) bool {
	switch {
	case strings.HasPrefix(ns, "kube-"), strings.HasSuffix(ns, "-system"), strings.HasPrefix(ns, "calico-"):
		return true
	}
	switch ns {
	case "tigera-operator", "cert-manager", "argocd", "local-path-storage":
		return true
	}
	return false
}

func uniqSorted(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// argoIndex maps graph node IDs to the Argo CD Application that manages them.
type argoIndex struct {
	byResource map[string]argoApp
	status     map[string]string // app name → "Healthy, Synced"
}

type argoApp struct{ name string }

// argoMembership reads status.resources of every Argo CD Application. An
// Application lists group, kind, namespace and name of what it manages.
func argoMembership(src graph.Source) argoIndex {
	idx := argoIndex{byResource: map[string]argoApp{}, status: map[string]string{}}
	list, err := src.List(argoApps, "", nil)
	if err != nil {
		return idx // Argo CD not installed, or not cached
	}
	for _, o := range list {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		name := u.GetName()
		h, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
		s, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
		idx.status[name] = strings.Trim(h+", "+s, ", ")
		resources, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
		for _, r := range resources {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			group, _ := m["group"].(string)
			kind, _ := m["kind"].(string)
			ns, _ := m["namespace"].(string)
			rname, _ := m["name"].(string)
			apiVersion := "v1"
			if group != "" {
				apiVersion = group + "/v1"
			}
			idx.byResource[graph.ID(graph.KindOf(apiVersion, kind), ns, rname)] = argoApp{name: name}
		}
	}
	return idx
}
