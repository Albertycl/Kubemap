// Package datastores recognises the databases, caches and queues in a cluster
// and what uses them, so the app map can draw url-shortener → Postgres
// (through PgBouncer) → and → Redis, and an SRE can see at a glance that the
// database has 2 of 2 instances ready.
//
// Signals, strongest first:
//
//  1. Operator resources: a CloudNativePG Cluster is a Postgres database,
//     a CNPG Pooler is PgBouncer in front of one.
//  2. The container image: postgres, bitnami/postgresql, redis, valkey,
//     mysql, mariadb, mongo, rabbitmq, kafka ... (exporters and operators
//     are not databases).
//  3. A well-known container port (5432, 3306, 6379 ...), when the image
//     says nothing. This is marked with a lower confidence.
//
// Metrics and trace stores (Prometheus, Tempo, Loki) are observability, not
// application data stores, and are left out.
package datastores

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
)

// Category groups engines for the UI.
type Category string

const (
	CategoryDatabase Category = "database"
	CategoryCache    Category = "cache"
	CategoryQueue    Category = "queue"
	CategorySearch   Category = "search"
	CategoryPooler   Category = "pooler" // PgBouncer and friends: in front of a database
)

// Confidence says how sure the detection is.
type Confidence string

const (
	ConfidenceOperator Confidence = "operator" // an operator resource says so
	ConfidenceImage    Confidence = "image"    // the container image says so
	ConfidencePort     Confidence = "port"     // only a well-known port suggests it
)

// Datastore is one recognised data store.
type Datastore struct {
	ID         string     `json:"id"` // graph node ID of the workload or operator resource
	Engine     string     `json:"engine"`
	Category   Category   `json:"category"`
	Confidence Confidence `json:"confidence"`
	Version    string     `json:"version,omitempty"` // from the image tag, when it is a version
	Operator   string     `json:"operator,omitempty"`
	// Ready and Desired count instances (pods); Status is the operator's or
	// workload's own status text.
	Ready   int    `json:"ready"`
	Desired int    `json:"desired"`
	Status  string `json:"status,omitempty"`
	Primary string `json:"primary,omitempty"` // CNPG: the current primary pod
	// Target is what a pooler sits in front of (a Datastore ID).
	Target string `json:"target,omitempty"`
	// Services are the Services that reach this store; Clients the
	// workloads that name one of them in their configuration (KM-50).
	Services   []string `json:"services,omitempty"`
	Clients    []string `json:"clients,omitempty"`
	Persistent bool     `json:"persistent"` // has PersistentVolumeClaims
	Evidence   []string `json:"evidence"`
}

type engine struct {
	name     string
	category Category
	images   []string // image base names (last path element, no tag)
	ports    []int32
}

// engines are matched on the image base name: an exact name, or a name
// followed by "-" ("redis-cluster", "postgresql-repmgr"), but never an
// exporter or operator image.
var engines = []engine{
	{"postgres", CategoryDatabase, []string{"postgres", "postgresql", "postgis", "timescaledb", "pgvector"}, []int32{5432}},
	{"pgbouncer", CategoryPooler, []string{"pgbouncer"}, nil},
	{"mysql", CategoryDatabase, []string{"mysql", "mysql-server", "percona-server"}, []int32{3306}},
	{"mariadb", CategoryDatabase, []string{"mariadb"}, nil},
	{"mongodb", CategoryDatabase, []string{"mongo", "mongodb"}, []int32{27017}},
	{"cassandra", CategoryDatabase, []string{"cassandra"}, []int32{9042}},
	{"clickhouse", CategoryDatabase, []string{"clickhouse", "clickhouse-server"}, []int32{8123}},
	{"redis", CategoryCache, []string{"redis", "redis-stack", "valkey", "keydb", "dragonfly"}, []int32{6379}},
	{"memcached", CategoryCache, []string{"memcached"}, []int32{11211}},
	{"rabbitmq", CategoryQueue, []string{"rabbitmq"}, []int32{5672}},
	{"kafka", CategoryQueue, []string{"kafka", "cp-kafka", "redpanda"}, []int32{9092}},
	{"nats", CategoryQueue, []string{"nats"}, []int32{4222}},
	{"elasticsearch", CategorySearch, []string{"elasticsearch", "opensearch"}, []int32{9200}},
	{"etcd", CategoryDatabase, []string{"etcd"}, []int32{2379}},
}

var (
	gvr = func(g, v, r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
	}
	cnpgClusters = gvr("postgresql.cnpg.io", "v1", "clusters")
	cnpgPoolers  = gvr("postgresql.cnpg.io", "v1", "poolers")
	deployments  = gvr("apps", "v1", "deployments")
	statefulSets = gvr("apps", "v1", "statefulsets")
)

// Detect finds the data stores in the cluster. dependencies (from
// deps.Find) fill in each store's clients; nil is fine.
func Detect(src graph.Source, g *graph.Graph, dependencies []deps.Dependency) []Datastore {
	var out []Datastore
	out = append(out, cnpg(src)...)
	out = append(out, workloads(src, g)...)
	for i := range out {
		fill(g, &out[i], dependencies)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// cnpg reads CloudNativePG Clusters and Poolers.
func cnpg(src graph.Source) []Datastore {
	var out []Datastore
	if list, err := src.List(cnpgClusters, "", nil); err == nil {
		for _, o := range list {
			u := o.(*unstructured.Unstructured)
			d := Datastore{
				ID: graph.ID("Cluster.postgresql.cnpg.io", u.GetNamespace(), u.GetName()), Engine: "postgres",
				Category: CategoryDatabase, Confidence: ConfidenceOperator, Operator: "cloudnative-pg",
				Evidence: []string{"CloudNativePG Cluster"},
			}
			d.Desired = nestedInt(u, "spec", "instances")
			d.Ready = nestedInt(u, "status", "readyInstances")
			d.Status, _, _ = unstructured.NestedString(u.Object, "status", "phase")
			d.Primary, _, _ = unstructured.NestedString(u.Object, "status", "currentPrimary")
			image, _, _ := unstructured.NestedString(u.Object, "status", "image")
			if image == "" {
				image, _, _ = unstructured.NestedString(u.Object, "spec", "imageName")
			}
			d.Version = version(image)
			out = append(out, d)
		}
	}
	if list, err := src.List(cnpgPoolers, "", nil); err == nil {
		for _, o := range list {
			u := o.(*unstructured.Unstructured)
			d := Datastore{
				ID: graph.ID("Pooler.postgresql.cnpg.io", u.GetNamespace(), u.GetName()), Engine: "pgbouncer",
				Category: CategoryPooler, Confidence: ConfidenceOperator, Operator: "cloudnative-pg",
				Evidence: []string{"CloudNativePG Pooler"},
			}
			d.Desired = nestedInt(u, "spec", "instances")
			if cluster, _, _ := unstructured.NestedString(u.Object, "spec", "cluster", "name"); cluster != "" {
				d.Target = graph.ID("Cluster.postgresql.cnpg.io", u.GetNamespace(), cluster)
			}
			if t, _, _ := unstructured.NestedString(u.Object, "spec", "type"); t != "" {
				d.Evidence = append(d.Evidence, "type "+t)
			}
			out = append(out, d)
		}
	}
	return out
}

func nestedInt(u *unstructured.Unstructured, path ...string) int {
	v, found, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
	if !found {
		return 0
	}
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// workloads inspects Deployments and StatefulSets that are their own top
// owner. Workloads an operator created (the CNPG Pooler's Deployment) are
// already covered by the operator resource.
func workloads(src graph.Source, g *graph.Graph) []Datastore {
	var out []Datastore
	check := func(kind, ns, name string, spec corev1.PodSpec, ready, desired int32) {
		id := graph.ID(kind, ns, name)
		if g != nil && g.Owner(id) != id {
			return
		}
		e, conf, image, evidence := recognise(spec)
		if e == nil {
			return
		}
		out = append(out, Datastore{
			ID: id, Engine: e.name, Category: e.category, Confidence: conf,
			Version: version(image), Ready: int(ready), Desired: int(desired), Evidence: evidence,
		})
	}
	if list, err := src.List(statefulSets, "", nil); err == nil {
		for _, o := range list {
			s := o.(*appsv1.StatefulSet)
			check("StatefulSet", s.Namespace, s.Name, s.Spec.Template.Spec, s.Status.ReadyReplicas, replicas(s.Spec.Replicas))
		}
	}
	if list, err := src.List(deployments, "", nil); err == nil {
		for _, o := range list {
			d := o.(*appsv1.Deployment)
			check("Deployment", d.Namespace, d.Name, d.Spec.Template.Spec, d.Status.ReadyReplicas, replicas(d.Spec.Replicas))
		}
	}
	return out
}

func replicas(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// recognise looks at a pod template's containers: first their images, then
// their ports. It returns the engine, how it was found, the matching image
// and the evidence.
func recognise(spec corev1.PodSpec) (*engine, Confidence, string, []string) {
	for _, c := range spec.Containers {
		base := imageBase(c.Image)
		for i := range engines {
			e := &engines[i]
			for _, name := range e.images {
				if base == name || (strings.HasPrefix(base, name+"-") && !helperImage(base)) {
					return e, ConfidenceImage, c.Image, []string{"image " + c.Image}
				}
			}
		}
	}
	for _, c := range spec.Containers {
		if helperImage(imageBase(c.Image)) {
			continue
		}
		for _, p := range c.Ports {
			for i := range engines {
				e := &engines[i]
				for _, port := range e.ports {
					if p.ContainerPort == port {
						return e, ConfidencePort, c.Image, []string{fmt.Sprintf("container %s port %d", c.Name, port)}
					}
				}
			}
		}
	}
	return nil, "", "", nil
}

// helperImage marks images that sit next to a database but are not one.
func helperImage(base string) bool {
	for _, s := range []string{"exporter", "operator", "backup", "sentinel-exporter", "proxy", "ui", "admin", "cli"} {
		if strings.HasSuffix(base, "-"+s) || strings.Contains(base, "-"+s+"-") {
			return true
		}
	}
	return false
}

// imageBase returns the last path element of an image without tag or
// digest: "docker.io/bitnamilegacy/redis:latest" → "redis".
func imageBase(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	if i := strings.Index(image, ":"); i >= 0 {
		image = image[:i]
	}
	return strings.ToLower(image)
}

// version returns the image tag when it looks like a version ("16",
// "8.2.3-alpine" → "8.2.3"), not for "latest" or digests.
func version(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	tag := image[colon+1:]
	if tag == "" || tag[0] < '0' || tag[0] > '9' {
		return ""
	}
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// fill adds status, Services, PVCs and clients from the graph and the
// dependencies.
func fill(g *graph.Graph, d *Datastore, dependencies []deps.Dependency) {
	if g == nil {
		return
	}
	if n, ok := g.Node(d.ID); ok && d.Status == "" {
		d.Status = n.Status
	}
	// Everything the store owns (pods through ReplicaSets, Services, PVCs).
	owned := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		for _, e := range g.Out(id) {
			if e.Type == graph.EdgeOwns && !owned[e.To] {
				owned[e.To] = true
				walk(e.To)
			}
		}
	}
	walk(d.ID)
	services := map[string]bool{}
	readyPods := 0
	for id := range owned {
		n, _ := g.Node(id)
		switch n.Kind {
		case "Service":
			services[id] = true
		case "PersistentVolumeClaim":
			d.Persistent = true
		case "Pod":
			if n.Status == "Running" {
				readyPods++
			}
			for _, e := range g.In(id) {
				if e.Type == graph.EdgeSelects {
					services[e.From] = true
				}
			}
			for _, e := range g.Out(id) {
				if e.Type == graph.EdgeMounts {
					d.Persistent = true
				}
			}
		}
	}
	if d.Operator == "cloudnative-pg" && d.Category == CategoryPooler && d.Ready == 0 {
		d.Ready = readyPods
	}
	for s := range services {
		d.Services = append(d.Services, s)
	}
	sort.Strings(d.Services)
	clients := map[string]bool{}
	for _, dep := range dependencies {
		if services[dep.To] && !owned[dep.From] && dep.From != d.ID {
			clients[dep.From] = true
		}
	}
	for c := range clients {
		d.Clients = append(d.Clients, c)
	}
	sort.Strings(d.Clients)
}
