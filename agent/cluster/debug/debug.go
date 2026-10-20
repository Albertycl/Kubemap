// Package debug walks the SRE checklist for one pod (KM-76, guided
// debugging): status and reason → events → logs → exit code → requests and
// limits against usage → node → Service endpoints → NetworkPolicy. It reuses
// the crash helper (KM-56) for the pod's own facts, the Service checks
// (KM-77) and the NetworkPolicy engine (KM-33), and the dependencies found in
// configuration (KM-50) to know which traffic to test.
//
// Every step returns evidence, a verdict and the kubectl command a person
// would type, so the portal can show the method, and the cause line names
// the most specific problem: a blocked connection or a broken Service
// explains a symptom such as CrashLoopBackOff or error logs.
package debug

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"

	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/netpol"
	"kubemap/agent/cluster/svccheck"
	"kubemap/pkg/protocol"
)

// Inputs is everything the walk reads. Only Diagnosis and Pod are
// required; a missing part makes its step "unknown".
type Inputs struct {
	Diagnosis *protocol.PodDiagnosis // KM-56, with events, logs and usage
	Pod       *corev1.Pod
	Pods      []*corev1.Pod // all pods, for node capacity and callers
	Nodes     []*corev1.Node
	Graph     *graph.Graph
	Deps      []deps.Dependency // KM-50
	Services  *svccheck.Snapshot
	Netpol    *netpol.Snapshot
}

// causeOrder ranks the steps for the cause line: specific causes before
// symptoms.
var causeOrder = []string{protocol.StepNetpol, protocol.StepService, protocol.StepResources, protocol.StepNode,
	protocol.StepLogs, protocol.StepExitCode, protocol.StepEvents, protocol.StepStatus}

// Walk runs the checklist.
func Walk(in Inputs) *protocol.PodDebug {
	d, p := in.Diagnosis, in.Pod
	out := &protocol.PodDebug{Namespace: p.Namespace, Name: p.Name, Owner: d.Owner, Status: d.Status, Notes: d.Notes}
	w := &walker{in: in, k: "kubectl -n " + p.Namespace}
	out.Steps = []protocol.DebugStep{
		w.status(), w.events(), w.logs(), w.exitCode(), w.resources(), w.node(), w.services(), w.netpols(),
	}
	for _, key := range causeOrder {
		for _, s := range out.Steps {
			if s.Key == key && s.Verdict == protocol.VerdictProblem && out.Cause == "" {
				out.Cause, out.CauseStep = s.Finding, s.Key
			}
		}
	}
	return out
}

type walker struct {
	in Inputs
	k  string // "kubectl -n <namespace>"
}

func step(key, title string) protocol.DebugStep {
	return protocol.DebugStep{Key: key, Title: title, Verdict: protocol.VerdictOK}
}

func (w *walker) name() string { return w.in.Pod.Name }

// 1. Status and reason.
func (w *walker) status() protocol.DebugStep {
	d := w.in.Diagnosis
	s := step(protocol.StepStatus, "Status and reason")
	s.Commands = []string{fmt.Sprintf("%s get pod %s -o wide", w.k, w.name())}
	s.Evidence = append(s.Evidence, fmt.Sprintf("phase %s, status %s", d.Phase, d.Status))
	if d.Reason != "" || d.Message != "" {
		s.Evidence = append(s.Evidence, strings.TrimSpace("reason "+d.Reason+": "+d.Message))
	}
	notReady := 0
	for _, c := range d.Containers {
		if c.Init {
			continue
		}
		s.Evidence = append(s.Evidence, fmt.Sprintf("container %s: %s, ready %v, restarts %d", c.Name, c.State, c.Ready, c.Restarts))
		if !c.Ready {
			notReady++
		}
	}
	switch {
	case d.Phase == string(corev1.PodSucceeded) && d.Status != "Evicted":
		s.Finding = "The pod finished successfully."
	case d.Status == "Running" && notReady == 0:
		s.Finding = "Running, and every container is ready."
	case d.Status == "Running":
		s.Verdict = protocol.VerdictProblem
		s.Finding = fmt.Sprintf("Running, but %d container(s) are not ready: they get no traffic from Services.", notReady)
	default:
		s.Verdict = protocol.VerdictProblem
		s.Finding = "The pod is " + d.Status + "."
	}
	return s
}

// 2. Events.
func (w *walker) events() protocol.DebugStep {
	s := step(protocol.StepEvents, "Events")
	s.Commands = []string{
		fmt.Sprintf("%s describe pod %s", w.k, w.name()),
		fmt.Sprintf("%s get events --field-selector involvedObject.name=%s --sort-by=.lastTimestamp", w.k, w.name()),
	}
	var warn []protocol.DiagEvent
	for _, e := range w.in.Diagnosis.Events {
		if e.Type == string(corev1.EventTypeWarning) {
			warn = append(warn, e)
		}
	}
	if len(warn) == 0 {
		s.Finding = fmt.Sprintf("No warning events (%d normal).", len(w.in.Diagnosis.Events))
		return s
	}
	s.Verdict = protocol.VerdictProblem
	for _, e := range warn[:min(5, len(warn))] {
		s.Evidence = append(s.Evidence, fmt.Sprintf("%s x%d: %s", e.Reason, e.Count, clip(e.Message, 200)))
	}
	s.Finding = fmt.Sprintf("%d warning event(s); the newest: %s: %s", len(warn), warn[0].Reason, clip(warn[0].Message, 160))
	return s
}

var errorLine = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|traceback|failed|refused|timed out)\b`)

// 3. Logs: the previous run after a restart, else the current one.
func (w *walker) logs() protocol.DebugStep {
	s := step(protocol.StepLogs, "Logs")
	var errs, last []string
	read := 0
	for _, c := range w.in.Diagnosis.Containers {
		prev := ""
		if c.LogsFrom == "previous" {
			prev = " --previous"
		}
		s.Commands = append(s.Commands, fmt.Sprintf("%s logs %s -c %s%s --tail=100", w.k, w.name(), c.Name, prev))
		if c.LogsFrom == "" {
			continue
		}
		read++
		lines := strings.Split(strings.TrimRight(c.Logs, "\n"), "\n")
		for _, l := range lines {
			if errorLine.MatchString(l) {
				errs = append(errs, fmt.Sprintf("%s (%s run): %s", c.Name, c.LogsFrom, clip(l, 200)))
			}
		}
		if n := len(lines); n > 0 && lines[n-1] != "" {
			last = append(last, fmt.Sprintf("%s (%s run), last line: %s", c.Name, c.LogsFrom, clip(lines[n-1], 200)))
		}
	}
	switch {
	case len(errs) > 0:
		s.Verdict = protocol.VerdictProblem
		s.Evidence = errs[max(0, len(errs)-3):]
		s.Finding = "The logs show errors: " + strings.SplitN(errs[len(errs)-1], ": ", 2)[1]
	case read == 0:
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "No logs: the containers have not run, or the logs could not be read."
	default:
		s.Evidence = last
		s.Finding = "No error lines in the logs."
	}
	return s
}

// 4. Exit code.
func (w *walker) exitCode() protocol.DebugStep {
	s := step(protocol.StepExitCode, "Exit code")
	s.Commands = []string{fmt.Sprintf(`%s get pod %s -o jsonpath='{range .status.containerStatuses[*]}{.name} {.lastState.terminated.reason} {.lastState.terminated.exitCode}{"\n"}{end}'`, w.k, w.name())}
	var bad []string
	for _, c := range w.in.Diagnosis.Containers {
		if c.Last == nil {
			continue
		}
		e := fmt.Sprintf("%s: last run ended %s, exit %d: %s", c.Name, c.Last.Reason, c.Last.ExitCode, c.Last.Meaning)
		s.Evidence = append(s.Evidence, e)
		if c.Last.ExitCode != 0 && !c.Init {
			bad = append(bad, fmt.Sprintf("%s exited %d: %s", c.Name, c.Last.ExitCode, c.Last.Meaning))
		}
	}
	switch {
	case len(bad) > 0:
		s.Verdict = protocol.VerdictProblem
		s.Finding = bad[0]
	case len(s.Evidence) == 0:
		s.Finding = "No container has exited."
	default:
		s.Finding = "Containers that exited did so normally (exit 0)."
	}
	return s
}

// 5. Requests and limits against usage.
func (w *walker) resources() protocol.DebugStep {
	s := step(protocol.StepResources, "Requests and limits vs usage")
	s.Commands = []string{
		fmt.Sprintf("%s top pod %s --containers", w.k, w.name()),
		fmt.Sprintf(`%s get pod %s -o jsonpath='{range .spec.containers[*]}{.name} {.resources}{"\n"}{end}'`, w.k, w.name()),
	}
	d := w.in.Diagnosis
	haveUsage := false
	for _, c := range d.Containers {
		if c.Init {
			continue
		}
		e := fmt.Sprintf("%s: requests %s, limits %s", c.Name, quantities(c.Requests), quantities(c.Limits))
		if u := c.Usage; u != nil {
			haveUsage = true
			e += fmt.Sprintf(", usage %dm CPU", u.CPUMillis)
			if u.CPUOfLimit > 0 {
				e += fmt.Sprintf(" (%.0f%% of limit)", u.CPUOfLimit)
			}
			e += fmt.Sprintf(", %s memory", mib(u.MemoryBytes))
			if u.MemOfLimit > 0 {
				e += fmt.Sprintf(" (%.0f%% of limit)", u.MemOfLimit)
			}
		}
		s.Evidence = append(s.Evidence, e)
	}
	for _, f := range d.Findings {
		switch {
		case strings.HasPrefix(f.Title, "Killed for memory"), f.Title == "Near the memory limit", f.Title == "At the CPU limit":
			if s.Verdict != protocol.VerdictProblem {
				s.Verdict = protocol.VerdictProblem
				s.Finding = fmt.Sprintf("%s: %s", f.Title, firstSentence(f.Detail))
			}
		case f.Title == "No memory limit":
			s.Evidence = append(s.Evidence, f.Container+": no memory limit")
		}
	}
	if s.Verdict == protocol.VerdictProblem {
		return s
	}
	if !haveUsage {
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "Usage is not available (metrics API), and nothing was killed for memory."
		for _, n := range d.Notes {
			if strings.HasPrefix(n, "usage") {
				s.Evidence = append(s.Evidence, n)
			}
		}
		return s
	}
	s.Finding = "Usage is within the limits."
	return s
}

// 6. Node: capacity for a pod that is not scheduled, pressure otherwise.
func (w *walker) node() protocol.DebugStep {
	s := step(protocol.StepNode, "Node")
	p := w.in.Pod
	if p.Spec.NodeName == "" {
		return w.scheduling(s)
	}
	s.Commands = []string{
		fmt.Sprintf("kubectl describe node %s", p.Spec.NodeName),
		fmt.Sprintf(`kubectl get node %s -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}'`, p.Spec.NodeName),
	}
	var n *corev1.Node
	for _, x := range w.in.Nodes {
		if x.Name == p.Spec.NodeName {
			n = x
		}
	}
	if n == nil {
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "Node " + p.Spec.NodeName + " is not in the cache."
		return s
	}
	var bad []string
	for _, c := range n.Status.Conditions {
		switch {
		case c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue:
			bad = append(bad, "NotReady: "+c.Message)
		case c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue:
			bad = append(bad, string(c.Type)+": "+c.Message)
		}
	}
	s.Evidence = append(s.Evidence, fmt.Sprintf("%s allocatable: cpu %s, memory %s; requested by pods: %s",
		n.Name, qty(n.Status.Allocatable, corev1.ResourceCPU), qty(n.Status.Allocatable, corev1.ResourceMemory), w.requestedOn(n.Name)))
	for _, t := range n.Spec.Taints {
		s.Evidence = append(s.Evidence, fmt.Sprintf("taint %s=%s:%s", t.Key, t.Value, t.Effect))
	}
	if len(bad) > 0 {
		s.Verdict = protocol.VerdictProblem
		s.Evidence = append(s.Evidence, bad...)
		s.Finding = fmt.Sprintf("Node %s is under pressure: %s", n.Name, strings.SplitN(bad[0], ":", 2)[0])
		return s
	}
	s.Finding = fmt.Sprintf("Node %s is Ready with no memory, disk or PID pressure.", n.Name)
	return s
}

// scheduling explains a pod that no node has taken.
func (w *walker) scheduling(s protocol.DebugStep) protocol.DebugStep {
	p := w.in.Pod
	s.Commands = []string{
		"kubectl get nodes -o custom-columns=NAME:.metadata.name,CPU:.status.allocatable.cpu,MEMORY:.status.allocatable.memory,TAINTS:.spec.taints",
		"kubectl describe node | grep -A8 'Allocated resources'",
	}
	cpu, mem := podRequests(p)
	s.Evidence = append(s.Evidence, fmt.Sprintf("the pod requests cpu %s, memory %s", cpu.String(), mem.String()))
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			s.Evidence = append(s.Evidence, "scheduler: "+c.Message)
		}
	}
	fits := 0
	biggest, biggestName := resource.Quantity{}, ""
	for _, n := range w.in.Nodes {
		alloc := n.Status.Allocatable[corev1.ResourceCPU]
		free := alloc.DeepCopy()
		used := w.requestedCPU(n.Name)
		free.Sub(used)
		s.Evidence = append(s.Evidence, fmt.Sprintf("node %s: cpu allocatable %s, requested %s, free %s; memory allocatable %s",
			n.Name, alloc.String(), used.String(), free.String(), qty(n.Status.Allocatable, corev1.ResourceMemory)))
		if alloc.Cmp(biggest) > 0 {
			biggest, biggestName = alloc, n.Name
		}
		if free.Cmp(cpu) >= 0 {
			fits++
		}
	}
	switch {
	case len(w.in.Nodes) == 0:
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "The pod is not scheduled and no node is in the cache."
	case biggest.Cmp(cpu) < 0:
		s.Verdict = protocol.VerdictProblem
		s.Finding = fmt.Sprintf("No node can ever run it: it requests %s CPU, the largest node (%s) has %s allocatable.", cpu.String(), biggestName, biggest.String())
	case fits == 0:
		s.Verdict = protocol.VerdictProblem
		s.Finding = fmt.Sprintf("Every node is full: it requests %s CPU, and no node has that much unrequested.", cpu.String())
	default:
		s.Verdict = protocol.VerdictProblem
		s.Finding = "The pod is not scheduled although CPU would fit: check taints, affinity, memory and the scheduler's message."
	}
	return s
}

func podRequests(p *corev1.Pod) (cpu, mem resource.Quantity) {
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu.Add(q)
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem.Add(q)
		}
	}
	return cpu, mem
}

func (w *walker) requestedCPU(node string) resource.Quantity {
	var sum resource.Quantity
	for _, p := range w.in.Pods {
		if p.Spec.NodeName == node && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			c, _ := podRequests(p)
			sum.Add(c)
		}
	}
	return sum
}

func (w *walker) requestedOn(node string) string {
	var cpu, mem resource.Quantity
	for _, p := range w.in.Pods {
		if p.Spec.NodeName == node && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			c, m := podRequests(p)
			cpu.Add(c)
			mem.Add(m)
		}
	}
	return fmt.Sprintf("cpu %s, memory %s", cpu.String(), mem.String())
}

// target is a Service the pod is connected to, in one direction.
type target struct {
	svc  *corev1.Service
	why  string // "selects this pod", "web names it in env API_URL"
	port int32
	from *corev1.Pod // for callers: a pod of the calling workload
}

// selecting returns the Services in the pod's namespace whose selector
// matches it.
func (w *walker) selecting() []*corev1.Service {
	var out []*corev1.Service
	if w.in.Services == nil {
		return nil
	}
	for _, svc := range w.in.Services.Services {
		if svc.Namespace == w.in.Pod.Namespace && len(svc.Spec.Selector) > 0 &&
			labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(w.in.Pod.Labels)) {
			out = append(out, svc)
		}
	}
	return out
}

func (w *walker) service(id string) *corev1.Service {
	if w.in.Services == nil {
		return nil
	}
	for _, svc := range w.in.Services.Services {
		if graph.ID("Service", svc.Namespace, svc.Name) == id {
			return svc
		}
	}
	return nil
}

// dependencies are the Services the pod's workload names in its
// configuration (KM-50).
func (w *walker) dependencies() []target {
	owner := w.owner()
	var out []target
	for _, d := range w.in.Deps {
		if d.From != owner {
			continue
		}
		svc := w.service(d.To)
		if svc == nil {
			continue
		}
		t := target{svc: svc, why: fmt.Sprintf("named in %s", d.Via)}
		if n, err := strconv.Atoi(d.Port); err == nil {
			t.port = int32(n)
		} else if len(svc.Spec.Ports) > 0 {
			t.port = svc.Spec.Ports[0].Port
		}
		out = append(out, t)
	}
	return out
}

func (w *walker) owner() string {
	id := graph.ID("Pod", w.in.Pod.Namespace, w.in.Pod.Name)
	if w.in.Graph == nil {
		return id
	}
	return w.in.Graph.Owner(id)
}

// 7. Service endpoints (KM-77).
func (w *walker) services() protocol.DebugStep {
	s := step(protocol.StepService, "Service endpoints")
	if w.in.Services == nil {
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "Services are not available in the cache."
		return s
	}
	var problems []string
	check := func(svc *corev1.Service, why string) {
		st := w.in.Services.Check(svc.Namespace, svc.Name).Services[0]
		s.Commands = append(s.Commands, fmt.Sprintf("kubectl -n %s get endpointslices -l kubernetes.io/service-name=%s", svc.Namespace, svc.Name))
		e := fmt.Sprintf("Service %s/%s (%s): %d pods, %d ready, %d ready endpoints", svc.Namespace, svc.Name, why, st.PodsMatched, st.PodsReady, st.ReadyEndpoints)
		for _, f := range st.Findings {
			if f.Severity == "critical" || f.Severity == "warning" {
				e += "; " + f.Problem
				problems = append(problems, fmt.Sprintf("Service %s/%s: %s. %s", svc.Namespace, svc.Name, f.Problem, firstSentence(f.Detail)))
				s.Commands = append(s.Commands, f.Command)
			}
		}
		s.Evidence = append(s.Evidence, e)
	}
	for _, svc := range w.selecting() {
		check(svc, "selects this pod")
	}
	// A Service meant for this pod whose selector just misses it (a typo).
	for _, svc := range w.nearMisses() {
		sel := labels.SelectorFromSet(svc.Spec.Selector).String()
		problems = append([]string{fmt.Sprintf("Service %s/%s selects %s, which matches no pod; this pod's labels are almost that (%s).",
			svc.Namespace, svc.Name, sel, w.labelsFor(svc))}, problems...)
		s.Evidence = append(s.Evidence, fmt.Sprintf("Service %s/%s: selector %s matches no pod", svc.Namespace, svc.Name, sel))
		s.Commands = append(s.Commands, fmt.Sprintf("kubectl -n %s get service %s -o jsonpath='{.spec.selector}'; kubectl -n %s get pod %s --show-labels",
			svc.Namespace, svc.Name, svc.Namespace, w.name()))
	}
	for _, t := range w.dependencies() {
		check(t.svc, "this pod's dependency, "+t.why)
	}
	switch {
	case len(problems) > 0:
		s.Verdict = protocol.VerdictProblem
		s.Finding = problems[0]
	case len(s.Evidence) == 0:
		s.Commands = append(s.Commands, fmt.Sprintf("%s get services -o wide", w.k))
		s.Finding = "No Service selects this pod, and its configuration names none."
	default:
		s.Finding = "The Services this pod serves or uses have ready endpoints."
	}
	return s
}

// nearMisses are Services in the pod's namespace that match no pod, whose
// selector keys the pod has with equal or similar values, and for which
// this pod is (one of) the closest pods: "catalog-api" is meant for the
// "catalog" pod (4 edits), not for "api" (8), though both are contained.
func (w *walker) nearMisses() []*corev1.Service {
	var out []*corev1.Service
	p := w.in.Pod
	for _, svc := range w.in.Services.Services {
		if svc.Namespace != p.Namespace || len(svc.Spec.Selector) == 0 {
			continue
		}
		if st := w.in.Services.Check(svc.Namespace, svc.Name).Services[0]; st.PodsMatched > 0 {
			continue
		}
		mine, ok := distance(svc.Spec.Selector, p.Labels)
		if !ok {
			continue
		}
		closest := true
		for _, o := range w.in.Pods {
			if o.Namespace != p.Namespace || o.Name == p.Name || o.Status.Phase == corev1.PodSucceeded || o.Status.Phase == corev1.PodFailed {
				continue
			}
			if d, ok := distance(svc.Spec.Selector, o.Labels); ok && d < mine {
				closest = false
			}
		}
		if closest {
			out = append(out, svc)
		}
	}
	return out
}

// distance is the total edit distance from a selector to a pod's labels;
// ok is false when the pod lacks a key, a value is not similar, or it
// matches exactly (then it is not a near miss).
func distance(sel, lbl map[string]string) (int, bool) {
	total := 0
	for k, v := range sel {
		pv, ok := lbl[k]
		if !ok || (pv != v && !similar(pv, v)) {
			return 0, false
		}
		total += svccheck.Distance(pv, v)
	}
	return total, total > 0
}

func (w *walker) labelsFor(svc *corev1.Service) string {
	var parts []string
	for k := range svc.Spec.Selector {
		parts = append(parts, k+"="+w.in.Pod.Labels[k])
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// 8. NetworkPolicy (KM-33): the pod's own dependencies, and the known
// callers of the Services that select it.
func (w *walker) netpols() protocol.DebugStep {
	s := step(protocol.StepNetpol, "NetworkPolicy")
	p := w.in.Pod
	s.Commands = []string{fmt.Sprintf("%s get networkpolicy", w.k)}
	if w.in.Netpol == nil {
		s.Verdict = protocol.VerdictUnknown
		s.Finding = "NetworkPolicies are not available in the cache."
		return s
	}
	var blocked []string
	deciding := map[string]bool{}
	run := func(from protocol.NetpolEndpoint, to protocol.NetpolEndpoint, port int32, label string) {
		r, err := w.in.Netpol.Check(&protocol.NetpolCheckRequest{From: from, To: to, Port: port})
		if err != nil {
			s.Evidence = append(s.Evidence, label+": not checked: "+err.Error())
			return
		}
		verdict := "allowed"
		if !r.Allowed {
			verdict = "BLOCKED"
		}
		e := fmt.Sprintf("%s: %s", label, verdict)
		for _, path := range r.Paths {
			for _, side := range []protocol.NetpolSide{path.Egress, path.Ingress} {
				if !side.Allowed {
					e += " — " + side.Direction + " " + side.Decision
					for _, pol := range side.Selecting {
						deciding[pol] = true
					}
				}
			}
		}
		s.Evidence = append(s.Evidence, e)
		if !r.Allowed {
			blocked = append(blocked, label)
		}
	}
	for _, t := range w.dependencies() {
		run(protocol.NetpolEndpoint{Namespace: p.Namespace, Pod: p.Name}, protocol.NetpolEndpoint{Namespace: t.svc.Namespace, Service: t.svc.Name}, t.port,
			fmt.Sprintf("this pod → Service %s/%s:%d (%s)", t.svc.Namespace, t.svc.Name, t.port, t.why))
	}
	for _, c := range w.callers() {
		run(protocol.NetpolEndpoint{Namespace: c.from.Namespace, Pod: c.from.Name}, protocol.NetpolEndpoint{Namespace: c.svc.Namespace, Service: c.svc.Name}, c.port,
			fmt.Sprintf("caller %s/%s → Service %s/%s:%d (%s)", c.from.Namespace, c.from.Name, c.svc.Namespace, c.svc.Name, c.port, c.why))
	}
	in, eg := w.in.Netpol.Isolating(p, networkingv1.PolicyTypeIngress), w.in.Netpol.Isolating(p, networkingv1.PolicyTypeEgress)
	s.Evidence = append(s.Evidence, fmt.Sprintf("ingress isolated by: %s; egress isolated by: %s", none(in), none(eg)))
	pols := make([]string, 0, len(deciding))
	for pol := range deciding {
		pols = append(pols, pol)
	}
	slices.Sort(pols)
	for _, pol := range pols {
		ns, name, _ := strings.Cut(pol, "/")
		s.Commands = append(s.Commands, fmt.Sprintf("kubectl -n %s describe networkpolicy %s", ns, name))
	}
	switch {
	case len(blocked) > 0:
		s.Verdict = protocol.VerdictProblem
		s.Finding = fmt.Sprintf("NetworkPolicy %s blocks %s.", strings.Join(pols, ", "), blocked[0])
	case len(s.Evidence) == 1 && (len(in) > 0 || len(eg) > 0):
		s.Finding = "No known caller or dependency to test; the pod is isolated by the policies listed, so only what they allow reaches it."
	case len(s.Evidence) == 1:
		s.Finding = "No policy selects this pod: all traffic to and from it is allowed."
	default:
		s.Finding = "Every known connection of this pod is allowed."
	}
	return s
}

// callers are the workloads whose configuration names a Service that
// selects this pod, each with one of its pods to test from.
func (w *walker) callers() []target {
	var out []target
	for _, svc := range w.selecting() {
		id := graph.ID("Service", svc.Namespace, svc.Name)
		for _, d := range w.in.Deps {
			if d.To != id {
				continue
			}
			from := w.podOf(d.From)
			if from == nil || from.Name == w.in.Pod.Name {
				continue
			}
			t := target{svc: svc, from: from, why: "named in " + d.Via}
			if n, err := strconv.Atoi(d.Port); err == nil {
				t.port = int32(n)
			} else if len(svc.Spec.Ports) > 0 {
				t.port = svc.Spec.Ports[0].Port
			}
			out = append(out, t)
		}
	}
	return out
}

// podOf returns a running pod owned by a workload.
func (w *walker) podOf(workload string) *corev1.Pod {
	if w.in.Graph == nil {
		return nil
	}
	for _, p := range w.in.Pods {
		if p.Status.Phase == corev1.PodRunning && w.in.Graph.Owner(graph.ID("Pod", p.Namespace, p.Name)) == workload {
			return p
		}
	}
	return nil
}

func none(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

func quantities(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := m[k]; ok {
			parts = append(parts, k+" "+v)
		}
	}
	return strings.Join(parts, ", ")
}

func qty(rl corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := rl[name]
	if !ok {
		return "?"
	}
	return q.String()
}

func mib(b int64) string { return fmt.Sprintf("%dMi", (b+(1<<19))>>20) }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// similar matches svccheck's typo rule: one value contains the other, or
// they differ by at most two edits.
func similar(a, b string) bool { return svccheck.Similar(a, b) }
