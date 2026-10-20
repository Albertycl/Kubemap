// Package summary builds the small cluster state the cluster agent pushes to
// the portal every few seconds (protocol.Summary): counts of nodes, pods and
// workloads, per-node Ready, and the worst current problems. Everything else
// stays in the cluster and is fetched on demand (KM-25).
package summary

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

var (
	gvr = func(g, v, r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
	}
	nodesGVR        = gvr("", "v1", "nodes")
	namespacesGVR   = gvr("", "v1", "namespaces")
	podsGVR         = gvr("", "v1", "pods")
	eventsGVR       = gvr("", "v1", "events")
	deploymentsGVR  = gvr("apps", "v1", "deployments")
	statefulSetsGVR = gvr("apps", "v1", "statefulsets")
	daemonSetsGVR   = gvr("apps", "v1", "daemonsets")
)

// EventWindow is how far back warning events count as current problems.
const EventWindow = 15 * time.Minute

// collapseAt folds one event reason that hits this many objects or more into
// a single problem ("NodeNotReady on 34 Pods"), so one cluster-wide incident
// does not fill the whole problem list.
const collapseAt = 5

// Problem severities, worst first. The order is what an SRE should look at
// first: a node down takes everything on it down, a crashing pod is an
// outage, a degraded workload may still serve, an event may be noise.
const (
	sevNode = iota
	sevPodFailing
	sevUnschedulable
	sevMissingRef
	sevWorkload
	sevService
	sevEvent
)

type problem struct {
	protocol.Problem
	severity int
}

// Build makes a summary of what src holds now. g is the relationship graph
// of the same data (for pod status words, broken references and Services
// that match no pods); k8sVersion comes from discovery.
func Build(src graph.Source, g *graph.Graph, k8sVersion string, now time.Time) *protocol.Summary {
	s := &protocol.Summary{Time: now.UTC(), KubernetesVersion: k8sVersion}
	var probs []problem

	if list, err := src.List(nodesGVR, "", nil); err == nil {
		for _, o := range list {
			n := o.(*corev1.Node)
			ready := nodeReady(n)
			s.Nodes.Total++
			if ready {
				s.Nodes.Ready++
			} else {
				probs = append(probs, problem{protocol.Problem{Kind: "Node", Name: n.Name, Reason: "NotReady",
					Message: nodeMessage(n), Since: readySince(n)}, sevNode})
			}
			if len(s.NodeList) < protocol.MaxSummaryNodes {
				s.NodeList = append(s.NodeList, protocol.NodeStatus{Name: n.Name, Ready: ready})
			}
		}
		sort.Slice(s.NodeList, func(i, j int) bool { return s.NodeList[i].Name < s.NodeList[j].Name })
	}
	if list, err := src.List(namespacesGVR, "", nil); err == nil {
		s.Namespaces = len(list)
	}
	if list, err := src.List(podsGVR, "", nil); err == nil {
		for _, o := range list {
			p := o.(*corev1.Pod)
			s.Pods.Total++
			switch p.Status.Phase {
			case corev1.PodRunning:
				s.Pods.Running++
			case corev1.PodPending:
				s.Pods.Pending++
			case corev1.PodSucceeded:
				s.Pods.Succeeded++
			case corev1.PodFailed:
				s.Pods.Failed++
			default:
				s.Pods.Unknown++
			}
			if pr, ok := podProblem(p, g); ok {
				probs = append(probs, pr)
			}
		}
	}
	probs = append(probs, workloads(src, s)...)
	if g != nil {
		for _, n := range g.Nodes {
			switch {
			case n.Missing:
				var refs []string
				for _, e := range g.In(n.ID) {
					refs = append(refs, e.From)
				}
				probs = append(probs, problem{protocol.Problem{Kind: n.Kind, Namespace: n.Namespace, Name: n.Name,
					Reason: "Missing", Message: "referenced by " + strings.Join(first(refs, 3), ", ")}, sevMissingRef})
			case n.Kind == "Service" && n.Status == "no pods match selector":
				probs = append(probs, problem{protocol.Problem{Kind: "Service", Namespace: n.Namespace, Name: n.Name,
					Reason: "NoEndpoints", Message: "selector matches no pods"}, sevService})
			}
		}
	}
	probs = append(probs, events(src, now)...)

	sort.SliceStable(probs, func(i, j int) bool {
		if probs[i].severity != probs[j].severity {
			return probs[i].severity < probs[j].severity
		}
		a, b := probs[i].Problem, probs[j].Problem
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Reason < b.Reason
	})
	for _, p := range probs {
		if len(s.Problems) == protocol.MaxProblems {
			break
		}
		// Events from other controllers may lack a reason or an object;
		// one such problem must not make the whole summary invalid.
		if p.Kind == "" || p.Name == "" {
			continue
		}
		if p.Reason == "" {
			p.Reason = "Unknown"
		}
		s.Problems = append(s.Problems, p.Problem)
	}
	return s
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func readySince(n *corev1.Node) time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// nodeMessage names the pressure conditions a NotReady node reports.
func nodeMessage(n *corev1.Node) string {
	var parts []string
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Message != "" {
			parts = append(parts, c.Message)
		}
		if c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue {
			parts = append(parts, string(c.Type))
		}
	}
	return clip(strings.Join(parts, "; "))
}

// podProblem reports a pod that is failing, using the graph's kubectl-style
// status word when there is one.
func podProblem(p *corev1.Pod, g *graph.Graph) (problem, bool) {
	status := ""
	if g != nil {
		if n, ok := g.Node(graph.ID("Pod", p.Namespace, p.Name)); ok {
			status = n.Status
		}
	}
	base := protocol.Problem{Kind: "Pod", Namespace: p.Namespace, Name: p.Name}
	switch {
	case strings.HasPrefix(status, "Pending (Unschedulable)"):
		base.Reason, base.Message = "Unschedulable", clip(conditionMessage(p, corev1.PodScheduled))
		return problem{base, sevUnschedulable}, true
	case status == "" || status == "Running" || status == "Succeeded" || status == "Pending" || status == "Terminating":
		return problem{}, false
	}
	base.Reason = strings.TrimSuffix(strings.Split(status, " (")[0], ",")
	base.Message, base.Since = containerDetail(p)
	if p.Status.Phase == corev1.PodFailed && p.Status.Reason != "" {
		// Evicted pods: the kubelet's reason and message say why.
		base.Reason, base.Message = p.Status.Reason, clip(p.Status.Message)
	}
	if strings.Contains(status, "OOMKilled") {
		base.Message = clip("last terminated: OOMKilled; " + base.Message)
	}
	return problem{base, sevPodFailing}, true
}

func conditionMessage(p *corev1.Pod, t corev1.PodConditionType) string {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return c.Message
		}
	}
	return ""
}

// containerDetail returns the message and time of the first container that
// is waiting or terminated with an error, with its restart count.
func containerDetail(p *corev1.Pod) (string, time.Time) {
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, c := range list {
			msg := ""
			switch {
			case c.State.Waiting != nil:
				msg = c.State.Waiting.Message
			case c.State.Terminated != nil && c.State.Terminated.Reason != "Completed":
				msg = c.State.Terminated.Message
			default:
				if c.Ready || c.State.Running != nil {
					continue
				}
			}
			since := time.Time{}
			if t := c.LastTerminationState.Terminated; t != nil {
				since = t.FinishedAt.Time
			}
			detail := fmt.Sprintf("container %s, %d restarts", c.Name, c.RestartCount)
			if msg != "" {
				detail += ": " + msg
			}
			return clip(detail), since
		}
	}
	return "", time.Time{}
}

// workloads counts Deployments, StatefulSets and DaemonSets and reports the
// ones with fewer ready replicas than wanted.
func workloads(src graph.Source, s *protocol.Summary) []problem {
	var out []problem
	degraded := func(kind, ns, name string, ready, want int32) {
		if ready < want {
			out = append(out, problem{protocol.Problem{Kind: kind, Namespace: ns, Name: name, Reason: "Degraded",
				Message: fmt.Sprintf("%d/%d ready", ready, want)}, sevWorkload})
		}
	}
	if list, err := src.List(deploymentsGVR, "", nil); err == nil {
		for _, o := range list {
			d := o.(*appsv1.Deployment)
			s.Workloads.Deployments++
			want := replicas(d.Spec.Replicas)
			if d.Status.ReadyReplicas < want {
				s.Workloads.DeploymentsDegraded++
			}
			degraded("Deployment", d.Namespace, d.Name, d.Status.ReadyReplicas, want)
		}
	}
	if list, err := src.List(statefulSetsGVR, "", nil); err == nil {
		for _, o := range list {
			st := o.(*appsv1.StatefulSet)
			s.Workloads.StatefulSets++
			degraded("StatefulSet", st.Namespace, st.Name, st.Status.ReadyReplicas, replicas(st.Spec.Replicas))
		}
	}
	if list, err := src.List(daemonSetsGVR, "", nil); err == nil {
		for _, o := range list {
			ds := o.(*appsv1.DaemonSet)
			s.Workloads.DaemonSets++
			degraded("DaemonSet", ds.Namespace, ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
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

// events turns recent warning events into problems: one per object and
// reason (the latest message wins, counts add up), and one per reason when
// it hits collapseAt objects or more.
func events(src graph.Source, now time.Time) []problem {
	list, err := src.List(eventsGVR, "", nil)
	if err != nil {
		return nil
	}
	type key struct{ kind, ns, name, reason string }
	type agg struct {
		last  time.Time
		msg   string
		count int32
	}
	byObject := map[key]*agg{}
	for _, o := range list {
		e := o.(*corev1.Event)
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		last := eventTime(e)
		if last.IsZero() || now.Sub(last) > EventWindow {
			continue
		}
		k := key{e.InvolvedObject.Kind, e.InvolvedObject.Namespace, e.InvolvedObject.Name, e.Reason}
		a := byObject[k]
		if a == nil {
			a = &agg{}
			byObject[k] = a
		}
		count := e.Count
		if e.Series != nil && e.Series.Count > count {
			count = e.Series.Count
		}
		if count < 1 {
			count = 1
		}
		a.count += count
		if last.After(a.last) {
			a.last, a.msg = last, e.Message
		}
	}
	// Group by (kind, reason) to find cluster-wide incidents.
	type group struct {
		keys []key
		last time.Time
	}
	groups := map[[2]string]*group{}
	for k, a := range byObject {
		gk := [2]string{k.kind, k.reason}
		g := groups[gk]
		if g == nil {
			g = &group{}
			groups[gk] = g
		}
		g.keys = append(g.keys, k)
		if a.last.After(g.last) {
			g.last = a.last
		}
	}
	var out []problem
	for gk, g := range groups {
		if len(g.keys) >= collapseAt {
			sort.Slice(g.keys, func(i, j int) bool { return byObject[g.keys[i]].last.After(byObject[g.keys[j]].last) })
			ex := g.keys[0]
			out = append(out, problem{protocol.Problem{
				Kind: gk[0], Name: fmt.Sprintf("%d %ss", len(g.keys), gk[0]), Reason: gk[1],
				Message: clip(fmt.Sprintf("on %d objects, latest %s/%s: %s", len(g.keys), ex.ns, ex.name, byObject[ex].msg)),
				Since:   g.last,
			}, sevEvent})
			continue
		}
		for _, k := range g.keys {
			a := byObject[k]
			out = append(out, problem{protocol.Problem{Kind: k.kind, Namespace: k.ns, Name: k.name, Reason: k.reason,
				Message: clip(fmt.Sprintf("%d× %s", a.count, a.msg)), Since: a.last}, sevEvent})
		}
	}
	return out
}

// eventTime is when an event was last seen, from whichever field is set.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

// clip keeps messages short: the summary is pushed every few seconds.
func clip(s string) string {
	const max = 240
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 rune
		cut--
	}
	return s[:cut] + "…"
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
	}
	return s
}

// Sender delivers a message to the portal; the tunnel (KM-21) implements it.
type Sender interface {
	Send(id string, p protocol.Payload) error
}

// Pusher sends a summary every protocol.SummaryInterval while the cache keeps
// changing, and at least every Refresh when it does not, so the portal always
// has a recent one without a stream of identical messages.
type Pusher struct {
	Src        graph.Source // the cache; its Generation says when it changed
	Graph      *graph.Builder
	Sender     Sender
	K8sVersion string
	Interval   time.Duration // default protocol.SummaryInterval
	Refresh    time.Duration // default 60 s
	Now        func() time.Time
	Log        *slog.Logger // default slog.Default()

	lastGen  uint64
	lastSent time.Time
	sentOnce bool
}

// Tick sends a summary if the cache changed or Refresh passed. It returns
// whether it sent one.
func (p *Pusher) Tick() (bool, error) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	refresh := p.Refresh
	if refresh == 0 {
		refresh = time.Minute
	}
	gen := p.Src.Generation()
	t := now()
	if p.sentOnce && gen == p.lastGen && t.Sub(p.lastSent) < refresh {
		return false, nil
	}
	var g *graph.Graph
	if p.Graph != nil {
		g = p.Graph.Graph()
	}
	sum := Build(p.Src, g, p.K8sVersion, t)
	// An invalid summary is a bug in Build, not a broken connection: skip
	// this tick and keep pushing, instead of stopping the pusher.
	if err := sum.Validate(); err != nil {
		log := p.Log
		if log == nil {
			log = slog.Default()
		}
		log.Error("summary: invalid, not sent", "error", err)
		return false, nil
	}
	if err := p.Sender.Send("", sum); err != nil {
		return false, err
	}
	p.lastGen, p.lastSent, p.sentOnce = gen, t, true
	return true, nil
}

// Run calls Tick every Interval until ctx ends. A send error is returned: the
// tunnel reconnects (KM-21) and starts a new Pusher.
func (p *Pusher) Run(ctx context.Context) error {
	interval := p.Interval
	if interval == 0 {
		interval = protocol.SummaryInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := p.Tick(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
