package query

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/appmap"
	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/datastores"
	"kubemap/agent/cluster/debug"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/diagnose"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/health"
	"kubemap/agent/cluster/netpol"
	"kubemap/agent/cluster/pressure"
	"kubemap/agent/cluster/rbac"
	"kubemap/agent/cluster/routes"
	"kubemap/agent/cluster/svccheck"
	"kubemap/agent/cluster/traces"
	"kubemap/agent/cluster/usage"
	"kubemap/agent/node/identity"
	"kubemap/pkg/protocol"
)

const (
	// DefaultTailLines is how many log lines a logs request without
	// tailLines or sinceSeconds returns; MaxTailLines caps what it may ask.
	DefaultTailLines = 500
	MaxTailLines     = 10000
	// maxLogRead stops reading a log stream after this many bytes, whatever
	// the options ask; only the newest MaxLogBytes are kept anyway.
	maxLogRead = 32 << 20
)

// logs reads a finished chunk of a pod's log (not follow; that is a stream,
// KM-26) and keeps the newest protocol.MaxLogBytes.
func (h *Handler) logs(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if h.Kube == nil {
		return nil, perr(protocol.CodeUnavailable, "logs are not available")
	}
	opts := &corev1.PodLogOptions{}
	tail := int64(DefaultTailLines)
	if l := req.Logs; l != nil {
		opts.Container, opts.Previous, opts.Timestamps = l.Container, l.Previous, l.Timestamps
		if l.SinceSeconds > 0 {
			opts.SinceSeconds = &l.SinceSeconds
			tail = 0
		}
		if l.TailLines > 0 {
			tail = min(l.TailLines, MaxTailLines)
		}
	}
	if tail > 0 {
		opts.TailLines = &tail
	}
	limit := int64(maxLogRead)
	opts.LimitBytes = &limit
	stream, err := h.Kube.CoreV1().Pods(req.Namespace).GetLogs(req.Name, opts).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, cut, err := readTail(stream, protocol.MaxLogBytes)
	if err != nil {
		return nil, err
	}
	return &protocol.Response{Logs: data, Truncated: cut}, nil
}

// readTail reads r to the end and returns its last max bytes, starting at a
// line boundary when older bytes were dropped. Memory stays under 2×max.
func readTail(r io.Reader, max int) ([]byte, bool, error) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	cut := false
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > 2*max {
			buf = append(buf[:0], buf[len(buf)-max:]...)
			cut = true
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	if len(buf) > max {
		buf, cut = buf[len(buf)-max:], true
	}
	if cut {
		if i := strings.IndexByte(string(buf), '\n'); i >= 0 && i+1 < len(buf) {
			buf = buf[i+1:] // drop the partial first line
		}
	}
	return buf, cut, nil
}

// Bound-object claims the kubelet puts into projected ServiceAccount tokens
// (Kubernetes 1.30+), as TokenReview returns them in status.user.extra.
const (
	extraPodName  = "authentication.kubernetes.io/pod-name"
	extraPodUID   = "authentication.kubernetes.io/pod-uid"
	extraNodeName = "authentication.kubernetes.io/node-name"
)

// tokenReview asks the API server whether a node agent's token is valid for
// the kubemap-portal audience (A8). The portal decides with
// TokenReviewResult.AcceptNode. The token is never logged or echoed.
func (h *Handler) tokenReview(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if h.Kube == nil {
		return nil, perr(protocol.CodeUnavailable, "token review is not available")
	}
	tr := &authv1.TokenReview{Spec: authv1.TokenReviewSpec{
		Token: req.TokenReview.Token, Audiences: []string{req.TokenReview.Audience},
	}}
	res, err := h.Kube.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		// The API error never contains the token, but keep the message short.
		return nil, perr(protocol.CodeUnavailable, "token review failed: %s", reasonOf(err))
	}
	st := res.Status
	out := &protocol.TokenReviewResult{
		Authenticated: st.Authenticated, Audiences: st.Audiences, Username: st.User.Username,
		PodName: first(st.User.Extra[extraPodName]), PodUID: first(st.User.Extra[extraPodUID]),
		NodeName: first(st.User.Extra[extraNodeName]),
	}
	// The API does not return the expiry; the token's own exp claim does.
	// It is read only after the API server authenticated the token, so the
	// claim is the signed one.
	if st.Authenticated {
		if c, err := identity.ParseClaims(req.TokenReview.Token); err == nil && !c.Expiry.IsZero() {
			exp := c.Expiry.UTC()
			out.ExpiresAt = &exp
		}
	}
	return &protocol.Response{TokenReview: out}, nil
}

func first(v authv1.ExtraValue) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func reasonOf(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// AppMap is the object of a graph response: everything the portal's app map
// (KM-53) draws, for the whole cluster, one namespace or around one object.
type AppMap struct {
	Apps          []apps.App             `json:"apps"`
	Links         []deps.AppLink         `json:"links"`
	Dependencies  []deps.Dependency      `json:"dependencies"`
	Routes        []routes.Route         `json:"routes"`
	RouteFindings []routes.Finding       `json:"routeFindings"`
	Datastores    []datastores.Datastore `json:"datastores"`
	Graph         *graph.Graph           `json:"graph"`
}

// analyse runs the application analysis (KM-32 to KM-52) over the cache.
func (h *Handler) analyse() appmap.Input {
	g := h.graphNow()
	dependencies := deps.Find(h.Cache)
	table := routes.Table(h.Cache)
	return appmap.Input{
		Graph: g, Apps: apps.Group(h.Cache, g), Dependencies: dependencies,
		Routes: table, RouteFindings: routes.Check(table),
		Datastores: datastores.Detect(h.Cache, g, dependencies),
		Pod:        h.cachedPod,
	}
}

// cachedPod returns a pod from the cache, or nil.
func (h *Handler) cachedPod(namespace, name string) *corev1.Pod {
	obj, err := h.Cache.Get(podsGVR, namespace, name)
	if err != nil {
		return nil
	}
	p, _ := obj.(*corev1.Pod)
	return p
}

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// traceTimeout bounds asking Tempo, so a slow Tempo delays the map only a
// little; the map is then shown without confirmations.
const traceTimeout = 6 * time.Second

// confirmTraces marks the links traces confirm (KM-54). Without a Tempo it
// does nothing; when Tempo or the cache fails, the summary says why.
func (h *Handler) confirmTraces(ctx context.Context, m *protocol.AppMap) {
	if h.Traces == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, traceTimeout)
	defer cancel()
	spans, url, window, err := h.Traces.Spans(ctx, h.Cache)
	if url == "" {
		return
	}
	source := "tempo " + url
	if err != nil {
		m.Traces = &protocol.MapTraceSummary{Source: source, Error: err.Error(), At: time.Now().UTC()}
		return
	}
	x, err := traces.NewIndex(h.Cache)
	if err != nil {
		m.Traces = &protocol.MapTraceSummary{Source: source, Error: "cannot read pods and Services: " + err.Error(), At: time.Now().UTC()}
		return
	}
	traces.Confirm(m, spans, x, source, window, time.Now().UTC())
}

// appMap builds the application map ready to draw (protocol.AppMap),
// narrowed to one namespace or one app.
func (h *Handler) appMap(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	full := appmap.Build(h.analyse())
	h.confirmTraces(ctx, full)
	app := ""
	if req.AppMap != nil {
		app = req.AppMap.App
	}
	m, ok := appmap.Narrow(full, req.Namespace, app)
	if !ok {
		return nil, perr(protocol.CodeNotFound, "no app %s", app)
	}
	if m != full {
		m.Traces = traces.Narrowed(full.Traces, m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the app map is %d bytes; ask for one namespace or one app", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// podDiagnose explains a crashing or stuck pod (KM-56). The pod comes from
// the cache; events, logs and usage are read live with list, get and
// pods/log.
func (h *Handler) podDiagnose(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	d, _, err := h.diagnose(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	return objectResponse(d, "the diagnosis")
}

// podDebug walks the SRE checklist for a pod (KM-76): the diagnosis, then
// the node, Service and NetworkPolicy steps from the cache.
func (h *Handler) podDebug(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	d, pod, err := h.diagnose(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	// The walk reports the steps; the full logs stay in pod.diagnose.
	return objectResponse(debug.Walk(debug.FromCache(h.Cache, h.graphNow(), pod, d)), "the checklist")
}

func objectResponse(v any, what string) (*protocol.Response, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "%s is %d bytes", what, len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// diagnose runs KM-56's collector and returns the pod it looked at.
func (h *Handler) diagnose(ctx context.Context, namespace, name string) (*protocol.PodDiagnosis, *corev1.Pod, error) {
	var pod *corev1.Pod
	c := &diagnose.Collector{
		Kube: h.Kube, Dynamic: h.Dynamic,
		Pod: func(ns, name string) (*corev1.Pod, error) {
			obj, err := h.Cache.Get(podsGVR, ns, name)
			if err == nil {
				if p, ok := obj.(*corev1.Pod); ok {
					pod = p
					return p, nil
				}
			}
			if h.Kube != nil && !apierrors.IsNotFound(err) {
				p, err := h.Kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
				pod = p
				return p, err
			}
			return nil, perr(protocol.CodeNotFound, "no pod %s/%s", ns, name)
		},
		Owner: func(p *corev1.Pod) string {
			g := h.graphNow()
			id := graph.ID("Pod", p.Namespace, p.Name)
			if o := g.Owner(id); o != id {
				return o
			}
			return ""
		},
	}
	d, err := c.Diagnose(ctx, namespace, name)
	return d, pod, err
}

// graphNow returns the current relationship graph.
func (h *Handler) graphNow() *graph.Graph {
	if h.Graph != nil {
		return h.Graph.Graph()
	}
	return graph.Build(h.Cache)
}

// serviceCheck checks Services from the cache (KM-77).
func (h *Handler) serviceCheck(req *protocol.Request) (*protocol.Response, error) {
	snap, err := svccheck.Load(h.Cache)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "%v", err)
	}
	res := snap.Check(req.Namespace, req.Name)
	if req.Name != "" && len(res.Services) == 0 {
		return nil, perr(protocol.CodeNotFound, "no Service %s/%s", req.Namespace, req.Name)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the check is %d bytes; ask for one namespace", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// netpolCheck evaluates the cluster's NetworkPolicies for one connection
// (KM-33), from the cache only.
func (h *Handler) netpolCheck(req *protocol.Request) (*protocol.Response, error) {
	snap, err := netpol.Load(h.Cache)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "%v", err)
	}
	res, err := snap.Check(req.NetpolCheck)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return &protocol.Response{Object: raw}, nil
}

// graph builds the application map, narrowed by namespace or around a node.
func (h *Handler) graph(req *protocol.Request) (*protocol.Response, error) {
	in := h.analyse()
	g := in.Graph
	m := &AppMap{
		Apps: in.Apps, Links: deps.Links(in.Dependencies, in.Apps), Dependencies: in.Dependencies,
		Routes: in.Routes, RouteFindings: in.RouteFindings, Datastores: in.Datastores, Graph: g,
	}
	switch {
	case req.Graph != nil && req.Graph.Around != "":
		if _, ok := g.Node(req.Graph.Around); !ok {
			return nil, perr(protocol.CodeNotFound, "no object %s in the graph", req.Graph.Around)
		}
		depth := req.Graph.Depth
		if depth == 0 {
			depth = 2
		}
		m.narrow(g.Around(req.Graph.Around, depth))
	case req.Namespace != "":
		m.narrow(g.Namespace(req.Namespace))
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the map is %d bytes; ask for one namespace or around one object", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// narrow keeps what touches the given subgraph.
func (m *AppMap) narrow(sub *graph.Graph) {
	in := func(id string) bool { _, ok := sub.Node(id); return ok }
	var keptApps []apps.App
	appIDs := map[string]bool{}
	for _, a := range m.Apps {
		for _, w := range a.Members {
			if in(w) {
				keptApps = append(keptApps, a)
				appIDs[a.ID] = true
				break
			}
		}
	}
	var links []deps.AppLink
	for _, l := range m.Links {
		if appIDs[l.From] || appIDs[l.To] {
			links = append(links, l)
		}
	}
	var dl []deps.Dependency
	for _, d := range m.Dependencies {
		if in(d.From) || in(d.To) {
			dl = append(dl, d)
		}
	}
	var rs []routes.Route
	for _, r := range m.Routes {
		if in(r.Source) || in(r.Service) {
			rs = append(rs, r)
		}
	}
	var fs []routes.Finding
	for _, f := range m.RouteFindings {
		if in(f.Route.Source) || in(f.Route.Service) {
			fs = append(fs, f)
		}
	}
	var ds []datastores.Datastore
	for _, d := range m.Datastores {
		if in(d.ID) {
			ds = append(ds, d)
		}
	}
	m.Apps, m.Links, m.Dependencies, m.Routes, m.RouteFindings, m.Datastores, m.Graph = keptApps, links, dl, rs, fs, ds, sub
}

var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// nodePressure combines node conditions from the cache with the PSI in the
// node agents' reports that the portal sends along (KM-60). It makes no
// Kubernetes or kubelet call.
func (h *Handler) nodePressure(req *protocol.Request) (*protocol.Response, error) {
	nodes, err := h.nodes(req.Name)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(pressure.Report(nodes, pressure.Reports(req.NodeMetrics), time.Now()))
	if err != nil {
		return nil, err
	}
	return &protocol.Response{Object: raw}, nil
}

// nodes returns all nodes from the cache, or the one named.
func (h *Handler) nodes(name string) ([]*corev1.Node, error) {
	objs, err := h.Cache.List(nodesGVR, "", nil)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "nodes: %v", err)
	}
	var out []*corev1.Node
	for _, o := range objs {
		if n, ok := o.(*corev1.Node); ok && (name == "" || n.Name == name) {
			out = append(out, n)
		}
	}
	if name != "" && len(out) == 0 {
		return nil, perr(protocol.CodeNotFound, "no node %s", name)
	}
	return out, nil
}

// healthCheck runs the health rules (KM-61, KM-62) on the cached nodes and
// pods and the node agents' reports that the portal sends along. It makes no
// Kubernetes or kubelet call.
func (h *Handler) healthCheck(req *protocol.Request) (*protocol.Response, error) {
	nodes, err := h.nodes("")
	if err != nil {
		return nil, err
	}
	objs, err := h.Cache.List(podsGVR, req.Namespace, nil)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "pods: %v", err)
	}
	pods := make([]*corev1.Pod, 0, len(objs))
	for _, o := range objs {
		if p, ok := o.(*corev1.Pod); ok {
			pods = append(pods, p)
		}
	}
	now := time.Now()
	all := pressure.Reports(req.NodeMetrics)
	reports := map[string]*protocol.Metrics{}
	var notes []string
	for _, n := range nodes {
		m, note := pressure.Fresh(all, n.Name, now)
		if note != "" {
			notes = append(notes, note+"; disk, memory and swap rules skipped for it")
			continue
		}
		reports[n.Name] = m
	}
	rep := health.Check(health.Inputs{Now: now, Nodes: nodes, Reports: reports, Pods: pods,
		Namespace: req.Namespace, Thresholds: h.Thresholds, Notes: notes})
	raw, err := json.Marshal(rep)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the health report is %d bytes; ask for one namespace", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// podUsage reports per-pod usage from the metrics API (KM-28), with the
// requests and limits from the cached pod specs. PodMetrics are read live
// with list (or get, for one pod) on pods.metrics.k8s.io; no kubelet call.
func (h *Handler) podUsage(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if h.Dynamic == nil {
		return nil, perr(protocol.CodeUnavailable, "the metrics API is not available in this agent")
	}
	objs, err := h.Cache.List(podsGVR, req.Namespace, nil)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "pods: %v", err)
	}
	specs := map[string]*corev1.Pod{}
	for _, o := range objs {
		if p, ok := o.(*corev1.Pod); ok && (req.Name == "" || p.Name == req.Name) {
			specs[p.Namespace+"/"+p.Name] = p
		}
	}
	res := h.Dynamic.Resource(usage.PodMetricsGVR).Namespace(req.Namespace)
	var items []unstructured.Unstructured
	if req.Name != "" {
		if _, ok := specs[req.Namespace+"/"+req.Name]; !ok {
			return nil, perr(protocol.CodeNotFound, "no pod %s/%s", req.Namespace, req.Name)
		}
		u, err := res.Get(ctx, req.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, perr(protocol.CodeNotFound, "no metrics for pod %s/%s yet (not running, or metrics-server has not scraped it)", req.Namespace, req.Name)
		}
		if err != nil {
			return nil, metricsErr(err)
		}
		items = []unstructured.Unstructured{*u}
	} else {
		list, err := res.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, metricsErr(err)
		}
		items = list.Items
	}
	pms, err := usage.Parse(items)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(usage.Report(pms, specs, time.Now(), nil))
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the usage report is %d bytes; ask for one namespace", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

// metricsErr explains the two usual failures of the metrics API.
func metricsErr(err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return perr(protocol.CodeUnavailable, "the metrics API (pods.metrics.k8s.io) is not served: is metrics-server installed?")
	case apierrors.IsForbidden(err):
		return perr(protocol.CodeForbidden, "the agent may not read pods.metrics.k8s.io (needs get and list): %s", reasonOf(err))
	}
	return err
}

// rbacWhoCan answers RBAC questions from the cached RBAC objects (KM-34):
// no SubjectAccessReview, so nothing is created and no permission is added.
func (h *Handler) rbacWhoCan(req *protocol.Request) (*protocol.Response, error) {
	snap, err := rbac.Load(h.Cache)
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, "%v", err)
	}
	res := snap.Query(req.RBAC)
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the answer is %d bytes; ask about one namespace or one subject", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}
