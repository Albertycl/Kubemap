package deps

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/graph"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return f[g], nil
}
func (f fakeSource) Generation() uint64 { return 1 }

var (
	gDeploy = gvr("apps", "v1", "deployments")
	gSTS    = gvr("apps", "v1", "statefulsets")
	gCron   = gvr("batch", "v1", "cronjobs")
	gJob    = gvr("batch", "v1", "jobs")
	gPods   = gvr("", "v1", "pods")
)

func om(ns, name string, lbl map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl}
}

func svc(ns, name string, sel map[string]string) *corev1.Service {
	return &corev1.Service{ObjectMeta: om(ns, name, nil), Spec: corev1.ServiceSpec{Selector: sel}}
}

func cmRef(cm, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: cm}, Key: key}}
}

func deploy(ns, name string, lbl map[string]string, c corev1.Container, vols ...corev1.Volume) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: om(ns, name, lbl), Spec: appsv1.DeploymentSpec{
		Template: corev1.PodTemplateSpec{ObjectMeta: om(ns, "", lbl), Spec: corev1.PodSpec{Containers: []corev1.Container{c}, Volumes: vols}}}}
}

// lab mirrors sre-lab-app: url-shortener reads its DB and Redis hosts from a
// ConfigMap, flask-vue sends traces to the collector in another namespace.
func lab() fakeSource {
	const ns = "sre-lab-app"
	f := fakeSource{}
	f[servicesGVR] = []runtime.Object{
		svc(ns, "url-shortener-pooler", map[string]string{"cnpg.io/poolerName": "url-shortener-pooler"}),
		svc(ns, "redis-master", map[string]string{"app.kubernetes.io/name": "redis"}),
		svc(ns, "flask-vue-backend", map[string]string{"app": "flask-vue-backend"}),
		svc(ns, "url-shortener", map[string]string{"app": "url-shortener"}),
		svc("observability", "otel-collector", map[string]string{"app": "otel"}),
		svc("shop", "api", map[string]string{"app": "api"}),
	}
	f[configMapsGVR] = []runtime.Object{
		&corev1.ConfigMap{ObjectMeta: om(ns, "url-shortener-db-config", nil), Data: map[string]string{
			"DB_HOST": "url-shortener-pooler", "DB_PORT": "5432", "DB_NAME": "urlshortener",
			"REDIS_HOST": "redis-master", "REDIS_PORT": "6379", "CACHE_TTL": "300"}},
		&corev1.ConfigMap{ObjectMeta: om(ns, "web-env", nil), Data: map[string]string{
			"API_URL": "http://flask-vue-backend:5000/api", "TITLE": "shop"}},
		&corev1.ConfigMap{ObjectMeta: om(ns, "nginx-conf", nil), Data: map[string]string{
			"default.conf": "server {\n  location /api { proxy_pass http://flask-vue-backend:5000; }\n  location /old { proxy_pass http://legacy-api:8080; }\n}\n"}},
	}
	f[gDeploy] = []runtime.Object{
		deploy(ns, "url-shortener", map[string]string{"app": "url-shortener"}, corev1.Container{Name: "app", Env: []corev1.EnvVar{
			{Name: "DB_HOST", ValueFrom: cmRef("url-shortener-db-config", "DB_HOST")},
			{Name: "DB_PORT", ValueFrom: cmRef("url-shortener-db-config", "DB_PORT")},
			{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password"}}},
			{Name: "REDIS_HOST", ValueFrom: cmRef("url-shortener-db-config", "REDIS_HOST")},
			{Name: "REDIS_PORT", ValueFrom: cmRef("url-shortener-db-config", "REDIS_PORT")},
			{Name: "BASE_URL", Value: "https://sre-lab.albertycliu.xyz"},
		}}),
		deploy(ns, "flask-vue-backend", map[string]string{"app": "flask-vue-backend"}, corev1.Container{Name: "backend", Env: []corev1.EnvVar{
			// The value equals a Service name, but the variable is not a host.
			{Name: "OTEL_SERVICE_NAME", Value: "flask-vue-backend"},
			{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: "http://otel-collector.observability.svc.cluster.local:4318/v1/traces"},
		}}),
		deploy(ns, "flask-vue-frontend", map[string]string{"app": "flask-vue-frontend"},
			corev1.Container{Name: "web", EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-env"}}}}},
			corev1.Volume{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "nginx-conf"}}}}),
		// A broken dependency: the Service was renamed.
		deploy(ns, "worker", map[string]string{"app": "worker"}, corev1.Container{Name: "w", Env: []corev1.EnvVar{
			{Name: "QUEUE_HOST", Value: "rabbitmq"},
			{Name: "API_ENDPOINT", Value: "api.shop.svc.cluster.local:8080"},
			{Name: "GONE_URL", Value: "http://old-svc.sre-lab-app.svc:80"},
		}}),
	}
	return f
}

func has(ds []Dependency, from, to, viaPart string) *Dependency {
	for i, d := range ds {
		if d.From == from && d.To == to && strings.Contains(d.Via, viaPart) {
			return &ds[i]
		}
	}
	return nil
}

func TestFind(t *testing.T) {
	ds := Find(lab())
	cases := []struct {
		from, to, via, port string
		missing             bool
	}{
		{"Deployment/sre-lab-app/url-shortener", "Service/sre-lab-app/url-shortener-pooler", "env DB_HOST (ConfigMap url-shortener-db-config)", "5432", false},
		{"Deployment/sre-lab-app/url-shortener", "Service/sre-lab-app/redis-master", "env REDIS_HOST (ConfigMap url-shortener-db-config)", "6379", false},
		{"Deployment/sre-lab-app/flask-vue-backend", "Service/observability/otel-collector", "env OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "4318", false},
		{"Deployment/sre-lab-app/flask-vue-frontend", "Service/sre-lab-app/flask-vue-backend", "envFrom API_URL (ConfigMap web-env)", "5000", false},
		{"Deployment/sre-lab-app/flask-vue-frontend", "Service/sre-lab-app/flask-vue-backend", "file default.conf (ConfigMap nginx-conf)", "5000", false},
		{"Deployment/sre-lab-app/worker", "Service/sre-lab-app/rabbitmq", "env QUEUE_HOST", "", true},
		{"Deployment/sre-lab-app/worker", "Service/shop/api", "env API_ENDPOINT", "8080", false},
		{"Deployment/sre-lab-app/worker", "Service/sre-lab-app/old-svc", "env GONE_URL", "80", true},
	}
	for _, c := range cases {
		d := has(ds, c.from, c.to, c.via)
		if d == nil {
			t.Errorf("missing dependency %s → %s via %s", c.from, c.to, c.via)
			continue
		}
		if d.Port != c.port || d.Missing != c.missing {
			t.Errorf("%s → %s: port %q missing %v, want %q %v", c.from, c.to, d.Port, d.Missing, c.port, c.missing)
		}
	}
	// Not dependencies: OTEL_SERVICE_NAME, the external BASE_URL, the DB
	// name, a file host that matches no Service, Secret values.
	for _, d := range ds {
		switch {
		case strings.Contains(d.Via, "OTEL_SERVICE_NAME"), strings.Contains(d.Via, "BASE_URL"),
			strings.Contains(d.Via, "DB_NAME"), strings.Contains(d.To, "legacy-api"), strings.Contains(d.Via, "PASSWORD"):
			t.Errorf("false dependency %+v", d)
		}
	}
	if len(ds) != len(cases) {
		t.Errorf("got %d dependencies, want %d: %+v", len(ds), len(cases), ds)
	}
}

func TestHosts(t *testing.T) {
	cases := []struct {
		value    string
		hostLike bool
		want     []string // host:port
	}{
		{"redis-master", true, []string{"redis-master:"}},
		{"redis-master", false, nil},
		{"db:5432", false, []string{"db:5432"}},
		{"postgres://app:secret@url-shortener-pooler:5432/urlshortener?sslmode=disable", false, []string{"url-shortener-pooler:5432"}},
		{"jdbc:postgresql://db.shop.svc:5432/app", false, []string{"db.shop.svc:5432"}},
		{"redis://:pw@redis-master:6379/0", false, []string{"redis-master:6379"}},
		{"kafka-0.kafka:9092,kafka-1.kafka:9092", false, []string{"kafka-0.kafka:9092", "kafka-1.kafka:9092"}},
		{"otel.observability.svc.cluster.local", false, []string{"otel.observability.svc.cluster.local:"}},
		{"user@mail-relay", true, []string{"mail-relay:"}},
		{"localhost:8080", false, nil},
		{"127.0.0.1:6379", true, nil},
		{"10.0.0.5", true, nil},
		{"Not A Host", true, nil},
		{"host:notaport", false, nil},
		{"http://", false, nil},
		{"", true, nil},
	}
	for _, c := range cases {
		var got []string
		for _, h := range hosts(c.value, c.hostLike) {
			got = append(got, h.host+":"+h.port)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("hosts(%q, %v) = %v, want %v", c.value, c.hostLike, got, c.want)
		}
	}
}

func TestServiceFor(t *testing.T) {
	services := map[string]bool{"a/web": true, "b/db": true}
	cases := []struct {
		host string
		want string // "ns/name" or "" for not a Service
	}{
		{"web", "a/web"},
		{"nothing", "a/nothing"}, // bare: can only be a Service in the pod's namespace
		{"db.b", "b/db"},
		{"example.com", ""},
		{"db.b.svc", "b/db"},
		{"db.b.svc.cluster.local", "b/db"},
		{"db.b.svc.corp.internal", "b/db"},
		{"db-0.db.b.svc.cluster.local", "b/db"},
		{"api.github.com", ""},
	}
	for _, c := range cases {
		ref, ok := serviceFor(c.host, "a", services)
		got := ""
		if ok {
			got = ref.ns + "/" + ref.name
		}
		if got != c.want {
			t.Errorf("serviceFor(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestTemplatesSkipOwnedPodsAndJobs(t *testing.T) {
	yes := true
	owned := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "x", Controller: &yes}
	f := fakeSource{
		gPods: {
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "owned", OwnerReferences: []metav1.OwnerReference{owned}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "bare"}},
		},
		gJob: {
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "from-cron", OwnerReferences: []metav1.OwnerReference{owned}}},
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "one-off"}},
		},
		gCron: {&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "nightly"}}},
		gSTS:  {&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "db"}}},
	}
	var ids []string
	for _, tpl := range templates(f) {
		ids = append(ids, tpl.id)
	}
	slices.Sort(ids)
	want := []string{"CronJob/a/nightly", "Job/a/one-off", "Pod/a/bare", "StatefulSet/a/db"}
	if !slices.Equal(ids, want) {
		t.Errorf("templates = %v, want %v", ids, want)
	}
}

func TestSiblingPort(t *testing.T) {
	v := map[string]string{"DB_PORT": "5432", "REDIS_PORT": "6379", "SMTP_PORT": "25"}
	for name, want := range map[string]string{"DB_HOST": "5432", "redis_host": "6379", "SMTP_SERVER": "25", "API_URL": "", "X_HOST": ""} {
		if got := siblingPort(name, v); got != want {
			t.Errorf("siblingPort(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestLinksAndEdges(t *testing.T) {
	ds := []Dependency{
		{From: "Deployment/ns/url-shortener", To: "Service/ns/url-shortener-pooler", Via: "env DB_HOST"},
		{From: "Deployment/ns/url-shortener", To: "Service/ns/redis-master", Via: "env REDIS_HOST"},
		{From: "Deployment/ns/url-shortener", To: "Service/ns/url-shortener", Via: "env SELF_URL"}, // inside the app
		{From: "Deployment/ns/url-shortener", To: "Service/ns/rabbitmq", Via: "env QUEUE_HOST", Missing: true},
		{From: "Deployment/ns/orphan", To: "Service/ns/redis-master", Via: "env X_HOST"}, // from outside any app
	}
	list := []apps.App{
		{ID: "argocd/url-shortener", Members: []string{"Deployment/ns/url-shortener", "Service/ns/url-shortener"}},
		{ID: "ns/url-shortener-db", Members: []string{"Service/ns/url-shortener-pooler"}},
		{ID: "ns/redis", Members: []string{"Service/ns/redis-master"}},
	}
	links := Links(ds, list)
	var got []string
	for _, l := range links {
		got = append(got, l.From+" -> "+l.To)
	}
	want := []string{
		"argocd/url-shortener -> Service/ns/rabbitmq",
		"argocd/url-shortener -> ns/redis",
		"argocd/url-shortener -> ns/url-shortener-db",
	}
	if !slices.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
	if !strings.Contains(links[2].Via[0], "env DB_HOST") {
		t.Errorf("via = %v", links[2].Via)
	}
	edges := Edges([]Dependency{{From: "a", To: "b", Via: "env DB_HOST", Port: "5432"}})
	if len(edges) != 1 || edges[0].Type != EdgeDependsOn || edges[0].Detail != "env DB_HOST port 5432" {
		t.Errorf("edges = %+v", edges)
	}
}

func TestEndToEndWithApps(t *testing.T) {
	f := lab()
	// Pods so Services select something and apps form.
	for _, o := range f[gDeploy] {
		d := o.(*appsv1.Deployment)
		f[gPods] = append(f[gPods], &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: d.Namespace, Name: d.Name + "-x", Labels: d.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name}}}})
	}
	g := graph.Build(f)
	links := Links(Find(f), apps.Group(f, g))
	found := false
	for _, l := range links {
		if l.From == "sre-lab-app/flask-vue-frontend" && l.To == "sre-lab-app/flask-vue-backend" {
			found = true
		}
	}
	if !found {
		t.Errorf("frontend → backend app link missing: %+v", links)
	}
}
