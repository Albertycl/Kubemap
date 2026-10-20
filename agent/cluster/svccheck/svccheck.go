// Package svccheck finds Services that exist but cannot carry traffic
// (KM-77): a selector that matches no pods (usually a label typo), pods that
// match but are not ready, a named targetPort no pod declares, and a
// targetPort that is not among the pods' declared ports. Each finding comes
// with the evidence, the fix and the kubectl command that shows it. It feeds
// the "Service endpoints" step of guided debugging (KM-76).
package svccheck

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

// Source is the agent's cache (or a test fake).
type Source interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

var (
	servicesGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	podsGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	slicesGVR   = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
)

const (
	critical = "critical"
	warning  = "warning"
	info     = "info"
)

// Snapshot is what the checks read.
type Snapshot struct {
	Services []*corev1.Service
	Pods     []*corev1.Pod
	Slices   []*discoveryv1.EndpointSlice
	// SlicesKnown is false when EndpointSlices are not cached.
	SlicesKnown bool
}

// Load reads a snapshot. Services and pods are required.
func Load(src Source) (*Snapshot, error) {
	s := &Snapshot{}
	objs, err := src.List(servicesGVR, "", nil)
	if err != nil {
		return nil, fmt.Errorf("svccheck: services: %w", err)
	}
	for _, o := range objs {
		s.Services = append(s.Services, o.(*corev1.Service))
	}
	if objs, err = src.List(podsGVR, "", nil); err != nil {
		return nil, fmt.Errorf("svccheck: pods: %w", err)
	}
	for _, o := range objs {
		s.Pods = append(s.Pods, o.(*corev1.Pod))
	}
	if objs, err = src.List(slicesGVR, "", nil); err == nil {
		s.SlicesKnown = true
		for _, o := range objs {
			s.Slices = append(s.Slices, o.(*discoveryv1.EndpointSlice))
		}
	}
	return s, nil
}

// Check checks the Services of one namespace (all when empty), or one
// Service when name is set.
func (s *Snapshot) Check(namespace, name string) *protocol.ServiceCheckResult {
	res := &protocol.ServiceCheckResult{Services: []protocol.ServiceStatus{}}
	for _, svc := range s.Services {
		if (namespace != "" && svc.Namespace != namespace) || (name != "" && svc.Name != name) {
			continue
		}
		st := s.check(svc)
		if !st.Healthy {
			res.Problems++
		}
		res.Services = append(res.Services, st)
	}
	sort.SliceStable(res.Services, func(i, j int) bool {
		a, b := res.Services[i], res.Services[j]
		if ra, rb := worst(a.Findings), worst(b.Findings); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})
	return res
}

func worst(fs []protocol.ServiceFinding) int {
	r := 3
	for _, f := range fs {
		r = min(r, rank(f.Severity))
	}
	return r
}

func rank(sev string) int {
	switch sev {
	case critical:
		return 0
	case warning:
		return 1
	case info:
		return 2
	}
	return 3
}

func finished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.DeletionTimestamp != nil
}

func ready(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (s *Snapshot) check(svc *corev1.Service) protocol.ServiceStatus {
	st := protocol.ServiceStatus{ID: graph.ID("Service", svc.Namespace, svc.Name), Namespace: svc.Namespace, Name: svc.Name,
		Type: string(cmp.Or(svc.Spec.Type, corev1.ServiceTypeClusterIP)), Selector: svc.Spec.Selector, ReadyEndpoints: s.readyEndpoints(svc)}
	k := "kubectl -n " + svc.Namespace
	add := func(sev, problem, port, detail, fix, command string) {
		st.Findings = append(st.Findings, protocol.ServiceFinding{Severity: sev, Problem: problem, Port: port, Detail: detail, Fix: fix, Command: command})
	}
	slicesCmd := fmt.Sprintf("%s get endpointslices -l kubernetes.io/service-name=%s", k, svc.Name)

	switch {
	case svc.Spec.Type == corev1.ServiceTypeExternalName:
		st.Healthy = true
		return st
	case len(svc.Spec.Selector) == 0:
		// Endpoints are written by hand or by a controller.
		if st.ReadyEndpoints == 0 {
			add(critical, "no endpoints", "", "The Service has no selector and its EndpointSlices hold no ready address.",
				"A Service without a selector needs EndpointSlices written by hand or by a controller; add them, or give the Service a selector.", slicesCmd)
		}
		st.Healthy = worst(st.Findings) > 1
		return st
	}

	sel := labels.SelectorFromSet(svc.Spec.Selector)
	var matched []*corev1.Pod
	for _, p := range s.Pods {
		if p.Namespace == svc.Namespace && !finished(p) && sel.Matches(labels.Set(p.Labels)) {
			matched = append(matched, p)
		}
	}
	slices.SortFunc(matched, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	st.PodsMatched = len(matched)
	for _, p := range matched {
		if ready(p) {
			st.PodsReady++
		}
	}

	// With publishNotReadyAddresses (StatefulSet peer Services, such as
	// CNPG's or Redis's headless ones) unready pods still get endpoints on
	// purpose, so members find each other while starting: the Service works,
	// the pods are the problem, and it is not critical for the Service.
	notReadySev, notReadyNote := critical, " The Service has no endpoints, so requests fail."
	if svc.Spec.PublishNotReadyAddresses {
		notReadySev, notReadyNote = warning, " The Service publishes not-ready addresses (publishNotReadyAddresses), so it still has endpoints; the pods are the problem, not the Service."
	}
	switch {
	case len(matched) == 0:
		add(critical, "selector matches no pods", "", s.nearMiss(svc),
			"Fix the Service's selector, or the labels in the workload's pod template, so they agree.",
			fmt.Sprintf("%s get pods -l %s; %s get pods --show-labels", k, sel.String(), k))
	case st.PodsReady == 0:
		var why []string
		for _, p := range matched[:min(len(matched), 3)] {
			why = append(why, p.Name+": "+graph.PodStatus(p))
		}
		add(notReadySev, "no ready pods", "", fmt.Sprintf("%d pods match the selector but none is ready (%s).%s",
			len(matched), strings.Join(why, "; "), notReadyNote),
			"Fix why the pods are not ready: see the pod's diagnosis (crash, failing readiness probe, image problem).",
			fmt.Sprintf("%s get pods -l %s; %s describe pod %s", k, sel.String(), k, matched[0].Name))
	case st.PodsReady < len(matched):
		detail := fmt.Sprintf("%d of %d matching pods are ready; traffic goes to the ready ones only.", st.PodsReady, len(matched))
		if svc.Spec.PublishNotReadyAddresses {
			detail = fmt.Sprintf("%d of %d matching pods are ready.%s", st.PodsReady, len(matched), notReadyNote)
		}
		add(warning, "some pods not ready", "", detail,
			"Check the pods that are not ready.", fmt.Sprintf("%s get pods -l %s", k, sel.String()))
	}

	if len(matched) > 0 {
		for _, sp := range svc.Spec.Ports {
			s.checkPort(svc, sp, matched, add, k)
		}
	}
	if s.SlicesKnown && st.PodsReady > 0 && st.ReadyEndpoints == 0 {
		add(warning, "EndpointSlices show no ready endpoint", "", fmt.Sprintf("%d pods are ready, but the Service's EndpointSlices list no ready address. "+
			"The endpoint controller may be behind, or no pod has the Service's (named) target port.", st.PodsReady),
			"Check the targetPort names against the pods' container ports.", slicesCmd)
	}
	st.Healthy = worst(st.Findings) > 1
	return st
}

// checkPort checks one Service port against the matched pods' declared
// container ports. Declared ports are informational for numbers (a process
// may listen on an undeclared port), but a named targetPort must exist: a
// pod without it gets no endpoint for that port.
func (s *Snapshot) checkPort(svc *corev1.Service, sp corev1.ServicePort, pods []*corev1.Pod, add func(sev, problem, port, detail, fix, command string), k string) {
	proto := cmp.Or(sp.Protocol, corev1.ProtocolTCP)
	target := sp.TargetPort
	if target.Type == intstr.Int && target.IntVal == 0 {
		target = intstr.FromInt32(sp.Port) // targetPort defaults to port
	}
	portName := fmt.Sprintf("%d/%s", sp.Port, proto)
	if sp.Name != "" {
		portName = sp.Name + " " + portName
	}
	declared := map[string]bool{} // "8080/TCP", "http/TCP"
	var numbers []string
	without := []string{}
	for _, p := range pods {
		has := false
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				cproto := cmp.Or(cp.Protocol, corev1.ProtocolTCP)
				n := fmt.Sprintf("%d/%s", cp.ContainerPort, cproto)
				if !declared[n] {
					numbers = append(numbers, n)
				}
				declared[n] = true
				if cp.Name != "" {
					declared[cp.Name+"/"+string(cproto)] = true
				}
				if (target.Type == intstr.String && cp.Name == target.StrVal && cproto == proto) ||
					(target.Type == intstr.Int && cp.ContainerPort == target.IntVal && cproto == proto) {
					has = true
				}
			}
		}
		if !has {
			without = append(without, p.Name)
		}
	}
	slices.Sort(numbers)
	podsCmd := fmt.Sprintf("%s get pod %s -o jsonpath='{.spec.containers[*].ports}'; %s get service %s -o jsonpath='{.spec.ports}'", k, pods[0].Name, k, svc.Name)
	switch {
	case target.Type == intstr.String && len(without) == len(pods):
		add(critical, "named targetPort not declared", portName, fmt.Sprintf("targetPort %q is not a container port name on any matching pod (declared: %s), "+
			"so no pod becomes an endpoint for this port.", target.StrVal, list(numbers)),
			fmt.Sprintf("Name the container port %q, or set targetPort to the number the app listens on.", target.StrVal), podsCmd)
	case target.Type == intstr.String && len(without) > 0:
		add(warning, "named targetPort missing on some pods", portName, fmt.Sprintf("%d of %d pods do not declare port %q (%s); they get no endpoint for it.",
			len(without), len(pods), target.StrVal, strings.Join(without[:min(3, len(without))], ", ")),
			"Make the pod templates agree (an old ReplicaSet during a rollout?).", podsCmd)
	case target.Type == intstr.Int && len(numbers) > 0 && len(without) == len(pods):
		add(warning, "targetPort not declared", portName, fmt.Sprintf("targetPort %d/%s is not among the pods' declared container ports (%s). "+
			"Declared ports are informational, so this works only if the app really listens on %d.", target.IntVal, proto, list(numbers), target.IntVal),
			"Set targetPort to the port the app listens on (usually the declared one).", podsCmd)
	}
}

func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// nearMiss explains a selector that matches nothing: which labels the
// namespace's pods have instead, and whether pods in other namespaces match
// (a Service in the wrong namespace).
func (s *Snapshot) nearMiss(svc *corev1.Service) string {
	var b strings.Builder
	fmt.Fprintf(&b, "No running pod in %s has all of %s.", svc.Namespace, labels.SelectorFromSet(svc.Spec.Selector).String())
	keys := make([]string, 0, len(svc.Spec.Selector))
	for key := range svc.Spec.Selector {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		want := svc.Spec.Selector[key]
		seen := map[string]bool{}
		for _, p := range s.Pods {
			if p.Namespace == svc.Namespace && !finished(p) {
				if v, ok := p.Labels[key]; ok && v != want {
					seen[v] = true
				}
			}
		}
		if len(seen) > 0 {
			vals := make([]string, 0, len(seen))
			for v := range seen {
				vals = append(vals, v)
			}
			slices.Sort(vals)
			closest := vals[0]
			for _, v := range vals {
				if levenshtein(v, want) < levenshtein(closest, want) {
					closest = v
				}
			}
			fmt.Fprintf(&b, " Pods here have %s=%s, not %s", key, strings.Join(vals[:min(5, len(vals))], ","), want)
			if similar(closest, want) {
				fmt.Fprintf(&b, " (a typo? %q vs %q)", want, closest)
			}
			b.WriteString(".")
		}
	}
	var elsewhere []string
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	for _, p := range s.Pods {
		if p.Namespace != svc.Namespace && !finished(p) && sel.Matches(labels.Set(p.Labels)) {
			elsewhere = append(elsewhere, p.Namespace+"/"+p.Name)
		}
	}
	if len(elsewhere) > 0 {
		slices.Sort(elsewhere)
		fmt.Fprintf(&b, " Pods in other namespaces match (%s): a Service only selects pods in its own namespace, so it may be in the wrong one or left over.",
			strings.Join(elsewhere[:min(3, len(elsewhere))], ", "))
	}
	return b.String()
}

// similar is true when one value contains the other or they differ by at
// most two edits: "catalog" and "catalog-api", "web" and "webb".
func similar(a, b string) bool {
	if a == b {
		return false
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	return levenshtein(a, b) <= 2
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// readyEndpoints counts ready addresses in the Service's EndpointSlices, or
// -1 when slices are not cached.
func (s *Snapshot) readyEndpoints(svc *corev1.Service) int {
	if !s.SlicesKnown {
		return -1
	}
	n := 0
	for _, es := range s.Slices {
		if es.Namespace != svc.Namespace || es.Labels[discoveryv1.LabelServiceName] != svc.Name {
			continue
		}
		for _, ep := range es.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				n += len(ep.Addresses)
			}
		}
	}
	return n
}

// Similar reports whether two label values look like a typo of each other:
// one contains the other, or they differ by at most two edits.
func Similar(a, b string) bool { return similar(a, b) }

// Distance is the edit distance between two label values.
func Distance(a, b string) int { return levenshtein(a, b) }
