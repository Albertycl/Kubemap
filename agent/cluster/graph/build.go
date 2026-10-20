package graph

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/cache"
	"kubemap/agent/cluster/routes"
)

// Source is what the graph reads: the agent's cache (*cache.Cache) in
// production, a map of objects in tests.
type Source interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
	Generation() uint64
}

// kinds lists the resources that become graph nodes. RBAC, NetworkPolicies
// and events are left out on purpose: they have their own engines (KM-33,
// KM-34) and would bury the application structure.
var kinds = []struct {
	gvr  schema.GroupVersionResource
	kind string // graph kind; custom resources get "Kind.group"
}{
	{gvr("", "v1", "nodes"), "Node"},
	{gvr("", "v1", "pods"), "Pod"},
	{gvr("", "v1", "services"), "Service"},
	{gvr("", "v1", "configmaps"), "ConfigMap"},
	{gvr("", "v1", "serviceaccounts"), "ServiceAccount"},
	{gvr("", "v1", "persistentvolumeclaims"), "PersistentVolumeClaim"},
	{gvr("", "v1", "persistentvolumes"), "PersistentVolume"},
	{gvr("apps", "v1", "deployments"), "Deployment"},
	{gvr("apps", "v1", "replicasets"), "ReplicaSet"},
	{gvr("apps", "v1", "statefulsets"), "StatefulSet"},
	{gvr("apps", "v1", "daemonsets"), "DaemonSet"},
	{gvr("batch", "v1", "jobs"), "Job"},
	{gvr("batch", "v1", "cronjobs"), "CronJob"},
	{gvr("networking.k8s.io", "v1", "ingresses"), "Ingress"},
	{gvr("autoscaling", "v2", "horizontalpodautoscalers"), "HorizontalPodAutoscaler"},
	{gvr("argoproj.io", "v1alpha1", "applications"), "Application.argoproj.io"},
	{gvr("traefik.io", "v1alpha1", "ingressroutes"), "IngressRoute.traefik.io"},
	{gvr("traefik.containo.us", "v1alpha1", "ingressroutes"), "IngressRoute.traefik.containo.us"},
	{gvr("postgresql.cnpg.io", "v1", "clusters"), "Cluster.postgresql.cnpg.io"},
	{gvr("postgresql.cnpg.io", "v1", "poolers"), "Pooler.postgresql.cnpg.io"},
}

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

// rootCAConfigMap is mounted into every pod by the ServiceAccount token
// projection; an edge from every pod to it would only be noise.
const rootCAConfigMap = "kube-root-ca.crt"

// Builder keeps the last graph and rebuilds it only when the source changed.
// It is safe for concurrent use.
type Builder struct {
	src  Source
	mu   sync.Mutex
	gen  uint64
	last *Graph
}

// NewBuilder returns a Builder over src.
func NewBuilder(src Source) *Builder { return &Builder{src: src} }

// Graph returns the current graph, rebuilding it if the source has changed
// since the last call. Callers must not change the returned graph.
func (b *Builder) Graph() *Graph {
	b.mu.Lock()
	defer b.mu.Unlock()
	gen := b.src.Generation()
	if b.last == nil || gen != b.gen {
		b.last, b.gen = Build(b.src), gen
	}
	return b.last
}

type builder struct {
	g       *Graph
	watched map[string]bool // kinds the source could list
	edges   map[Edge]bool
}

// Build makes a graph from everything src holds now.
func Build(src Source) *Graph {
	b := &builder{g: &Graph{}, watched: map[string]bool{}, edges: map[Edge]bool{}}
	b.g.byID = map[string]int{}
	var objs []typedObject
	for _, k := range kinds {
		list, err := src.List(k.gvr, "", nil)
		if err != nil {
			if errors.Is(err, cache.ErrNotCached) && isOptional(k.kind) {
				continue // CRD not installed: nothing is missing
			}
			b.g.Incomplete = append(b.g.Incomplete, k.gvr.GroupResource().String())
			continue
		}
		b.watched[k.kind] = true
		for _, o := range list {
			objs = append(objs, typedObject{kind: k.kind, obj: o})
			b.addNode(k.kind, o)
		}
	}
	for _, o := range objs {
		b.ownerEdges(o)
	}
	// Pods by namespace, for Service selectors.
	pods := map[string][]*corev1.Pod{}
	for _, o := range objs {
		if p, ok := o.obj.(*corev1.Pod); ok {
			pods[p.Namespace] = append(pods[p.Namespace], p)
		}
	}
	for _, o := range objs {
		switch x := o.obj.(type) {
		case *corev1.Pod:
			b.podEdges(x)
		case *corev1.Service:
			b.serviceEdges(x, pods[x.Namespace])
		case *networkingv1.Ingress:
			b.routeEdges(routes.FromIngress(x))
		case *unstructured.Unstructured:
			if strings.HasPrefix(o.kind, "IngressRoute.") {
				b.routeEdges(routes.FromIngressRoute(x, o.kind))
			}
		case *autoscalingv2.HorizontalPodAutoscaler:
			ref := x.Spec.ScaleTargetRef
			b.edge(ID("HorizontalPodAutoscaler", x.Namespace, x.Name), ID(KindOf(ref.APIVersion, ref.Kind), x.Namespace, ref.Name), EdgeScales, "")
		case *corev1.PersistentVolumeClaim:
			if x.Spec.VolumeName != "" {
				b.edge(ID("PersistentVolumeClaim", x.Namespace, x.Name), ID("PersistentVolume", "", x.Spec.VolumeName), EdgeBinds, "")
			}
		}
	}
	b.clearOptionalMissing()
	b.g.sortAndIndex()
	return b.g
}

type typedObject struct {
	kind string
	obj  runtime.Object
}

func isOptional(kind string) bool { return strings.Contains(kind, ".") }

func (b *builder) addNode(kind string, obj runtime.Object) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	n := Node{
		ID: ID(kind, acc.GetNamespace(), acc.GetName()), Kind: kind,
		Namespace: acc.GetNamespace(), Name: acc.GetName(),
		Labels: acc.GetLabels(), Status: status(obj),
	}
	b.g.byID[n.ID] = len(b.g.Nodes)
	b.g.Nodes = append(b.g.Nodes, n)
}

// edge adds a deduplicated edge. A target the graph does not have becomes a
// placeholder node, marked Missing when its kind is one the source lists
// (so its absence is real) and unmarked for kinds the agent never caches,
// such as Secrets.
func (b *builder) edge(from, to string, t EdgeType, detail string) {
	e := Edge{From: from, To: to, Type: t, Detail: detail}
	if b.edges[e] {
		return
	}
	b.edges[e] = true
	b.g.Edges = append(b.g.Edges, e)
	b.ensure(from) // an owner may be a kind the agent does not cache
	b.ensure(to)
}

func (b *builder) ensure(id string) {
	if _, ok := b.g.byID[id]; ok {
		return
	}
	kind, ns, name := splitID(id)
	b.g.byID[id] = len(b.g.Nodes)
	b.g.Nodes = append(b.g.Nodes, Node{ID: id, Kind: kind, Namespace: ns, Name: name, Missing: b.watched[kind]})
}

func splitID(id string) (kind, ns, name string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 {
		return id, "", ""
	}
	return parts[0], parts[1], parts[2]
}

// ownerEdges links every object to the owners in its ownerReferences. Owners
// live in the same namespace, or are cluster-scoped when the object is.
func (b *builder) ownerEdges(o typedObject) {
	acc, err := meta.Accessor(o.obj)
	if err != nil {
		return
	}
	for _, ref := range acc.GetOwnerReferences() {
		owner := ID(KindOf(ref.APIVersion, ref.Kind), acc.GetNamespace(), ref.Name)
		b.edge(owner, ID(o.kind, acc.GetNamespace(), acc.GetName()), EdgeOwns, "")
	}
}

func (b *builder) podEdges(p *corev1.Pod) {
	id := ID("Pod", p.Namespace, p.Name)
	sa := p.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	b.edge(id, ID("ServiceAccount", p.Namespace, sa), EdgeRunsAs, "")
	if p.Spec.NodeName != "" {
		b.edge(id, ID("Node", "", p.Spec.NodeName), EdgeRunsOn, "")
	}
	cm := func(name, detail string, optional *bool) {
		if name != "" && name != rootCAConfigMap {
			b.edge(id, ID("ConfigMap", p.Namespace, name), EdgeConfig, withOptional(detail, optional))
		}
	}
	secret := func(name, detail string, optional *bool) {
		if name != "" {
			b.edge(id, ID("Secret", p.Namespace, name), EdgeSecretRef, withOptional(detail, optional))
		}
	}
	for _, v := range p.Spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			cm(v.ConfigMap.Name, "volume "+v.Name, v.ConfigMap.Optional)
		case v.Secret != nil:
			secret(v.Secret.SecretName, "volume "+v.Name, v.Secret.Optional)
		case v.PersistentVolumeClaim != nil:
			b.edge(id, ID("PersistentVolumeClaim", p.Namespace, v.PersistentVolumeClaim.ClaimName), EdgeMounts, "volume "+v.Name)
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					cm(s.ConfigMap.Name, "volume "+v.Name, s.ConfigMap.Optional)
				}
				if s.Secret != nil {
					secret(s.Secret.Name, "volume "+v.Name, s.Secret.Optional)
				}
			}
		}
	}
	for _, ps := range p.Spec.ImagePullSecrets {
		secret(ps.Name, "imagePullSecrets", nil)
	}
	for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		for _, from := range c.EnvFrom {
			if from.ConfigMapRef != nil {
				cm(from.ConfigMapRef.Name, "envFrom", from.ConfigMapRef.Optional)
			}
			if from.SecretRef != nil {
				secret(from.SecretRef.Name, "envFrom", from.SecretRef.Optional)
			}
		}
		for _, env := range c.Env {
			if env.ValueFrom == nil {
				continue
			}
			if r := env.ValueFrom.ConfigMapKeyRef; r != nil {
				cm(r.Name, "env "+env.Name, r.Optional)
			}
			if r := env.ValueFrom.SecretKeyRef; r != nil {
				secret(r.Name, "env "+env.Name, r.Optional)
			}
		}
	}
}

const optionalSuffix = " (optional)"

func withOptional(detail string, optional *bool) string {
	if optional != nil && *optional {
		return detail + optionalSuffix
	}
	return detail
}

// clearOptionalMissing unmarks Missing nodes that are only referenced
// optionally: a pod with an optional ConfigMap runs fine without it
// (CoreDNS's coredns-custom is the classic example).
func (b *builder) clearOptionalMissing() {
	required := map[string]bool{}
	for _, e := range b.g.Edges {
		if !strings.HasSuffix(e.Detail, optionalSuffix) {
			required[e.To] = true
		}
	}
	for i := range b.g.Nodes {
		if n := &b.g.Nodes[i]; n.Missing && !required[n.ID] {
			n.Missing = false
		}
	}
}

func (b *builder) serviceEdges(s *corev1.Service, pods []*corev1.Pod) {
	if len(s.Spec.Selector) == 0 {
		return // ExternalName, or endpoints managed by hand
	}
	sel := labels.SelectorFromSet(s.Spec.Selector)
	id := ID("Service", s.Namespace, s.Name)
	matched := 0
	for _, p := range pods {
		if sel.Matches(labels.Set(p.Labels)) {
			b.edge(id, ID("Pod", p.Namespace, p.Name), EdgeSelects, "")
			matched++
		}
	}
	if matched == 0 {
		// A classic outage: the selector matches no pod, so the Service has
		// no endpoints and every request to it fails.
		b.g.Nodes[b.g.byID[id]].Status = "no pods match selector"
	}
}

// routeEdges links an Ingress or IngressRoute to the Services it sends
// traffic to, one edge per host and path.
func (b *builder) routeEdges(rs []routes.Route) {
	for _, r := range rs {
		b.edge(r.Source, r.Service, EdgeRoutes, r.Detail())
	}
}

// status returns a short health word for an object, or "".
func status(obj runtime.Object) string {
	switch x := obj.(type) {
	case *corev1.Pod:
		return podStatus(x)
	case *corev1.Node:
		for _, c := range x.Status.Conditions {
			if c.Type == corev1.NodeReady {
				if c.Status == corev1.ConditionTrue {
					return "Ready"
				}
				return "NotReady"
			}
		}
	case *appsv1.Deployment:
		return ready(x.Status.ReadyReplicas, x.Spec.Replicas)
	case *appsv1.StatefulSet:
		return ready(x.Status.ReadyReplicas, x.Spec.Replicas)
	case *appsv1.ReplicaSet:
		if x.Spec.Replicas != nil && *x.Spec.Replicas == 0 {
			return "" // an old revision; not interesting
		}
		return ready(x.Status.ReadyReplicas, x.Spec.Replicas)
	case *appsv1.DaemonSet:
		return fmt.Sprintf("%d/%d ready", x.Status.NumberReady, x.Status.DesiredNumberScheduled)
	case *batchv1.Job:
		for _, c := range x.Status.Conditions {
			if c.Status == corev1.ConditionTrue && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
				return string(c.Type)
			}
		}
		return "Running"
	case *corev1.PersistentVolumeClaim:
		return string(x.Status.Phase)
	case *corev1.PersistentVolume:
		return string(x.Status.Phase)
	case *unstructured.Unstructured:
		// Argo CD: health and sync; CNPG and most operators: status.phase.
		if h, _, _ := unstructured.NestedString(x.Object, "status", "health", "status"); h != "" {
			if s, _, _ := unstructured.NestedString(x.Object, "status", "sync", "status"); s != "" {
				return h + ", " + s
			}
			return h
		}
		if p, _, _ := unstructured.NestedString(x.Object, "status", "phase"); p != "" {
			return p
		}
	}
	return ""
}

func ready(have int32, want *int32) string {
	w := int32(1)
	if want != nil {
		w = *want
	}
	return fmt.Sprintf("%d/%d ready", have, w)
}

// podStatus is what "kubectl get pods" shows in STATUS, roughly: a waiting
// or terminated container reason beats the phase.
func podStatus(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, c := range list {
			if w := c.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				if last := c.LastTerminationState.Terminated; last != nil && last.Reason == "OOMKilled" {
					return w.Reason + " (OOMKilled)"
				}
				return w.Reason
			}
			if t := c.State.Terminated; t != nil && t.Reason != "" && t.Reason != "Completed" {
				return t.Reason
			}
		}
	}
	if p.Status.Phase == corev1.PodPending {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
				return "Pending (" + c.Reason + ")"
			}
		}
	}
	if p.Status.Phase == corev1.PodRunning {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
				return "Running, not ready"
			}
		}
	}
	return string(p.Status.Phase)
}

// PodStatus is the short status word of a pod, as "kubectl get pods" shows
// it: a waiting or terminated container reason beats the phase.
func PodStatus(p *corev1.Pod) string { return podStatus(p) }
