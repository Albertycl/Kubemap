package apps

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/cache"
	"kubemap/agent/cluster/graph"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	if objs, ok := f[g]; ok {
		return objs, nil
	}
	if g.Group != "" && g.Group != "apps" && g.Group != "batch" && !strings.Contains(g.Group, "k8s.io") && g.Group != "autoscaling" {
		return nil, fmt.Errorf("%w: absent", cache.ErrNotCached)
	}
	return nil, nil
}

func (f fakeSource) Generation() uint64 { return 1 }

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

var (
	gPods   = gvr("", "v1", "pods")
	gSvc    = gvr("", "v1", "services")
	gCM     = gvr("", "v1", "configmaps")
	gSA     = gvr("", "v1", "serviceaccounts")
	gPVC    = gvr("", "v1", "persistentvolumeclaims")
	gDeploy = gvr("apps", "v1", "deployments")
	gRS     = gvr("apps", "v1", "replicasets")
	gSTS    = gvr("apps", "v1", "statefulsets")
	gIng    = gvr("networking.k8s.io", "v1", "ingresses")
	gIR     = gvr("traefik.io", "v1alpha1", "ingressroutes")
	gCNPG   = gvr("postgresql.cnpg.io", "v1", "clusters")
	gPooler = gvr("postgresql.cnpg.io", "v1", "poolers")
)

func om(ns, name string, lbl map[string]string, owner ...metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl, OwnerReferences: owner}
}

func own(apiVersion, kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name}
}

func i32(v int32) *int32 { return &v }

func running() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
}

// deployment adds a Deployment with one ReplicaSet and one running pod.
func deployment(f fakeSource, ns, name string, lbl map[string]string, owner ...metav1.OwnerReference) {
	f[gDeploy] = append(f[gDeploy], &appsv1.Deployment{ObjectMeta: om(ns, name, lbl, owner...),
		Spec: appsv1.DeploymentSpec{Replicas: i32(1)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}})
	f[gRS] = append(f[gRS], &appsv1.ReplicaSet{ObjectMeta: om(ns, name+"-rs", lbl, own("apps/v1", "Deployment", name)), Spec: appsv1.ReplicaSetSpec{Replicas: i32(1)},
		Status: appsv1.ReplicaSetStatus{ReadyReplicas: 1}})
	f[gPods] = append(f[gPods], &corev1.Pod{ObjectMeta: om(ns, name+"-rs-x", lbl, own("apps/v1", "ReplicaSet", name+"-rs")), Status: running()})
	defaultSA(f, ns)
}

// defaultSA adds the "default" ServiceAccount every namespace has.
func defaultSA(f fakeSource, ns string) {
	for _, o := range f[gSA] {
		if o.(*corev1.ServiceAccount).Namespace == ns {
			return
		}
	}
	f[gSA] = append(f[gSA], &corev1.ServiceAccount{ObjectMeta: om(ns, "default", nil)})
}

func argoApplication(name string, health, sync string, resources ...[4]string) *unstructured.Unstructured {
	var rs []any
	for _, r := range resources {
		rs = append(rs, map[string]any{"group": r[0], "kind": r[1], "namespace": r[2], "name": r[3]})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": name},
		"status": map[string]any{"health": map[string]any{"status": health}, "sync": map[string]any{"status": sync},
			"resources": rs},
	}}
}

// lab mirrors the sre-lab-app namespace of the dev cluster.
func lab() fakeSource {
	f := fakeSource{}
	const ns = "sre-lab-app"
	fv := func(c string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": "flask-vue", "app.kubernetes.io/component": c}
	}
	deployment(f, ns, "flask-vue-backend", fv("backend"))
	deployment(f, ns, "flask-vue-frontend", fv("frontend"))
	deployment(f, ns, "url-shortener", map[string]string{"app": "url-shortener"})
	deployment(f, ns, "whoami", nil)
	f[gSvc] = append(f[gSvc],
		&corev1.Service{ObjectMeta: om(ns, "flask-vue-backend", nil), Spec: corev1.ServiceSpec{Selector: fv("backend")}},
		&corev1.Service{ObjectMeta: om(ns, "flask-vue-frontend", nil), Spec: corev1.ServiceSpec{Selector: fv("frontend")}},
		&corev1.Service{ObjectMeta: om(ns, "url-shortener", nil), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "url-shortener"}}},
		&corev1.Service{ObjectMeta: om(ns, "whoami", nil), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "whoami-typo"}}},
	)
	f[gIR] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"namespace": ns, "name": "flask-vue-http"}}}}
	f[argoApps] = []runtime.Object{
		argoApplication("flask-vue", "Healthy", "Synced",
			[4]string{"", "Service", ns, "flask-vue-backend"}, [4]string{"", "Service", ns, "flask-vue-frontend"},
			[4]string{"apps", "Deployment", ns, "flask-vue-backend"}, [4]string{"apps", "Deployment", ns, "flask-vue-frontend"},
			[4]string{"traefik.io", "IngressRoute", ns, "flask-vue-http"}),
		argoApplication("url-shortener", "Healthy", "OutOfSync",
			[4]string{"", "Service", ns, "url-shortener"}, [4]string{"apps", "Deployment", ns, "url-shortener"}),
	}
	// CNPG: the Cluster owns its instance pod and Services; the Pooler owns
	// a Deployment labelled with the database name.
	f[gCNPG] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": ns, "name": "url-shortener-db"}, "status": map[string]any{"phase": "Cluster in healthy state"}}}}
	f[gPooler] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Pooler",
		"metadata": map[string]any{"namespace": ns, "name": "url-shortener-pooler"}}}}
	f[gPods] = append(f[gPods], &corev1.Pod{ObjectMeta: om(ns, "url-shortener-db-1", map[string]string{"cnpg.io/cluster": "url-shortener-db"},
		own("postgresql.cnpg.io/v1", "Cluster", "url-shortener-db")), Status: running()})
	// The Cluster's PVC carries only a weak label and sorts first; the
	// Service's instance label must win.
	f[gPVC] = append(f[gPVC], &corev1.PersistentVolumeClaim{ObjectMeta: om(ns, "url-shortener-db-1", map[string]string{"app.kubernetes.io/name": "postgresql"},
		own("postgresql.cnpg.io/v1", "Cluster", "url-shortener-db"))})
	f[gSvc] = append(f[gSvc], &corev1.Service{ObjectMeta: om(ns, "url-shortener-db-rw", map[string]string{"app.kubernetes.io/instance": "url-shortener-db", "app.kubernetes.io/name": "postgresql"}, own("postgresql.cnpg.io/v1", "Cluster", "url-shortener-db")),
		Spec: corev1.ServiceSpec{Selector: map[string]string{"cnpg.io/cluster": "url-shortener-db"}}})
	deployment(f, ns, "url-shortener-pooler", map[string]string{"app.kubernetes.io/instance": "url-shortener-db", "app.kubernetes.io/managed-by": "cloudnative-pg"},
		own("postgresql.cnpg.io/v1", "Pooler", "url-shortener-pooler"))
	// Helm: redis.
	helm := map[string]string{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "redis", "app.kubernetes.io/name": "redis"}
	f[gSTS] = append(f[gSTS], &appsv1.StatefulSet{ObjectMeta: om(ns, "redis-master", helm), Spec: appsv1.StatefulSetSpec{Replicas: i32(1)},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}})
	f[gPods] = append(f[gPods], &corev1.Pod{ObjectMeta: om(ns, "redis-master-0", helm, own("apps/v1", "StatefulSet", "redis-master")),
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "redis-configuration"}}}}}}, Status: running()})
	f[gCM] = append(f[gCM], &corev1.ConfigMap{ObjectMeta: om(ns, "redis-configuration", helm)})
	// A label app of two parts, one failing, reached by an Ingress.
	shop := func(c string) map[string]string {
		return map[string]string{"app.kubernetes.io/part-of": "shop", "app.kubernetes.io/name": c}
	}
	deployment(f, "shop", "shop-web", shop("web"))
	f[gDeploy] = append(f[gDeploy], &appsv1.Deployment{ObjectMeta: om("shop", "shop-api", shop("api")), Spec: appsv1.DeploymentSpec{Replicas: i32(1)}})
	f[gRS] = append(f[gRS], &appsv1.ReplicaSet{ObjectMeta: om("shop", "shop-api-rs", shop("api"), own("apps/v1", "Deployment", "shop-api")), Spec: appsv1.ReplicaSetSpec{Replicas: i32(1)}})
	f[gPods] = append(f[gPods], &corev1.Pod{ObjectMeta: om("shop", "shop-api-rs-x", shop("api"), own("apps/v1", "ReplicaSet", "shop-api-rs")),
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}})
	f[gSvc] = append(f[gSvc], &corev1.Service{ObjectMeta: om("shop", "shop-web", nil), Spec: corev1.ServiceSpec{Selector: shop("web")}})
	f[gIng] = append(f[gIng], &networkingv1.Ingress{ObjectMeta: om("shop", "shop", nil), Spec: networkingv1.IngressSpec{
		DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "shop-web"}}}})
	// System: CoreDNS without labels.
	deployment(f, "kube-system", "coredns", map[string]string{"k8s-app": "kube-dns"})
	return f
}

func group(t *testing.T, f fakeSource) map[string]App {
	t.Helper()
	out := map[string]App{}
	for _, a := range Group(f, graph.Build(f)) {
		if _, dup := out[a.ID]; dup {
			t.Fatalf("duplicate app %s", a.ID)
		}
		out[a.ID] = a
	}
	return out
}

func TestGrouping(t *testing.T) {
	apps := group(t, lab())
	want := map[string]struct {
		source    Source
		workloads []string
	}{
		"argocd/flask-vue":     {SourceArgoCD, []string{"Deployment/sre-lab-app/flask-vue-backend", "Deployment/sre-lab-app/flask-vue-frontend"}},
		"argocd/url-shortener": {SourceArgoCD, []string{"Deployment/sre-lab-app/url-shortener"}},
		"sre-lab-app/url-shortener-db": {SourceLabel, []string{
			"Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db", "Pooler.postgresql.cnpg.io/sre-lab-app/url-shortener-pooler"}},
		"sre-lab-app/redis":   {SourceHelm, []string{"StatefulSet/sre-lab-app/redis-master"}},
		"sre-lab-app/whoami":  {SourceWorkload, []string{"Deployment/sre-lab-app/whoami"}},
		"shop/shop":           {SourceLabel, []string{"Deployment/shop/shop-api", "Deployment/shop/shop-web"}},
		"kube-system/coredns": {SourceWorkload, []string{"Deployment/kube-system/coredns"}},
	}
	for id, w := range want {
		a, ok := apps[id]
		if !ok {
			t.Errorf("no app %s (have %v)", id, keys(apps))
			continue
		}
		if a.Source != w.source || !slices.Equal(a.Workloads, w.workloads) {
			t.Errorf("%s: source %s workloads %v, want %s %v", id, a.Source, a.Workloads, w.source, w.workloads)
		}
	}
	if len(apps) != len(want) {
		t.Errorf("apps = %v", keys(apps))
	}
}

func keys(m map[string]App) []string {
	var k []string
	for id := range m {
		k = append(k, id)
	}
	slices.Sort(k)
	return k
}

func TestMembers(t *testing.T) {
	apps := group(t, lab())
	has := func(id string, members ...string) {
		t.Helper()
		for _, m := range members {
			if !slices.Contains(apps[id].Members, m) {
				t.Errorf("%s lacks member %s (has %v)", id, m, apps[id].Members)
			}
		}
	}
	has("argocd/flask-vue",
		"ReplicaSet/sre-lab-app/flask-vue-backend-rs", "Pod/sre-lab-app/flask-vue-backend-rs-x",
		"Service/sre-lab-app/flask-vue-backend", "Service/sre-lab-app/flask-vue-frontend",
		"IngressRoute.traefik.io/sre-lab-app/flask-vue-http")
	has("sre-lab-app/url-shortener-db", "Pod/sre-lab-app/url-shortener-db-1", "Service/sre-lab-app/url-shortener-db-rw",
		"Deployment/sre-lab-app/url-shortener-pooler")
	has("sre-lab-app/redis", "Pod/sre-lab-app/redis-master-0", "ConfigMap/sre-lab-app/redis-configuration")
	has("shop/shop", "Service/shop/shop-web", "Ingress/shop/shop", "Pod/shop/shop-api-rs-x")
	// Workloads come first in Members.
	a := apps["shop/shop"]
	if !slices.Equal(a.Members[:len(a.Workloads)], a.Workloads) {
		t.Errorf("members do not start with workloads: %v", a.Members)
	}
	// No leaking between apps.
	if slices.Contains(apps["argocd/url-shortener"].Members, "Pod/sre-lab-app/url-shortener-db-1") {
		t.Error("database pod leaked into url-shortener (that is a dependency, KM-50)")
	}
}

func TestHealth(t *testing.T) {
	apps := group(t, lab())
	for id, want := range map[string]string{
		"argocd/flask-vue":             "Healthy, Synced",
		"argocd/url-shortener":         "Healthy, OutOfSync",
		"sre-lab-app/redis":            "Healthy",
		"sre-lab-app/url-shortener-db": "Healthy",
		"shop/shop":                    "Degraded",
		"sre-lab-app/whoami":           "Degraded",
	} {
		if got := apps[id].Health; got != want {
			t.Errorf("%s health = %q, want %q (problems %v)", id, got, want, apps[id].Problems)
		}
	}
	shop := strings.Join(apps["shop/shop"].Problems, "; ")
	if !strings.Contains(shop, "Pod/shop/shop-api-rs-x: CrashLoopBackOff") || !strings.Contains(shop, "Deployment/shop/shop-api: 0/1 ready") {
		t.Errorf("shop problems = %s", shop)
	}
	// whoami's Service selector has a typo, so nothing links it to the
	// Deployment; it is adopted by name and makes the app Degraded.
	if p := apps["sre-lab-app/whoami"].Problems; len(p) != 1 || p[0] != "Service/sre-lab-app/whoami: no pods match selector" {
		t.Errorf("whoami problems = %v", p)
	}
}

func TestSystemAndNamespace(t *testing.T) {
	apps := group(t, lab())
	if !apps["kube-system/coredns"].System || apps["argocd/flask-vue"].System {
		t.Error("system flag")
	}
	if apps["argocd/flask-vue"].Namespace != "sre-lab-app" {
		t.Errorf("argo app namespace = %q", apps["argocd/flask-vue"].Namespace)
	}
	for ns, want := range map[string]bool{"kube-system": true, "calico-system": true, "tigera-operator": true,
		"cnpg-system": true, "monitoring": false, "sre-lab-app": false, "default": false} {
		if isSystem(ns) != want {
			t.Errorf("isSystem(%s) = %v", ns, !want)
		}
	}
}

func TestArgoAppWithoutWorkloads(t *testing.T) {
	f := fakeSource{argoApps: {argoApplication("gone", "Missing", "OutOfSync", [4]string{"apps", "Deployment", "x", "deleted"})}}
	apps := group(t, f)
	a, ok := apps["argocd/gone"]
	if !ok || a.Health != "Missing, OutOfSync" || len(a.Members) != 0 {
		t.Errorf("gone app = %+v", a)
	}
}

func TestMissingMemberHealth(t *testing.T) {
	f := fakeSource{}
	deployment(f, "a", "web", map[string]string{"app": "web"})
	p := f[gPods][0].(*corev1.Pod)
	p.Spec.Volumes = []corev1.Volume{{Name: "c", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "never-created"}}}}}
	a := group(t, f)["a/web"]
	if a.Health != "Missing" || !slices.Contains(a.Problems, "ConfigMap/a/never-created: missing") {
		t.Errorf("app = %+v", a)
	}
}

func TestNoArgo(t *testing.T) {
	// Without Argo CD, flask-vue groups by its app.kubernetes.io/name label.
	f := lab()
	delete(f, argoApps)
	apps := group(t, f)
	a, ok := apps["sre-lab-app/flask-vue"]
	if !ok || a.Source != SourceLabel || len(a.Workloads) != 2 {
		t.Errorf("flask-vue without argo = %+v", a)
	}
	if u := apps["sre-lab-app/url-shortener"]; u.Source != SourceLabel {
		t.Errorf("url-shortener = %+v", u)
	}
}

func TestHelpers(t *testing.T) {
	if !notAllReady("1/3 ready") || notAllReady("3/3 ready") || notAllReady("odd") {
		t.Error("notAllReady")
	}
	if got := uniqSorted([]string{"b", "a", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("uniqSorted = %v", got)
	}
	for s, want := range map[string]bool{"Running": false, "Pending": false, "": false, "Succeeded": false, "OOMKilled": true, "Running, not ready": true} {
		if bad(s) != want {
			t.Errorf("bad(%q) = %v", s, !want)
		}
	}
}
