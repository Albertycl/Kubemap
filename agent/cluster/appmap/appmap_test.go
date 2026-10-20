package appmap

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/datastores"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/routes"
	"kubemap/pkg/protocol"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return f[g], nil
}
func (f fakeSource) Generation() uint64 { return 1 }

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

const ns = "shop"

func own(apiVersion, kind, name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name}}
}

func om(namespace, name string, lbl map[string]string, owners []metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: lbl, OwnerReferences: owners}
}

func i32(v int32) *int32 { return &v }

func running() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
}

func crashing(restarts int32) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "main", RestartCount: restarts,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}
}

func deploy(namespace, name string, lbl map[string]string, owners []metav1.OwnerReference, ready int32, image string, env []corev1.EnvVar, vols ...corev1.Volume) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: om(namespace, name, lbl, owners), Spec: appsv1.DeploymentSpec{Replicas: i32(1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: vols,
			Containers: []corev1.Container{{Name: "main", Image: image, Env: env}}}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready}}
}

func svc(namespace, name string, sel map[string]string, owners []metav1.OwnerReference) *corev1.Service {
	return &corev1.Service{ObjectMeta: om(namespace, name, nil, owners), Spec: corev1.ServiceSpec{Selector: sel}}
}

// shop is a small web shop with every kind of link and a few faults:
//
//	IngressRoute shop: shop.example.com/     → storefront (no name hint: routed, calls no store → frontend)
//	                   shop.example.com/api  → orders     (routed, calls stores → backend)
//	Ingress legacy:    /old → Service ghost, which does not exist
//	storefront → orders       env API_URL (literal)
//	storefront → cart         env CART_HOST: Service cart selects no pods
//	orders → shop-pooler      env DB_HOST from ConfigMap orders-config key db-host
//	orders → session-store    env REDIS_HOST literal; Redis image → cache
//	orders → obs/collector    OTLP endpoint, another namespace
//	mailer → orders           file app.yaml (ConfigMap mailer-conf); label component=worker
//	shop-pooler → shop-db     CNPG Pooler → Cluster (operator); shop-db-1 is crashing
func shop() fakeSource {
	part := map[string]string{"app.kubernetes.io/part-of": "shop"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"app.kubernetes.io/part-of": "shop"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cnpg := own("postgresql.cnpg.io/v1", "Cluster", "shop-db")
	pooler := own("postgresql.cnpg.io/v1", "Pooler", "shop-pooler")
	rs := func(name string) []metav1.OwnerReference { return own("apps/v1", "ReplicaSet", name) }
	dep := func(name string) []metav1.OwnerReference { return own("apps/v1", "Deployment", name) }
	cmVol := func(name string) corev1.Volume {
		return corev1.Volume{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}
	}
	prefix := "PathPrefix"
	f := fakeSource{}
	f[gvr("apps", "v1", "deployments")] = []runtime.Object{
		deploy(ns, "storefront", with(map[string]string{"app": "storefront"}), nil, 1, "shop/storefront:1", []corev1.EnvVar{
			{Name: "API_URL", Value: "http://orders:8080"}, {Name: "CART_HOST", Value: "cart"}}),
		deploy(ns, "orders", with(map[string]string{"app": "orders"}), nil, 0, "shop/orders:1", []corev1.EnvVar{
			{Name: "DB_HOST", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "orders-config"}, Key: "db-host"}}},
			{Name: "DB_PORT", Value: "5432"},
			{Name: "REDIS_HOST", Value: "session-store"},
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://collector.obs.svc:4318"}}),
		deploy(ns, "mailer", with(map[string]string{"app": "mailer", "app.kubernetes.io/component": "worker"}), nil, 1, "shop/mailer:1", nil, cmVol("mailer-conf")),
		deploy(ns, "shop-pooler", nil, pooler, 1, "ghcr.io/cloudnative-pg/pgbouncer:1.25.1", nil),
		deploy("obs", "collector", map[string]string{"app": "collector"}, nil, 1, "otel/opentelemetry-collector:0.110.0", nil),
	}
	f[gvr("apps", "v1", "statefulsets")] = []runtime.Object{
		&appsv1.StatefulSet{ObjectMeta: om(ns, "session-store", map[string]string{"app": "session-store"}, nil), Spec: appsv1.StatefulSetSpec{Replicas: i32(1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "redis:7.2.4"}}}}},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}},
	}
	f[gvr("apps", "v1", "replicasets")] = []runtime.Object{
		&appsv1.ReplicaSet{ObjectMeta: om(ns, "storefront-rs", nil, dep("storefront"))},
		&appsv1.ReplicaSet{ObjectMeta: om(ns, "orders-rs", nil, dep("orders"))},
		&appsv1.ReplicaSet{ObjectMeta: om(ns, "mailer-rs", nil, dep("mailer"))},
		&appsv1.ReplicaSet{ObjectMeta: om(ns, "shop-pooler-rs", nil, dep("shop-pooler"))},
		&appsv1.ReplicaSet{ObjectMeta: om("obs", "collector-rs", nil, dep("collector"))},
	}
	pod := func(namespace, name string, lbl map[string]string, owners []metav1.OwnerReference, st corev1.PodStatus) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: om(namespace, name, lbl, owners), Spec: corev1.PodSpec{NodeName: "k8server"}, Status: st}
	}
	dbLabels := map[string]string{"cnpg.io/cluster": "shop-db"}
	f[gvr("", "v1", "pods")] = []runtime.Object{
		pod(ns, "storefront-1", map[string]string{"app": "storefront"}, rs("storefront-rs"), running()),
		pod(ns, "orders-1", map[string]string{"app": "orders"}, rs("orders-rs"), crashing(5)),
		pod(ns, "mailer-1", map[string]string{"app": "mailer"}, rs("mailer-rs"), running()),
		pod(ns, "shop-pooler-1", map[string]string{"cnpg.io/poolerName": "shop-pooler"}, rs("shop-pooler-rs"), running()),
		pod(ns, "shop-db-1", dbLabels, cnpg, crashing(3)),
		pod(ns, "shop-db-2", dbLabels, cnpg, running()),
		pod(ns, "session-store-0", map[string]string{"app": "session-store"}, own("apps/v1", "StatefulSet", "session-store"), running()),
		pod("obs", "collector-1", map[string]string{"app": "collector"}, rs("collector-rs"), running()),
	}
	f[gvr("", "v1", "services")] = []runtime.Object{
		svc(ns, "storefront", map[string]string{"app": "storefront"}, nil),
		svc(ns, "orders", map[string]string{"app": "orders"}, nil),
		svc(ns, "cart", map[string]string{"app": "cart"}, nil),            // nothing runs behind it
		svc(ns, "mailer-metrics", map[string]string{"app": "mailr"}, nil), // a selector typo nobody links to
		svc(ns, "mailer", map[string]string{"app": "mailr"}, nil),         // the same typo, adopted by name
		svc(ns, "session-store", map[string]string{"app": "session-store"}, nil),
		svc(ns, "shop-pooler", map[string]string{"cnpg.io/poolerName": "shop-pooler"}, pooler),
		svc(ns, "shop-db-rw", dbLabels, cnpg),
		svc("obs", "collector", map[string]string{"app": "collector"}, nil),
	}
	// storefront has a ready endpoint; the crashing orders pod has none, so
	// /api answers 503.
	slice := func(name string, ready bool) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-x",
			Labels: map[string]string{discoveryv1.LabelServiceName: name}},
			Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}}
	}
	f[gvr("discovery.k8s.io", "v1", "endpointslices")] = []runtime.Object{slice("storefront", true), slice("orders", false)}
	f[gvr("", "v1", "configmaps")] = []runtime.Object{
		&corev1.ConfigMap{ObjectMeta: om(ns, "orders-config", part, nil), Data: map[string]string{"db-host": "shop-pooler"}},
		&corev1.ConfigMap{ObjectMeta: om(ns, "mailer-conf", part, nil), Data: map[string]string{"app.yaml": "orders:\n  url: http://orders:8080/api\n"}},
	}
	f[gvr("postgresql.cnpg.io", "v1", "clusters")] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": ns, "name": "shop-db"},
		"spec":     map[string]any{"instances": int64(2)},
		"status": map[string]any{"readyInstances": int64(1), "phase": "Waiting for the instances to become active",
			"currentPrimary": "shop-db-2", "image": "ghcr.io/cloudnative-pg/postgresql:16.4"},
	}}}
	f[gvr("postgresql.cnpg.io", "v1", "poolers")] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Pooler",
		"metadata": map[string]any{"namespace": ns, "name": "shop-pooler"},
		"spec":     map[string]any{"cluster": map[string]any{"name": "shop-db"}, "instances": int64(1)},
	}}}
	f[gvr("traefik.io", "v1alpha1", "ingressroutes")] = []runtime.Object{&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"namespace": ns, "name": "shop"},
		"spec": map[string]any{"entryPoints": []any{"websecure"}, "tls": map[string]any{}, "routes": []any{
			map[string]any{"match": "Host(`shop.example.com`) && PathPrefix(`/api`)", "kind": "Rule",
				"services": []any{map[string]any{"name": "orders", "port": int64(8080)}}},
			map[string]any{"match": "Host(`shop.example.com`)", "kind": "Rule",
				"services": []any{map[string]any{"name": "storefront", "port": int64(80)}}},
		}}}}}
	f[gvr("networking.k8s.io", "v1", "ingresses")] = []runtime.Object{&networkingv1.Ingress{
		ObjectMeta: om(ns, "legacy", nil, nil),
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "old.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/old", PathType: (*networkingv1.PathType)(&prefix),
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "ghost",
					Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}}},
	}}
	return f
}

func analyse(f fakeSource) Input {
	g := graph.Build(f)
	dependencies := deps.Find(f)
	table := routes.Table(f)
	pods := map[string]*corev1.Pod{}
	for _, o := range f[gvr("", "v1", "pods")] {
		p := o.(*corev1.Pod)
		pods[p.Namespace+"/"+p.Name] = p
	}
	return Input{Graph: g, Apps: apps.Group(f, g), Dependencies: dependencies, Routes: table,
		RouteFindings: routes.Check(table), Datastores: datastores.Detect(f, g, dependencies),
		Pod: func(namespace, name string) *corev1.Pod { return pods[namespace+"/"+name] }}
}

// index returns every component of the map (apps, peers, unassigned) by ID.
func index(m *protocol.AppMap) map[string]protocol.MapComponent {
	out := map[string]protocol.MapComponent{}
	for _, a := range m.Apps {
		for _, c := range a.Components {
			out[c.ID] = c
		}
	}
	for _, c := range append(m.Peers, m.Unassigned...) {
		out[c.ID] = c
	}
	return out
}

func linkOf(m *protocol.AppMap, from, to string) (protocol.MapLink, bool) {
	for _, l := range m.Links {
		if l.From == from && l.To == to {
			return l, true
		}
	}
	return protocol.MapLink{}, false
}

const (
	entry      = "IngressRoute.traefik.io/shop/shop"
	legacy     = "Ingress/shop/legacy"
	storefront = "Deployment/shop/storefront"
	orders     = "Deployment/shop/orders"
	mailer     = "Deployment/shop/mailer"
	pooler     = "Pooler.postgresql.cnpg.io/shop/shop-pooler"
	db         = "Cluster.postgresql.cnpg.io/shop/shop-db"
	cache      = "StatefulSet/shop/session-store"
	collector  = "Deployment/obs/collector"
	cart       = "Service/shop/cart"
	ghost      = "Service/shop/ghost"
)

func TestTiers(t *testing.T) {
	comps := index(Build(analyse(shop())))
	for id, want := range map[string]struct {
		tier   protocol.Tier
		reason string
	}{
		entry:      {protocol.TierEntry, "IngressRoute.traefik.io rules"},
		legacy:     {protocol.TierEntry, "Ingress rules"},
		storefront: {protocol.TierFrontend, "receives traffic from an entry"},
		orders:     {protocol.TierBackend, "receives traffic from an entry and calls a data store"},
		mailer:     {protocol.TierBackend, "label app.kubernetes.io/component=worker"},
		pooler:     {protocol.TierDatabase, "pgbouncer: CloudNativePG Pooler"},
		db:         {protocol.TierDatabase, "postgres: CloudNativePG Cluster"},
		cache:      {protocol.TierCache, "redis"},
		collector:  {protocol.TierBackend, "not exposed through an entry"},
		cart:       {protocol.TierBackend, "a Service with nothing behind it"},
	} {
		c, ok := comps[id]
		if !ok {
			t.Errorf("no component %s", id)
			continue
		}
		if c.Tier != want.tier || !strings.HasPrefix(c.TierReason, want.reason) {
			t.Errorf("%s: tier %s (%q), want %s (%q)", id, c.Tier, c.TierReason, want.tier, want.reason)
		}
	}
}

func TestTierWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		want protocol.Tier
	}{
		{"flask-vue-frontend", protocol.TierFrontend}, {"flask-vue-backend", protocol.TierBackend},
		{"billing-api", protocol.TierBackend}, {"orders-db", protocol.TierDatabase}, {"session-cache", protocol.TierCache},
		{"cert-manager-webhook", ""}, {"whoami", ""},
	} {
		got := protocol.Tier("")
		for _, w := range strings.FieldsFunc(tc.name, func(r rune) bool { return r == '-' }) {
			if t, ok := tierOfWord(w); ok {
				got = t
				break
			}
		}
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLinksAndEvidence(t *testing.T) {
	m := Build(analyse(shop()))
	for _, tc := range []struct {
		from, to string
		missing  bool
		ev       protocol.MapEvidence
	}{
		{entry, orders, false, protocol.MapEvidence{Type: protocol.EvidenceRoute, Object: entry, Name: "shop.example.com/api", Port: "8080"}},
		{entry, storefront, false, protocol.MapEvidence{Type: protocol.EvidenceRoute, Object: entry, Name: "shop.example.com/", Port: "80"}},
		{legacy, ghost, true, protocol.MapEvidence{Type: protocol.EvidenceRoute, Object: legacy, Name: "old.example.com/old", Port: "80"}},
		{storefront, orders, false, protocol.MapEvidence{Type: protocol.EvidenceEnv, Object: storefront, Name: "API_URL", Port: "8080"}},
		{storefront, cart, true, protocol.MapEvidence{Type: protocol.EvidenceEnv, Object: storefront, Name: "CART_HOST"}},
		{orders, pooler, false, protocol.MapEvidence{Type: protocol.EvidenceConfigMap, Object: "ConfigMap/shop/orders-config", Name: "DB_HOST", Key: "db-host", Port: "5432"}},
		{orders, cache, false, protocol.MapEvidence{Type: protocol.EvidenceEnv, Object: orders, Name: "REDIS_HOST"}},
		{orders, collector, false, protocol.MapEvidence{Type: protocol.EvidenceEnv, Object: orders, Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Port: "4318"}},
		{mailer, orders, false, protocol.MapEvidence{Type: protocol.EvidenceFile, Object: "ConfigMap/shop/mailer-conf", Name: "app.yaml", Port: "8080"}},
		{pooler, db, false, protocol.MapEvidence{Type: protocol.EvidenceOperator, Object: pooler, Name: "shop-db"}},
	} {
		l, ok := linkOf(m, tc.from, tc.to)
		if !ok {
			t.Errorf("no link %s → %s", tc.from, tc.to)
			continue
		}
		if l.Missing != tc.missing {
			t.Errorf("%s → %s: missing = %v", tc.from, tc.to, l.Missing)
		}
		found := slices.ContainsFunc(l.Evidence, func(e protocol.MapEvidence) bool {
			return e.Type == tc.ev.Type && e.Object == tc.ev.Object && e.Name == tc.ev.Name && e.Key == tc.ev.Key && e.Port == tc.ev.Port && e.Detail != ""
		})
		if !found {
			t.Errorf("%s → %s: evidence %+v, want %+v", tc.from, tc.to, l.Evidence, tc.ev)
		}
	}
	if len(m.Links) != 10 {
		for _, l := range m.Links {
			t.Logf("link %s → %s", l.From, l.To)
		}
		t.Errorf("%d links, want 10", len(m.Links))
	}
	l, _ := linkOf(m, orders, pooler)
	if !slices.Equal(l.Via, []string{"Service/shop/shop-pooler"}) || l.FromApp != "shop/shop" || l.ToApp == "" || l.ToApp == l.FromApp {
		t.Errorf("orders → pooler: %+v", l)
	}
}

func TestHealthAndPods(t *testing.T) {
	m := Build(analyse(shop()))
	comps := index(m)
	for id, healthy := range map[string]bool{
		entry: false, legacy: false, storefront: true, orders: false, mailer: true,
		pooler: true, db: false, cache: true, cart: false, ghost: false,
	} {
		if c := comps[id]; c.Healthy != healthy {
			t.Errorf("%s: healthy = %v (%s)", id, c.Healthy, c.Status)
		}
	}
	o := comps[orders]
	if len(o.Pods) != 1 || o.Pods[0].Status != "CrashLoopBackOff" || o.Pods[0].Ready || o.Pods[0].Restarts != 5 || o.Pods[0].Node != "k8server" {
		t.Errorf("orders pods = %+v", o.Pods)
	}
	if !slices.Equal(o.Services, []string{"Service/shop/orders"}) {
		t.Errorf("orders services = %v", o.Services)
	}
	d := comps[db]
	if d.Engine != "postgres" || d.Version != "16.4" || len(d.Pods) != 2 || d.Pods[0].Primary || !d.Pods[1].Primary || d.Pods[0].Restarts != 3 {
		t.Errorf("db = %+v", d)
	}
	if p := comps[pooler]; len(p.Pods) != 1 || p.Pods[0].Name != "shop-pooler-1" {
		t.Errorf("pooler pods (through its Deployment) = %+v", p.Pods)
	}
	if g := comps[ghost]; !g.Missing || g.Status != "missing" {
		t.Errorf("ghost = %+v", g)
	}
	if c := comps[cart]; c.Missing || c.Status != "no pods match selector" {
		t.Errorf("cart = %+v", c)
	}
	// Shown though nothing links to it. Its name matches no app or
	// workload, so KM-49 adopts it nowhere and it is unassigned.
	if !slices.ContainsFunc(m.Unassigned, func(c protocol.MapComponent) bool {
		return c.ID == "Service/shop/mailer-metrics" && !c.Healthy && c.App == "" && c.Status == "no pods match selector"
	}) {
		t.Errorf("mailer-metrics not unassigned: %+v", m.Unassigned)
	}
	e := comps[entry]
	if len(e.Routes) != 2 || e.Routes[0].Service != "Service/shop/orders" || !e.Routes[0].TLS ||
		!slices.Contains(e.Routes[0].Problems, "no ready endpoints (requests get 503)") || e.Routes[1].Problems != nil || e.Routes[1].ReadyEndpoints != 1 {
		t.Errorf("entry routes = %+v", e.Routes)
	}
	if lg := comps[legacy]; len(lg.Routes) != 1 || !slices.Contains(lg.Routes[0].Problems, "service missing") {
		t.Errorf("legacy routes = %+v", lg.Routes)
	}
}

func TestAppsAndOrder(t *testing.T) {
	m := Build(analyse(shop()))
	var shopApp protocol.MapApp
	for _, a := range m.Apps {
		if a.ID == "shop/shop" {
			shopApp = a
		}
	}
	var ids []string
	for _, c := range shopApp.Components {
		ids = append(ids, c.ID)
	}
	// Tier order, then ID: the IngressRoute is an app member through the
	// Services it routes to; the broken Service mailer is adopted by name.
	want := []string{entry, storefront, mailer, orders, "Service/shop/mailer"}
	if !slices.Equal(ids, want) {
		t.Errorf("shop components = %v, want %v", ids, want)
	}
	if shopApp.Health != "Degraded" || shopApp.State != protocol.StateFail || len(shopApp.Problems) == 0 {
		t.Errorf("shop health = %s %s %v", shopApp.Health, shopApp.State, shopApp.Problems)
	}
	if !slices.Contains(shopApp.Problems, protocol.MapProblem{ID: "Pod/shop/orders-1", Status: "CrashLoopBackOff"}) {
		t.Errorf("shop problems = %+v", shopApp.Problems)
	}
	for _, a := range m.Apps {
		for _, c := range a.Components {
			if c.App != a.ID {
				t.Errorf("%s in app %s says app %q", c.ID, a.ID, c.App)
			}
		}
	}
	// Stable: the same input gives byte-equal JSON.
	a, _ := json.Marshal(m)
	b, _ := json.Marshal(Build(analyse(shop())))
	if string(a) != string(b) {
		t.Error("output is not stable")
	}
}

func TestNarrow(t *testing.T) {
	m := Build(analyse(shop()))
	if out, ok := Narrow(m, "", ""); !ok || out != m {
		t.Error("no filter must return the map itself")
	}
	n, ok := Narrow(m, "obs", "")
	if !ok || len(n.Apps) != 1 || n.Apps[0].ID != "obs/collector" {
		t.Fatalf("obs apps = %+v", n.Apps)
	}
	// The link from orders reaches into obs, so orders is a peer, without pods.
	if len(n.Links) != 1 || len(n.Peers) != 1 || n.Peers[0].ID != orders || n.Peers[0].Pods != nil {
		t.Errorf("obs links %+v peers %+v", n.Links, n.Peers)
	}
	n, _ = Narrow(m, ns, "")
	var unassigned []string
	for _, c := range n.Unassigned {
		unassigned = append(unassigned, c.ID)
	}
	if slices.Contains(unassigned, collector) || len(n.Peers) != 1 || n.Peers[0].ID != collector {
		t.Errorf("shop: unassigned %v peers %+v", unassigned, n.Peers)
	}
	n, ok = Narrow(m, "", "shop/shop")
	if !ok || len(n.Apps) != 1 || n.Unassigned != nil {
		t.Fatalf("app filter: %+v", n)
	}
	peers := map[string]bool{}
	for _, p := range n.Peers {
		peers[p.ID] = true
	}
	for _, id := range []string{pooler, cache, collector, cart} {
		if !peers[id] {
			t.Errorf("app filter: peer %s missing (%v)", id, peers)
		}
	}
	if _, ok := Narrow(m, "", "shop/nope"); ok {
		t.Error("unknown app must not be ok")
	}
}

func TestStateAndProblems(t *testing.T) {
	for health, want := range map[string]protocol.AppState{
		"Healthy": protocol.StateOK, "Healthy, Synced": protocol.StateOK, "Healthy, OutOfSync": protocol.StateWarn,
		"Missing": protocol.StateWarn, "Missing, OutOfSync": protocol.StateWarn, "Degraded": protocol.StateFail,
		"Degraded; Argo CD: Progressing, Synced": protocol.StateFail, "": protocol.StateWarn,
	} {
		if got := state(health); got != want {
			t.Errorf("state(%q) = %s, want %s", health, got, want)
		}
	}
	got := problems([]string{"Service/ns/x: no pods match selector", "odd"})
	want := []protocol.MapProblem{{ID: "Service/ns/x", Status: "no pods match selector"}, {Status: "odd"}}
	if !slices.Equal(got, want) {
		t.Errorf("problems = %+v", got)
	}
}

func TestEmpty(t *testing.T) {
	m := Build(Input{})
	raw, _ := json.Marshal(m)
	if string(raw) != `{"apps":[],"links":[]}` {
		t.Errorf("empty map = %s", raw)
	}
}
