package graph

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/cache"
)

// fakeSource serves objects by resource, like the cache. Resources in
// forbidden fail as the cache does for RBAC-denied ones.
type fakeSource struct {
	objs      map[schema.GroupVersionResource][]runtime.Object
	forbidden map[schema.GroupVersionResource]bool
	gen       uint64
	lists     int
}

func (f *fakeSource) List(g schema.GroupVersionResource, ns string, _ labels.Selector) ([]runtime.Object, error) {
	f.lists++
	if f.forbidden[g] {
		return nil, fmt.Errorf("%w: %s is forbidden", cache.ErrNotCached, g)
	}
	if _, ok := f.objs[g]; !ok && isOptional(kindFor(g)) {
		return nil, fmt.Errorf("%w: %s is absent", cache.ErrNotCached, g)
	}
	return f.objs[g], nil
}

func (f *fakeSource) Generation() uint64 { return f.gen }

func kindFor(g schema.GroupVersionResource) string {
	for _, k := range kinds {
		if k.gvr == g {
			return k.kind
		}
	}
	return ""
}

func (f *fakeSource) add(g schema.GroupVersionResource, objs ...runtime.Object) {
	f.objs[g] = append(f.objs[g], objs...)
}

var (
	gPods   = gvr("", "v1", "pods")
	gSvc    = gvr("", "v1", "services")
	gCM     = gvr("", "v1", "configmaps")
	gPVC    = gvr("", "v1", "persistentvolumeclaims")
	gPV     = gvr("", "v1", "persistentvolumes")
	gNodes  = gvr("", "v1", "nodes")
	gSA     = gvr("", "v1", "serviceaccounts")
	gDeploy = gvr("apps", "v1", "deployments")
	gRS     = gvr("apps", "v1", "replicasets")
	gSTS    = gvr("apps", "v1", "statefulsets")
	gIng    = gvr("networking.k8s.io", "v1", "ingresses")
	gHPA    = gvr("autoscaling", "v2", "horizontalpodautoscalers")
	gJob    = gvr("batch", "v1", "jobs")
	gCNPG   = gvr("postgresql.cnpg.io", "v1", "clusters")
	gArgo   = gvr("argoproj.io", "v1alpha1", "applications")
)

func om(ns, name string, lbl map[string]string, owners ...metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl, OwnerReferences: owners}
}

func owner(apiVersion, kind, name string) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, Controller: &t}
}

func i32(v int32) *int32 { return &v }

// shop is a small app modelled on this cluster's flask-vue and url-shortener:
// Ingress → Services → Deployment pods, a Postgres operator cluster, config,
// a volume, an HPA, and three planted faults for the demo.
func shop() *fakeSource {
	f := &fakeSource{objs: map[schema.GroupVersionResource][]runtime.Object{}, gen: 1}
	web := map[string]string{"app": "web"}
	api := map[string]string{"app": "api"}
	f.add(gNodes, &corev1.Node{ObjectMeta: om("", "k8server", nil),
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	f.add(gDeploy,
		&appsv1.Deployment{ObjectMeta: om("shop", "web", web), Spec: appsv1.DeploymentSpec{Replicas: i32(2)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 2}},
		&appsv1.Deployment{ObjectMeta: om("shop", "api", api), Spec: appsv1.DeploymentSpec{Replicas: i32(1)}})
	f.add(gRS,
		&appsv1.ReplicaSet{ObjectMeta: om("shop", "web-7d9f8", web, owner("apps/v1", "Deployment", "web")), Spec: appsv1.ReplicaSetSpec{Replicas: i32(2)}, Status: appsv1.ReplicaSetStatus{ReadyReplicas: 2}},
		&appsv1.ReplicaSet{ObjectMeta: om("shop", "web-old", web, owner("apps/v1", "Deployment", "web")), Spec: appsv1.ReplicaSetSpec{Replicas: i32(0)}},
		&appsv1.ReplicaSet{ObjectMeta: om("shop", "api-5c6b7", api, owner("apps/v1", "Deployment", "api")), Spec: appsv1.ReplicaSetSpec{Replicas: i32(1)}})
	running := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	f.add(gPods,
		&corev1.Pod{ObjectMeta: om("shop", "web-7d9f8-a", web, owner("apps/v1", "ReplicaSet", "web-7d9f8")),
			Spec: corev1.PodSpec{NodeName: "k8server", ServiceAccountName: "web",
				Volumes: []corev1.Volume{
					{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-conf"}}}},
					{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
						{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: rootCAConfigMap}}}}}}},
				}},
			Status: running},
		&corev1.Pod{ObjectMeta: om("shop", "web-7d9f8-b", web, owner("apps/v1", "ReplicaSet", "web-7d9f8")),
			Spec: corev1.PodSpec{NodeName: "k8server", ServiceAccountName: "web"}, Status: running},
		&corev1.Pod{ObjectMeta: om("shop", "api-5c6b7-a", api, owner("apps/v1", "ReplicaSet", "api-5c6b7")),
			Spec: corev1.PodSpec{NodeName: "k8server",
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
				InitContainers:   []corev1.Container{{Name: "migrate", EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db-app"}}}}}},
				Containers: []corev1.Container{{Name: "app",
					EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "api-env"}}}},
					Env: []corev1.EnvVar{
						{Name: "DB_HOST", Value: "db-rw"},
						{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db-app"}, Key: "password"}}},
						{Name: "LOG_LEVEL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "api-env"}, Key: "level"}}},
					}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "api-data"}}}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}}},
		// A CNPG instance pod, owned by the operator's Cluster resource.
		&corev1.Pod{ObjectMeta: om("shop", "db-1", map[string]string{"cnpg.io/cluster": "db"}, owner("postgresql.cnpg.io/v1", "Cluster", "db")),
			Spec: corev1.PodSpec{NodeName: "k8server"}, Status: running},
	)
	f.add(gSvc,
		&corev1.Service{ObjectMeta: om("shop", "web", nil), Spec: corev1.ServiceSpec{Selector: web}},
		&corev1.Service{ObjectMeta: om("shop", "api", nil), Spec: corev1.ServiceSpec{Selector: api}},
		&corev1.Service{ObjectMeta: om("shop", "db-rw", nil, owner("postgresql.cnpg.io/v1", "Cluster", "db")), Spec: corev1.ServiceSpec{Selector: map[string]string{"cnpg.io/cluster": "db"}}},
		// Fault 1: a typo in the selector, so the Service matches no pod.
		&corev1.Service{ObjectMeta: om("shop", "api-typo", nil), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "apii"}}},
		&corev1.Service{ObjectMeta: om("shop", "external", nil), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"}},
	)
	prefix := networkingv1.PathTypePrefix
	f.add(gIng, &networkingv1.Ingress{ObjectMeta: om("shop", "shop", nil), Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
		Host: "shop.example.com",
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			{Path: "/api", PathType: &prefix, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api"}}},
			{Path: "/", PathType: &prefix, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web"}}},
			// Fault 2: a route to a Service that was deleted.
			{Path: "/old", PathType: &prefix, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "legacy"}}},
		}}}}}}})
	f.add(gHPA, &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: om("shop", "web", nil),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}}})
	// Fault 3: api-env is referenced but never created. web-conf exists.
	f.add(gCM, &corev1.ConfigMap{ObjectMeta: om("shop", "web-conf", nil)}, &corev1.ConfigMap{ObjectMeta: om("shop", rootCAConfigMap, nil)})
	f.add(gPVC, &corev1.PersistentVolumeClaim{ObjectMeta: om("shop", "api-data", nil), Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-123"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}})
	f.add(gPV, &corev1.PersistentVolume{ObjectMeta: om("", "pvc-123", nil), Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound}})
	f.add(gSA, &corev1.ServiceAccount{ObjectMeta: om("shop", "web", nil)}, &corev1.ServiceAccount{ObjectMeta: om("shop", "default", nil)})
	f.add(gCNPG, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": "shop", "name": "db"},
		"status":   map[string]any{"phase": "Cluster in healthy state"},
	}})
	f.add(gArgo, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": "shop"},
		"status":   map[string]any{"health": map[string]any{"status": "Degraded"}, "sync": map[string]any{"status": "Synced"}},
	}})
	return f
}

func hasEdge(g *Graph, from, to string, t EdgeType) bool {
	return slices.ContainsFunc(g.Out(from), func(e Edge) bool { return e.To == to && e.Type == t })
}

func node(t *testing.T, g *Graph, id string) Node {
	t.Helper()
	n, ok := g.Node(id)
	if !ok {
		t.Fatalf("no node %s", id)
	}
	return n
}

func TestEdges(t *testing.T) {
	g := Build(shop())
	want := []struct {
		from, to string
		t        EdgeType
	}{
		{"Deployment/shop/web", "ReplicaSet/shop/web-7d9f8", EdgeOwns},
		{"ReplicaSet/shop/web-7d9f8", "Pod/shop/web-7d9f8-a", EdgeOwns},
		{"Cluster.postgresql.cnpg.io/shop/db", "Pod/shop/db-1", EdgeOwns},
		{"Cluster.postgresql.cnpg.io/shop/db", "Service/shop/db-rw", EdgeOwns},
		{"Service/shop/web", "Pod/shop/web-7d9f8-a", EdgeSelects},
		{"Service/shop/web", "Pod/shop/web-7d9f8-b", EdgeSelects},
		{"Service/shop/db-rw", "Pod/shop/db-1", EdgeSelects},
		{"Ingress/shop/shop", "Service/shop/api", EdgeRoutes},
		{"Ingress/shop/shop", "Service/shop/web", EdgeRoutes},
		{"HorizontalPodAutoscaler/shop/web", "Deployment/shop/web", EdgeScales},
		{"Pod/shop/web-7d9f8-a", "ConfigMap/shop/web-conf", EdgeConfig},
		{"Pod/shop/api-5c6b7-a", "ConfigMap/shop/api-env", EdgeConfig},
		{"Pod/shop/api-5c6b7-a", "Secret/shop/db-app", EdgeSecretRef},
		{"Pod/shop/api-5c6b7-a", "Secret/shop/regcred", EdgeSecretRef},
		{"Pod/shop/api-5c6b7-a", "PersistentVolumeClaim/shop/api-data", EdgeMounts},
		{"PersistentVolumeClaim/shop/api-data", "PersistentVolume//pvc-123", EdgeBinds},
		{"Pod/shop/web-7d9f8-a", "ServiceAccount/shop/web", EdgeRunsAs},
		{"Pod/shop/api-5c6b7-a", "ServiceAccount/shop/default", EdgeRunsAs},
		{"Pod/shop/web-7d9f8-a", "Node//k8server", EdgeRunsOn},
	}
	for _, w := range want {
		if !hasEdge(g, w.from, w.to, w.t) {
			t.Errorf("missing edge %s -[%s]-> %s", w.from, w.t, w.to)
		}
	}
	// No noise: no edge to the root CA ConfigMap, none from the typo Service
	// or the ExternalName Service, and no duplicate edges from the api pod's
	// two references to api-env and db-app.
	if hasEdge(g, "Pod/shop/web-7d9f8-a", "ConfigMap/shop/"+rootCAConfigMap, EdgeConfig) {
		t.Error("edge to kube-root-ca.crt")
	}
	if len(g.Out("Service/shop/api-typo")) != 0 || len(g.Out("Service/shop/external")) != 0 {
		t.Error("selectorless or unmatched services must have no selects edges")
	}
	details := map[string]bool{}
	for _, e := range g.Out("Pod/shop/api-5c6b7-a") {
		if e.To == "Secret/shop/db-app" {
			details[e.Detail] = true
		}
	}
	if !details["envFrom"] || !details["env DB_PASSWORD"] || len(details) != 2 {
		t.Errorf("db-app references = %v", details)
	}
	routes := map[string]string{}
	for _, e := range g.Out("Ingress/shop/shop") {
		routes[e.Detail] = e.To
	}
	if routes["shop.example.com/api"] != "Service/shop/api" || routes["shop.example.com/"] != "Service/shop/web" {
		t.Errorf("routes = %v", routes)
	}
}

func TestFaultsAreVisible(t *testing.T) {
	g := Build(shop())
	if n := node(t, g, "Service/shop/api-typo"); n.Status != "no pods match selector" {
		t.Errorf("typo service status = %q", n.Status)
	}
	if n := node(t, g, "Service/shop/legacy"); !n.Missing {
		t.Error("ingress backend to a deleted Service must be Missing")
	}
	if n := node(t, g, "ConfigMap/shop/api-env"); !n.Missing {
		t.Error("referenced ConfigMap that does not exist must be Missing")
	}
	// Secrets are never cached, so a reference to one is not "missing".
	if n := node(t, g, "Secret/shop/db-app"); n.Missing {
		t.Error("Secret must not be marked Missing")
	}
	// An optional reference to a ConfigMap that does not exist is fine.
	f := shop()
	yes := true
	p := f.objs[gPods][0].(*corev1.Pod)
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "custom", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "coredns-custom"}, Optional: &yes}}})
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "side", EnvFrom: []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "api-env"}, Optional: &yes}}}})
	g2 := Build(f)
	if n := node(t, g2, "ConfigMap/shop/coredns-custom"); n.Missing {
		t.Error("optional-only reference must not be Missing")
	}
	if !slices.ContainsFunc(g2.Out("Pod/shop/"+p.Name), func(e Edge) bool { return e.Detail == "volume custom (optional)" }) {
		t.Error("optional edge detail missing")
	}
	// api-env is also referenced without optional by the api pod: still Missing.
	if n := node(t, g2, "ConfigMap/shop/api-env"); !n.Missing {
		t.Error("a required reference must keep Missing")
	}
	if n := node(t, g, "Pod/shop/api-5c6b7-a"); n.Status != "CrashLoopBackOff (OOMKilled)" {
		t.Errorf("api pod status = %q", n.Status)
	}
}

func TestStatus(t *testing.T) {
	g := Build(shop())
	for id, want := range map[string]string{
		"Node//k8server":                      "Ready",
		"Deployment/shop/web":                 "2/2 ready",
		"Deployment/shop/api":                 "0/1 ready",
		"ReplicaSet/shop/web-old":             "",
		"Pod/shop/web-7d9f8-a":                "Running",
		"PersistentVolumeClaim/shop/api-data": "Bound",
		"Cluster.postgresql.cnpg.io/shop/db":  "Cluster in healthy state",
		"Application.argoproj.io/argocd/shop": "Degraded, Synced",
	} {
		if got := node(t, g, id).Status; got != want {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}
	del := metav1.Now()
	cases := []struct {
		pod  corev1.Pod
		want string
	}{
		{corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &del}}, "Terminating"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}}}, "Pending (Unschedulable)"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}}, "Running, not ready"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, "ImagePullBackOff"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}, "Pending"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}}}, "OOMKilled"},
		{corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}}}, "Succeeded"},
	}
	for _, c := range cases {
		if got := podStatus(&c.pod); got != c.want {
			t.Errorf("podStatus = %q, want %q", got, c.want)
		}
	}
	job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	if status(job) != "Failed" || status(&batchv1.Job{}) != "Running" {
		t.Error("job status")
	}
	ds := &appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{NumberReady: 1, DesiredNumberScheduled: 3}}
	if status(ds) != "1/3 ready" {
		t.Errorf("daemonset = %q", status(ds))
	}
	if status(&corev1.Node{}) != "" || status(&unstructured.Unstructured{Object: map[string]any{}}) != "" {
		t.Error("objects without status must give empty status")
	}
	notReady := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}}}
	if status(notReady) != "NotReady" {
		t.Error("node NotReady")
	}
	health := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"health": map[string]any{"status": "Healthy"}}}}
	if status(health) != "Healthy" {
		t.Error("argo health without sync")
	}
}

func TestOwner(t *testing.T) {
	g := Build(shop())
	for id, want := range map[string]string{
		"Pod/shop/web-7d9f8-a": "Deployment/shop/web",
		"Pod/shop/db-1":        "Cluster.postgresql.cnpg.io/shop/db",
		"Deployment/shop/web":  "Deployment/shop/web",
		"Node//k8server":       "Node//k8server",
	} {
		if got := g.Owner(id); got != want {
			t.Errorf("Owner(%s) = %s, want %s", id, got, want)
		}
	}
	// An ownership loop must not hang.
	loop := &Graph{Nodes: []Node{{ID: "A"}, {ID: "B"}}, Edges: []Edge{{From: "A", To: "B", Type: EdgeOwns}, {From: "B", To: "A", Type: EdgeOwns}}}
	loop.index()
	if o := loop.Owner("A"); o != "A" && o != "B" {
		t.Errorf("loop owner = %s", o)
	}
}

func TestAroundAndNamespace(t *testing.T) {
	g := Build(shop())
	sub := g.Around("Ingress/shop/shop", 2)
	for _, id := range []string{"Service/shop/web", "Pod/shop/web-7d9f8-a", "Service/shop/legacy", "Pod/shop/api-5c6b7-a"} {
		if _, ok := sub.Node(id); !ok {
			t.Errorf("Around(ingress, 2) lacks %s", id)
		}
	}
	if _, ok := sub.Node("ConfigMap/shop/web-conf"); ok {
		t.Error("Around(ingress, 2) should not reach 3 hops away")
	}
	for _, e := range sub.Edges {
		if _, ok := sub.Node(e.From); !ok {
			t.Errorf("dangling edge %v", e)
		}
	}
	ns := g.Namespace("shop")
	if _, ok := ns.Node("Node//k8server"); !ok {
		t.Error("namespace view must include the Node its pods run on")
	}
	if _, ok := ns.Node("PersistentVolume//pvc-123"); !ok {
		t.Error("namespace view must include bound PVs")
	}
	if _, ok := ns.Node("Application.argoproj.io/argocd/shop"); ok {
		t.Error("namespace view leaked another namespace")
	}
}

func TestIncompleteAndAbsent(t *testing.T) {
	f := shop()
	f.forbidden = map[schema.GroupVersionResource]bool{gCM: true}
	delete(f.objs, gCNPG) // operator not installed
	g := Build(f)
	if !slices.Equal(g.Incomplete, []string{"configmaps"}) {
		t.Errorf("incomplete = %v", g.Incomplete)
	}
	// With ConfigMaps unknown, a referenced ConfigMap is not "missing".
	if n := node(t, g, "ConfigMap/shop/web-conf"); n.Missing {
		t.Error("ConfigMap must not be Missing when ConfigMaps are not cached")
	}
	// The CNPG owner is not installed, so it is a plain placeholder.
	if n := node(t, g, "Cluster.postgresql.cnpg.io/shop/db"); n.Missing {
		t.Error("owner of an uninstalled CRD kind must not be Missing")
	}
}

func TestBuilderRebuildsOnlyOnChange(t *testing.T) {
	f := shop()
	b := NewBuilder(f)
	g1 := b.Graph()
	lists := f.lists
	if g2 := b.Graph(); g2 != g1 || f.lists != lists {
		t.Error("unchanged source must not rebuild")
	}
	f.gen++
	f.add(gPods, &corev1.Pod{ObjectMeta: om("shop", "web-new", map[string]string{"app": "web"})})
	g3 := b.Graph()
	if g3 == g1 || !hasEdge(g3, "Service/shop/web", "Pod/shop/web-new", EdgeSelects) {
		t.Error("changed source must rebuild")
	}
}

func TestStableJSON(t *testing.T) {
	a, _ := json.Marshal(Build(shop()))
	b, _ := json.Marshal(Build(shop()))
	if string(a) != string(b) {
		t.Error("equal input must give equal JSON")
	}
	if !strings.Contains(string(a), `"missing":true`) {
		t.Error("JSON lacks missing marker")
	}
}

func TestKindOfAndIDs(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"v1", "Pod"}:                        "Pod",
		{"apps/v1", "Deployment"}:            "Deployment",
		{"postgresql.cnpg.io/v1", "Cluster"}: "Cluster.postgresql.cnpg.io",
		{"cluster.x-k8s.io/v1", "Cluster"}:   "Cluster.cluster.x-k8s.io",
	} {
		if got := KindOf(in[0], in[1]); got != want {
			t.Errorf("kindOf%v = %s, want %s", in, got, want)
		}
	}
	if k, ns, n := splitID("PersistentVolume//pv-1"); k != "PersistentVolume" || ns != "" || n != "pv-1" {
		t.Errorf("splitID = %s %s %s", k, ns, n)
	}
	if k, _, _ := splitID("odd"); k != "odd" {
		t.Error("splitID of a bad id")
	}
}

func TestIngressRouteEdges(t *testing.T) {
	f := shop()
	gIR := gvr("traefik.io", "v1alpha1", "ingressroutes")
	f.add(gIR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"namespace": "shop", "name": "shop-http"},
		"spec": map[string]any{"routes": []any{
			map[string]any{"match": "Host(`shop.example.com`) && PathPrefix(`/api`)", "services": []any{map[string]any{"name": "api", "port": int64(80)}}},
			map[string]any{"match": "Host(`shop.example.com`)", "services": []any{map[string]any{"name": "gone", "port": int64(80)}}},
		}},
	}})
	g := Build(f)
	id := "IngressRoute.traefik.io/shop/shop-http"
	if !hasEdge(g, id, "Service/shop/api", EdgeRoutes) {
		t.Errorf("no IngressRoute → api edge: %+v", g.Out(id))
	}
	if n := node(t, g, "Service/shop/gone"); !n.Missing {
		t.Error("IngressRoute backend that does not exist must be Missing")
	}
	details := map[string]bool{}
	for _, e := range g.Out(id) {
		details[e.Detail] = true
	}
	if !details["shop.example.com/api"] || !details["shop.example.com/"] {
		t.Errorf("details = %v", details)
	}
}

func TestJSONRoundTripKeepsIndex(t *testing.T) {
	data, err := json.Marshal(Build(shop()))
	if err != nil {
		t.Fatal(err)
	}
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Node("Deployment/shop/web"); !ok || len(g.Out("Service/shop/web")) != 2 || g.Owner("Pod/shop/web-7d9f8-a") != "Deployment/shop/web" {
		t.Error("decoded graph has no working index")
	}
	if err := json.Unmarshal([]byte("[1]"), &g); err == nil {
		t.Error("bad JSON accepted")
	}
}
