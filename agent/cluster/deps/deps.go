// Package deps finds runtime dependencies that no Kubernetes object states:
// a workload's env vars and ConfigMaps name the Services it talks to, as in
// url-shortener's DB_HOST=url-shortener-pooler and REDIS_HOST=redis-master.
//
// It reads pod templates (so a workload scaled to zero still shows its
// dependencies), resolves env values from literals, configMapKeyRef and
// envFrom ConfigMaps, and matches the host names in them against the cached
// Services. Secret values are never read: a dependency hidden in a Secret
// (for example a full DATABASE_URL) is not found, by design.
package deps

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/graph"
)

// EdgeDependsOn is the edge type of a found dependency: workload → Service.
const EdgeDependsOn graph.EdgeType = "depends-on"

// Dependency is one workload → Service link found in configuration.
type Dependency struct {
	From string `json:"from"` // workload node ID
	To   string `json:"to"`   // Service node ID
	// Via says where the name was found: "env DB_HOST (ConfigMap
	// url-shortener-db-config)" or "env OTEL_EXPORTER_OTLP_TRACES_ENDPOINT".
	Via  string `json:"via"`
	Port string `json:"port,omitempty"` // from the value or a sibling *_PORT var
	// Kind, Name, ConfigMap and Key say the same as Via, structured for
	// the app map's evidence: Kind is "env" (a literal value), "configmap"
	// (env or envFrom from a ConfigMap key) or "file" (a mounted ConfigMap
	// file); Name is the env var or file name.
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	ConfigMap string `json:"configMap,omitempty"`
	Key       string `json:"key,omitempty"`
	// Missing is true when the name is clearly a cluster Service but no
	// such Service exists: a typo or a deleted Service, a classic outage.
	Missing bool `json:"missing,omitempty"`
}

// AppLink is a dependency between two applications, for the app map.
type AppLink struct {
	From string   `json:"from"` // app ID
	To   string   `json:"to"`   // app ID, or the Service ID when no app owns it
	Via  []string `json:"via"`  // the dependencies behind the link, "workload → Service (via)"
}

var (
	gvr = func(g, v, r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
	}
	workloadResources = []schema.GroupVersionResource{
		gvr("apps", "v1", "deployments"), gvr("apps", "v1", "statefulsets"), gvr("apps", "v1", "daemonsets"),
		gvr("batch", "v1", "cronjobs"), gvr("batch", "v1", "jobs"), gvr("", "v1", "pods"),
	}
	servicesGVR   = gvr("", "v1", "services")
	configMapsGVR = gvr("", "v1", "configmaps")
)

// hostKey matches env var names that hold a host, address or URL. Bare words
// ("redis-master") count as a host only under such a name, so that
// OTEL_SERVICE_NAME=flask-vue-backend is not read as a dependency.
var hostKey = regexp.MustCompile(`(?i)(HOST|HOSTNAME|ADDR|ADDRESS|URL|URI|DSN|ENDPOINT|ENDPOINTS|SERVER|SERVERS|BROKER|BROKERS|UPSTREAM|_SVC|SERVICE_HOST)$`)

// dnsName is a lowercase DNS name, as Service names and namespaces are.
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

type template struct {
	id   string // workload node ID
	ns   string
	spec corev1.PodSpec
}

// Find returns every dependency found in the configuration of the cluster's
// workloads, sorted.
func Find(src graph.Source) []Dependency {
	services := map[string]bool{} // "ns/name"
	if list, err := src.List(servicesGVR, "", nil); err == nil {
		for _, o := range list {
			s := o.(*corev1.Service)
			services[s.Namespace+"/"+s.Name] = true
		}
	}
	configMaps := map[string]map[string]string{}
	if list, err := src.List(configMapsGVR, "", nil); err == nil {
		for _, o := range list {
			cm := o.(*corev1.ConfigMap)
			configMaps[cm.Namespace+"/"+cm.Name] = cm.Data
		}
	}
	seen := map[Dependency]bool{}
	var out []Dependency
	for _, t := range templates(src) {
		for _, d := range append(scan(t, services, configMaps), scanFiles(t, services, configMaps)...) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Via < b.Via
	})
	return out
}

// templates returns the pod spec of every top-level workload. Pods and Jobs
// that belong to a controller are skipped: their controller's template
// covers them.
func templates(src graph.Source) []template {
	var out []template
	for _, r := range workloadResources {
		list, err := src.List(r, "", nil)
		if err != nil {
			continue
		}
		for _, o := range list {
			switch x := o.(type) {
			case *appsv1.Deployment:
				out = append(out, template{graph.ID("Deployment", x.Namespace, x.Name), x.Namespace, x.Spec.Template.Spec})
			case *appsv1.StatefulSet:
				out = append(out, template{graph.ID("StatefulSet", x.Namespace, x.Name), x.Namespace, x.Spec.Template.Spec})
			case *appsv1.DaemonSet:
				out = append(out, template{graph.ID("DaemonSet", x.Namespace, x.Name), x.Namespace, x.Spec.Template.Spec})
			case *batchv1.CronJob:
				out = append(out, template{graph.ID("CronJob", x.Namespace, x.Name), x.Namespace, x.Spec.JobTemplate.Spec.Template.Spec})
			case *batchv1.Job:
				if len(x.OwnerReferences) == 0 {
					out = append(out, template{graph.ID("Job", x.Namespace, x.Name), x.Namespace, x.Spec.Template.Spec})
				}
			case *corev1.Pod:
				// Only pods without an owner; operator pods (CNPG) are
				// databases themselves, not clients of their config.
				if len(x.OwnerReferences) == 0 {
					out = append(out, template{graph.ID("Pod", x.Namespace, x.Name), x.Namespace, x.Spec})
				}
			}
		}
	}
	return out
}

type envVar struct {
	name, value, via string
	configMap, key   string // when the value comes from a ConfigMap
}

// scan resolves every env var of a pod template and returns the Services
// named in them.
func scan(t template, services map[string]bool, configMaps map[string]map[string]string) []Dependency {
	var out []Dependency
	for _, c := range append(append([]corev1.Container{}, t.spec.InitContainers...), t.spec.Containers...) {
		vars := resolve(c, t.ns, configMaps)
		values := map[string]string{}
		for _, v := range vars {
			values[v.name] = v.value
		}
		for _, v := range vars {
			for _, h := range hosts(v.value, hostKey.MatchString(v.name)) {
				svc, ok := serviceFor(h.host, t.ns, services)
				if !ok {
					continue
				}
				port := h.port
				if port == "" {
					port = siblingPort(v.name, values)
				}
				kind := "env"
				if v.configMap != "" {
					kind = "configmap"
				}
				out = append(out, Dependency{
					From: t.id, To: graph.ID("Service", svc.ns, svc.name), Via: v.via, Port: port,
					Missing: !services[svc.ns+"/"+svc.name],
					Kind:    kind, Name: v.name, ConfigMap: v.configMap, Key: v.key,
				})
			}
		}
	}
	return out
}

// scanFiles looks for Service names in ConfigMaps mounted as files (an nginx
// "proxy_pass http://backend:5000;", an app.yaml with "url: http://api"). File
// content is noisier than env vars, so only explicit hosts (with a scheme or
// port, or in cluster-DNS form) that match an existing Service count, and a
// name that matches nothing is never reported as missing.
func scanFiles(t template, services map[string]bool, configMaps map[string]map[string]string) []Dependency {
	var out []Dependency
	for _, v := range t.spec.Volumes {
		var names []string
		switch {
		case v.ConfigMap != nil:
			names = append(names, v.ConfigMap.Name)
		case v.Projected != nil:
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					names = append(names, src.ConfigMap.Name)
				}
			}
		}
		for _, name := range names {
			data := configMaps[t.ns+"/"+name]
			keys := make([]string, 0, len(data))
			for k := range data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				for _, h := range hosts(data[k], false) {
					svc, ok := serviceFor(h.host, t.ns, services)
					if ok && services[svc.ns+"/"+svc.name] {
						out = append(out, Dependency{From: t.id, To: graph.ID("Service", svc.ns, svc.name),
							Via: "file " + k + " (ConfigMap " + name + ")", Port: h.port,
							Kind: "file", Name: k, ConfigMap: name, Key: k})
					}
				}
			}
		}
	}
	return out
}

// resolve lists a container's env vars with their values: literals,
// configMapKeyRef and envFrom ConfigMaps. Secret-backed vars are skipped.
func resolve(c corev1.Container, ns string, configMaps map[string]map[string]string) []envVar {
	var out []envVar
	for _, from := range c.EnvFrom {
		if from.ConfigMapRef == nil {
			continue
		}
		data := configMaps[ns+"/"+from.ConfigMapRef.Name]
		for k, v := range data {
			out = append(out, envVar{from.Prefix + k, v, "envFrom " + from.Prefix + k + " (ConfigMap " + from.ConfigMapRef.Name + ")", from.ConfigMapRef.Name, k})
		}
	}
	for _, e := range c.Env {
		switch {
		case e.ValueFrom == nil:
			out = append(out, envVar{name: e.Name, value: e.Value, via: "env " + e.Name})
		case e.ValueFrom.ConfigMapKeyRef != nil:
			r := e.ValueFrom.ConfigMapKeyRef
			if v, ok := configMaps[ns+"/"+r.Name][r.Key]; ok {
				out = append(out, envVar{e.Name, v, "env " + e.Name + " (ConfigMap " + r.Name + ")", r.Name, r.Key})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// siblingPort finds DB_PORT for DB_HOST, REDIS_PORT for REDIS_HOST.
func siblingPort(name string, values map[string]string) string {
	for _, suffix := range []string{"_HOST", "_HOSTNAME", "_ADDR", "_ADDRESS", "_SERVER"} {
		if base, ok := strings.CutSuffix(strings.ToUpper(name), suffix); ok {
			if p := values[base+"_PORT"]; p != "" {
				return p
			}
		}
	}
	return ""
}

type hostRef struct {
	host, port string
	explicit   bool // came with a scheme or port, so a bare word is a host
}

// hosts extracts host names from a config value: URLs (also jdbc: and
// redis:// with credentials), host:port lists and bare words. Bare words are
// kept only when hostLike is true. Loopback and IP addresses are dropped.
func hosts(value string, hostLike bool) []hostRef {
	// A bare word is a host only if the whole value is a name or a list of
	// names; "run the nightly job" under SERVER_HOST is prose.
	bareOK := hostLike && !strings.ContainsAny(strings.TrimSpace(value), " \t\n")
	var out []hostRef
	for _, tok := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\''
	}) {
		h := parseHost(tok)
		if strings.HasSuffix(h.host, ".svc") || strings.Contains(h.host, ".svc.") {
			h.explicit = true // cluster DNS form: always a host
		}
		if h.host == "" || (!h.explicit && !bareOK) {
			continue
		}
		if net.ParseIP(h.host) != nil || h.host == "localhost" || !dnsName.MatchString(h.host) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func parseHost(tok string) hostRef {
	if i := strings.Index(tok, "://"); i >= 0 {
		// Keep only the last scheme ("jdbc:postgresql://" → "postgresql://").
		start := strings.LastIndex(tok[:i], ":") + 1
		u, err := url.Parse(tok[start:])
		if err != nil || u.Host == "" {
			return hostRef{}
		}
		return hostRef{host: strings.ToLower(u.Hostname()), port: u.Port(), explicit: true}
	}
	if at := strings.LastIndex(tok, "@"); at >= 0 {
		tok = tok[at+1:] // user@host
	}
	if slash := strings.Index(tok, "/"); slash >= 0 {
		tok = tok[:slash]
	}
	if host, port, err := net.SplitHostPort(tok); err == nil {
		if _, ok := parsePort(port); ok {
			return hostRef{host: strings.ToLower(host), port: port, explicit: true}
		}
		return hostRef{}
	}
	return hostRef{host: tok} // bare words keep their case; DNS names are lowercase
}

func parsePort(p string) (string, bool) {
	if p == "" || len(p) > 5 {
		return "", false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return p, true
}

type svcRef struct{ ns, name string }

// serviceFor maps a host name to the Service it names, using cluster DNS
// rules. ok is false for names that are not cluster Services (external
// hosts). A name in cluster-DNS form ("x.ns.svc...") is returned even when
// the Service does not exist, so the caller can report it as missing; a bare
// or two-part name is returned only when the Service exists, or when it is a
// bare name (then it can only mean a Service in the pod's own namespace).
func serviceFor(host, podNS string, services map[string]bool) (svcRef, bool) {
	parts := strings.Split(host, ".")
	// "svc.ns.svc", "svc.ns.svc.cluster.local", "pod.svc.ns.svc..."
	for i := 1; i+1 < len(parts); i++ {
		if parts[i+1] == "svc" {
			return svcRef{ns: parts[i], name: parts[i-1]}, true
		}
	}
	switch len(parts) {
	case 1:
		return svcRef{ns: podNS, name: host}, true
	case 2:
		if services[parts[1]+"/"+parts[0]] {
			return svcRef{ns: parts[1], name: parts[0]}, true
		}
	}
	return svcRef{}, false
}

// Edges turns dependencies into graph edges, for the app map.
func Edges(deps []Dependency) []graph.Edge {
	out := make([]graph.Edge, 0, len(deps))
	for _, d := range deps {
		detail := d.Via
		if d.Port != "" {
			detail += " port " + d.Port
		}
		out = append(out, graph.Edge{From: d.From, To: d.To, Type: EdgeDependsOn, Detail: detail})
	}
	return out
}

// Links rolls dependencies up to application level: url-shortener →
// url-shortener-db (through Service url-shortener-pooler) and → redis. A
// dependency inside one app is not a link. A Service no app owns is linked
// by its own ID, so external-to-app targets still show.
func Links(deps []Dependency, list []apps.App) []AppLink {
	appOf := map[string]string{}
	for _, a := range list {
		for _, m := range a.Members {
			if _, taken := appOf[m]; !taken {
				appOf[m] = a.ID
			}
		}
	}
	byPair := map[[2]string][]string{}
	for _, d := range deps {
		from, ok := appOf[d.From]
		if !ok {
			continue
		}
		to, ok := appOf[d.To]
		if !ok {
			to = d.To
		}
		if from == to {
			continue
		}
		key := [2]string{from, to}
		byPair[key] = append(byPair[key], d.From+" → "+d.To+" ("+d.Via+")")
	}
	out := make([]AppLink, 0, len(byPair))
	for k, via := range byPair {
		sort.Strings(via)
		out = append(out, AppLink{From: k[0], To: k[1], Via: via})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}
