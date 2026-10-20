package datastores

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return f[g], nil
}
func (f fakeSource) Generation() uint64 { return 1 }

var (
	gPods = gvr("", "v1", "pods")
	gSvc  = gvr("", "v1", "services")
	gPVC  = gvr("", "v1", "persistentvolumeclaims")
	gRS   = gvr("apps", "v1", "replicasets")
)

const ns = "sre-lab-app"

func own(apiVersion, kind, name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name}}
}

func om(name string, lbl map[string]string, owners []metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl, OwnerReferences: owners}
}

func i32(v int32) *int32 { return &v }

func running() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
}

func containers(specs ...[2]any) []corev1.Container {
	var out []corev1.Container
	for i, s := range specs {
		c := corev1.Container{Name: []string{"main", "side", "third"}[i], Image: s[0].(string)}
		for _, p := range s[1].([]int32) {
			c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: p})
		}
		out = append(out, c)
	}
	return out
}

func sts(name string, ready int32, cs ...[2]any) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: om(name, nil, nil), Spec: appsv1.StatefulSetSpec{Replicas: i32(1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers(cs...)}}}, Status: appsv1.StatefulSetStatus{ReadyReplicas: ready}}
}

func deploy(name string, owners []metav1.OwnerReference, cs ...[2]any) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: om(name, nil, owners), Spec: appsv1.DeploymentSpec{Replicas: i32(1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers(cs...)}}}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
}

// lab mirrors sre-lab-app, plus a few traps and edge cases.
func lab() fakeSource {
	f := fakeSource{}
	f[cnpgClusters] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": ns, "name": "url-shortener-db"},
		"spec":     map[string]any{"instances": int64(2)},
		"status": map[string]any{"readyInstances": int64(2), "phase": "Cluster in healthy state",
			"currentPrimary": "url-shortener-db-2", "image": "ghcr.io/cloudnative-pg/postgresql:16"},
	}}}
	f[cnpgPoolers] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Pooler",
		"metadata": map[string]any{"namespace": ns, "name": "url-shortener-pooler"},
		"spec":     map[string]any{"cluster": map[string]any{"name": "url-shortener-db"}, "type": "rw", "instances": int64(1)},
	}}}
	cnpg := own("postgresql.cnpg.io/v1", "Cluster", "url-shortener-db")
	pooler := own("postgresql.cnpg.io/v1", "Pooler", "url-shortener-pooler")
	dbLabels := map[string]string{"cnpg.io/cluster": "url-shortener-db"}
	f[gPods] = []runtime.Object{
		&corev1.Pod{ObjectMeta: om("url-shortener-db-1", dbLabels, cnpg), Status: running(),
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "pgdata", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "url-shortener-db-1"}}}}}},
		&corev1.Pod{ObjectMeta: om("url-shortener-db-2", dbLabels, cnpg), Status: running()},
		&corev1.Pod{ObjectMeta: om("url-shortener-pooler-x", map[string]string{"cnpg.io/poolerName": "url-shortener-pooler"}, own("apps/v1", "ReplicaSet", "url-shortener-pooler-rs")), Status: running()},
		&corev1.Pod{ObjectMeta: om("redis-master-0", map[string]string{"app.kubernetes.io/name": "redis"}, own("apps/v1", "StatefulSet", "redis-master")), Status: running()},
	}
	f[gRS] = []runtime.Object{&appsv1.ReplicaSet{ObjectMeta: om("url-shortener-pooler-rs", nil, own("apps/v1", "Deployment", "url-shortener-pooler"))}}
	f[gPVC] = []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: om("url-shortener-db-1", dbLabels, cnpg)}}
	f[gSvc] = []runtime.Object{
		&corev1.Service{ObjectMeta: om("url-shortener-db-rw", nil, cnpg), Spec: corev1.ServiceSpec{Selector: map[string]string{"cnpg.io/cluster": "url-shortener-db"}}},
		&corev1.Service{ObjectMeta: om("url-shortener-pooler", nil, pooler), Spec: corev1.ServiceSpec{Selector: map[string]string{"cnpg.io/poolerName": "url-shortener-pooler"}}},
		&corev1.Service{ObjectMeta: om("redis-master", nil, nil), Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/name": "redis"}}},
	}
	f[statefulSets] = []runtime.Object{
		// Bitnami Redis with its exporter sidecar.
		sts("redis-master", 1, [2]any{"docker.io/bitnamilegacy/redis:latest", []int32{6379}}, [2]any{"docker.io/bitnamilegacy/redis-exporter:1.76.0-debian-12-r0", []int32{9121}}),
		// Observability stores are not application databases.
		sts("prometheus-kube-prometheus-stack-prometheus", 1, [2]any{"quay.io/prometheus/prometheus:v2.53.1", []int32{9090}}),
		sts("tempo", 1, [2]any{"docker.io/grafana/tempo:2.9.0", []int32{3200, 4317}}),
		// A replicated Postgres from a "-" variant image name.
		sts("pg-ha", 1, [2]any{"bitnami/postgresql-repmgr:15.4.0", []int32{5432}}),
		// A custom image: only the port tells.
		sts("legacy-db", 0, [2]any{"registry.example.com/team/storage-svc:2024.1", []int32{8080, 3306}}),
		// A digest-pinned image.
		sts("sessions", 1, [2]any{"mongo@sha256:0000000000000000000000000000000000000000000000000000000000000000", []int32{27017}}),
	}
	f[deployments] = []runtime.Object{
		// Argo CD's cache.
		deploy("argocd-redis", nil, [2]any{"public.ecr.aws/docker/library/redis:8.2.3-alpine", []int32{6379}}),
		// Created by the CNPG Pooler: covered by the Pooler resource.
		deploy("url-shortener-pooler", pooler, [2]any{"ghcr.io/cloudnative-pg/pgbouncer:1.25.1", []int32{5432, 9127}}),
		// An exporter alone is not a database, even with a DB-like name.
		deploy("pg-exporter", nil, [2]any{"quay.io/prometheuscommunity/postgres-exporter:v0.15.0", []int32{9187}}),
		// The application itself.
		deploy("url-shortener", nil, [2]any{"docker.io/library/url-shortener:v0.2", []int32{8080}}),
	}
	return f
}

func detect(t *testing.T) map[string]Datastore {
	t.Helper()
	f := lab()
	g := graph.Build(f)
	dependencies := []deps.Dependency{
		{From: "Deployment/sre-lab-app/url-shortener", To: "Service/sre-lab-app/url-shortener-pooler", Via: "env DB_HOST"},
		{From: "Deployment/sre-lab-app/url-shortener", To: "Service/sre-lab-app/redis-master", Via: "env REDIS_HOST"},
	}
	out := map[string]Datastore{}
	for _, d := range Detect(f, g, dependencies) {
		out[d.ID] = d
	}
	return out
}

func TestDetect(t *testing.T) {
	got := detect(t)
	want := map[string]struct {
		engine string
		cat    Category
		conf   Confidence
	}{
		"Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db":    {"postgres", CategoryDatabase, ConfidenceOperator},
		"Pooler.postgresql.cnpg.io/sre-lab-app/url-shortener-pooler": {"pgbouncer", CategoryPooler, ConfidenceOperator},
		"StatefulSet/sre-lab-app/redis-master":                       {"redis", CategoryCache, ConfidenceImage},
		"Deployment/sre-lab-app/argocd-redis":                        {"redis", CategoryCache, ConfidenceImage},
		"StatefulSet/sre-lab-app/pg-ha":                              {"postgres", CategoryDatabase, ConfidenceImage},
		"StatefulSet/sre-lab-app/legacy-db":                          {"mysql", CategoryDatabase, ConfidencePort},
		"StatefulSet/sre-lab-app/sessions":                           {"mongodb", CategoryDatabase, ConfidenceImage},
	}
	for id, w := range want {
		d, ok := got[id]
		if !ok {
			t.Errorf("not detected: %s", id)
			continue
		}
		if d.Engine != w.engine || d.Category != w.cat || d.Confidence != w.conf {
			t.Errorf("%s = %s/%s/%s, want %s/%s/%s", id, d.Engine, d.Category, d.Confidence, w.engine, w.cat, w.conf)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("false positive: %s %+v", id, got[id])
		}
	}
}

func TestCNPGDetails(t *testing.T) {
	got := detect(t)
	db := got["Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db"]
	if db.Ready != 2 || db.Desired != 2 || db.Primary != "url-shortener-db-2" || db.Version != "16" ||
		db.Status != "Cluster in healthy state" || !db.Persistent || db.Operator != "cloudnative-pg" {
		t.Errorf("db = %+v", db)
	}
	if !slices.Equal(db.Services, []string{"Service/sre-lab-app/url-shortener-db-rw"}) {
		t.Errorf("db services = %v", db.Services)
	}
	p := got["Pooler.postgresql.cnpg.io/sre-lab-app/url-shortener-pooler"]
	if p.Target != "Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db" || p.Ready != 1 || p.Desired != 1 {
		t.Errorf("pooler = %+v", p)
	}
	if !slices.Equal(p.Clients, []string{"Deployment/sre-lab-app/url-shortener"}) {
		t.Errorf("pooler clients = %v", p.Clients)
	}
	if !slices.Contains(p.Evidence, "type rw") {
		t.Errorf("pooler evidence = %v", p.Evidence)
	}
}

func TestRedisDetails(t *testing.T) {
	got := detect(t)
	r := got["StatefulSet/sre-lab-app/redis-master"]
	if r.Version != "" || r.Ready != 1 || !slices.Equal(r.Clients, []string{"Deployment/sre-lab-app/url-shortener"}) ||
		!slices.Equal(r.Services, []string{"Service/sre-lab-app/redis-master"}) {
		t.Errorf("redis = %+v", r)
	}
	if a := got["Deployment/sre-lab-app/argocd-redis"]; a.Version != "8.2.3" || len(a.Clients) != 0 {
		t.Errorf("argocd-redis = %+v", a)
	}
	if l := got["StatefulSet/sre-lab-app/legacy-db"]; l.Ready != 0 || l.Evidence[0] != "container main port 3306" {
		t.Errorf("legacy-db = %+v", l)
	}
}

func TestImageHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io/bitnamilegacy/redis:latest": "redis",
		"redis":                                "redis",
		"localhost:5000/team/Redis:7":          "redis",
		"mongo@sha256:abc":                     "mongo",
		"ghcr.io/cloudnative-pg/postgresql:16.2-bookworm": "postgresql",
	} {
		if got := imageBase(in); got != want {
			t.Errorf("imageBase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"redis:8.2.3-alpine":        "8.2.3",
		"postgres:16":               "16",
		"redis:latest":              "",
		"redis":                     "",
		"localhost:5000/redis":      "",
		"mongo:7@sha256:abc":        "7",
		"bitnami/postgresql:15_ubi": "15",
	} {
		if got := version(in); got != want {
			t.Errorf("version(%q) = %q, want %q", in, got, want)
		}
	}
	for base, want := range map[string]bool{"redis-exporter": true, "postgres-operator": true, "kafka-ui": true,
		"redis-cluster": false, "postgresql-repmgr": false, "redis": false} {
		if helperImage(base) != want {
			t.Errorf("helperImage(%s) = %v", base, !want)
		}
	}
}

func TestNoGraph(t *testing.T) {
	// Without a graph, detection still works; relations stay empty.
	ds := Detect(lab(), nil, nil)
	if len(ds) == 0 {
		t.Fatal("nothing detected")
	}
	for _, d := range ds {
		if len(d.Services) != 0 || len(d.Clients) != 0 {
			t.Errorf("relations without graph: %+v", d)
		}
	}
}
