package summary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

var now = time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC)

type fakeSource struct {
	objs map[schema.GroupVersionResource][]runtime.Object
	gen  uint64
}

func (f *fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	return f.objs[g], nil
}
func (f *fakeSource) Generation() uint64 { return f.gen }

func i32(v int32) *int32 { return &v }

func node(name string, ready corev1.ConditionStatus, pressure ...corev1.NodeConditionType) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeReady, Status: ready,
		Message: "kubelet stopped posting node status", LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Minute))})
	for _, p := range pressure {
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: p, Status: corev1.ConditionTrue})
	}
	return n
}

func pod(ns, name string, phase corev1.PodPhase, cs ...corev1.ContainerStatus) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Status: corev1.PodStatus{Phase: phase, ContainerStatuses: cs}}
	if phase == corev1.PodRunning {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func warning(kind, ns, name, reason, msg string, ago time.Duration, count int32) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "." + reason},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: ns, Name: name},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: msg, Count: count,
		LastTimestamp: metav1.NewTime(now.Add(-ago))}
}

// demo mirrors the kubemap-demo namespace (A9) on a node under pressure.
func demo() *fakeSource {
	f := &fakeSource{objs: map[schema.GroupVersionResource][]runtime.Object{}, gen: 1}
	add := func(g schema.GroupVersionResource, o ...runtime.Object) { f.objs[g] = append(f.objs[g], o...) }
	add(nodesGVR, node("k8server", corev1.ConditionTrue), node("node-2", corev1.ConditionUnknown, corev1.NodeMemoryPressure))
	add(namespacesGVR, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubemap-demo"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	crash := corev1.ContainerStatus{Name: "app", RestartCount: 7,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-time.Minute))}}}
	oom := corev1.ContainerStatus{Name: "hog", RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-2 * time.Minute))}}}
	pending := pod("kubemap-demo", "pending-0", corev1.PodPending)
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/1 nodes are available: 1 Insufficient memory."}}
	evicted := pod("kubemap-demo", "evicted-1", corev1.PodFailed)
	evicted.Status.Reason, evicted.Status.Message = "Evicted", "The node was low on resource: memory."
	add(podsGVR,
		pod("kubemap-demo", "web-1", corev1.PodRunning),
		pod("kubemap-demo", "crashloop-1", corev1.PodRunning, crash),
		pod("kubemap-demo", "oom-1", corev1.PodRunning, oom),
		pending,
		pod("kubemap-demo", "job-done", corev1.PodSucceeded),
		evicted,
	)
	add(gvr("", "v1", "serviceaccounts"), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "kubemap-demo", Name: "default"}})
	add(deploymentsGVR,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "kubemap-demo", Name: "web"}, Spec: appsv1.DeploymentSpec{Replicas: i32(1)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "kubemap-demo", Name: "crashloop"}, Spec: appsv1.DeploymentSpec{Replicas: i32(1)}})
	add(statefulSetsGVR, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kubemap-demo", Name: "pending"}, Spec: appsv1.StatefulSetSpec{Replicas: i32(1)}})
	add(daemonSetsGVR, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "agent"}, Status: appsv1.DaemonSetStatus{NumberReady: 1, DesiredNumberScheduled: 2}})
	add(gvr("", "v1", "services"), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kubemap-demo", Name: "no-endpoints"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "nothing"}}})
	var evs []runtime.Object
	// One incident on many pods collapses to one problem.
	for i := range 6 {
		evs = append(evs, warning("Pod", "kubemap-demo", fmt.Sprintf("p%d", i), "NodeNotReady", "Node is not ready", time.Duration(i)*time.Minute, 1))
	}
	evs = append(evs,
		warning("Pod", "kubemap-demo", "web-1", "Unhealthy", "Readiness probe failed: timeout", 30*time.Second, 5),
		warning("Pod", "kubemap-demo", "web-1", "Unhealthy", "Readiness probe failed: connection refused", 10*time.Second, 2),
		warning("Pod", "kubemap-demo", "old", "BackOff", "too old", time.Hour, 1), // outside the window
		&corev1.Event{InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "x"}, Type: corev1.EventTypeNormal, Reason: "Pulled", LastTimestamp: metav1.NewTime(now)},
	)
	add(eventsGVR, evs...)
	return f
}

func build(f *fakeSource) *protocol.Summary {
	return Build(f, graph.Build(f), "v1.34.8+k3s1", now)
}

func TestCounts(t *testing.T) {
	s := build(demo())
	if s.KubernetesVersion != "v1.34.8+k3s1" || !s.Time.Equal(now) {
		t.Errorf("header = %s %v", s.KubernetesVersion, s.Time)
	}
	if s.Nodes != (protocol.NodeCounts{Total: 2, Ready: 1}) || s.Namespaces != 2 {
		t.Errorf("nodes %+v namespaces %d", s.Nodes, s.Namespaces)
	}
	if len(s.NodeList) != 2 || s.NodeList[0] != (protocol.NodeStatus{Name: "k8server", Ready: true}) || s.NodeList[1].Ready {
		t.Errorf("node list = %+v", s.NodeList)
	}
	want := protocol.PodCounts{Total: 6, Running: 3, Pending: 1, Succeeded: 1, Failed: 1}
	if s.Pods != want {
		t.Errorf("pods = %+v, want %+v", s.Pods, want)
	}
	if s.Workloads != (protocol.WorkloadCounts{Deployments: 2, DeploymentsDegraded: 1, StatefulSets: 1, DaemonSets: 1}) {
		t.Errorf("workloads = %+v", s.Workloads)
	}
	// The summary must pass the protocol's own checks and encode.
	if _, err := protocol.Encode("", s); err != nil {
		t.Fatal(err)
	}
}

func TestProblemsWorstFirst(t *testing.T) {
	s := build(demo())
	var got []string
	for _, p := range s.Problems {
		got = append(got, p.Kind+"/"+p.Name+" "+p.Reason)
	}
	want := []string{
		"Node/node-2 NotReady",
		"Pod/crashloop-1 CrashLoopBackOff",
		"Pod/evicted-1 Evicted",
		"Pod/oom-1 CrashLoopBackOff",
		"Pod/pending-0 Unschedulable",
		"DaemonSet/agent Degraded",
		"Deployment/crashloop Degraded",
		"StatefulSet/pending Degraded",
		"Service/no-endpoints NoEndpoints",
		"Pod/6 Pods NodeNotReady",
		"Pod/web-1 Unhealthy",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	byName := map[string]protocol.Problem{}
	for _, p := range s.Problems {
		byName[p.Name] = p
	}
	if m := byName["node-2"].Message; !strings.Contains(m, "MemoryPressure") || byName["node-2"].Since.IsZero() {
		t.Errorf("node problem = %+v", byName["node-2"])
	}
	if m := byName["oom-1"].Message; !strings.HasPrefix(m, "last terminated: OOMKilled") || !strings.Contains(m, "3 restarts") {
		t.Errorf("oom problem = %q", m)
	}
	if p := byName["crashloop-1"]; !strings.Contains(p.Message, "7 restarts: back-off 5m0s") || !p.Since.Equal(now.Add(-time.Minute)) {
		t.Errorf("crash problem = %+v", p)
	}
	if m := byName["evicted-1"].Message; m != "The node was low on resource: memory." {
		t.Errorf("evicted = %q", m)
	}
	if m := byName["pending-0"].Message; m != "0/1 nodes are available: 1 Insufficient memory." {
		t.Errorf("pending = %q", m)
	}
	// Two events for the same object and reason add up; the latest message wins.
	if p := byName["web-1"]; p.Message != "7× Readiness probe failed: connection refused" || !p.Since.Equal(now.Add(-10*time.Second)) {
		t.Errorf("event = %+v", p)
	}
	if p := byName["6 Pods"]; !strings.Contains(p.Message, "on 6 objects, latest kubemap-demo/p0: Node is not ready") {
		t.Errorf("collapsed = %+v", p)
	}
}

func TestProblemLimit(t *testing.T) {
	f := demo()
	for i := range 80 {
		f.objs[podsGVR] = append(f.objs[podsGVR], pod("load", fmt.Sprintf("crash-%02d", i), corev1.PodRunning,
			corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}))
	}
	s := build(f)
	if len(s.Problems) != protocol.MaxProblems {
		t.Fatalf("%d problems, want %d", len(s.Problems), protocol.MaxProblems)
	}
	if s.Problems[0].Kind != "Node" {
		t.Error("the node problem must survive the cut")
	}
	if _, err := protocol.Encode("", s); err != nil {
		t.Fatal(err)
	}
}

func TestMissingReference(t *testing.T) {
	f := demo()
	p := pod("kubemap-demo", "needs-config", corev1.PodRunning)
	p.Spec.Volumes = []corev1.Volume{{Name: "c", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "never-created"}}}}}
	f.objs[podsGVR] = append(f.objs[podsGVR], p)
	f.objs[gvr("", "v1", "configmaps")] = []runtime.Object{}
	s := build(f)
	found := false
	for _, pr := range s.Problems {
		if pr.Kind == "ConfigMap" && pr.Name == "never-created" && pr.Reason == "Missing" &&
			strings.Contains(pr.Message, "Pod/kubemap-demo/needs-config") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing ConfigMap not reported: %+v", s.Problems)
	}
}

// The node lifecycle controller sets Ready to Unknown when the kubelet stops
// posting status: that must count as not ready, so the portal reads "node
// down" and not "node agent down" (KM-65 review N-3).
func TestNodeReadyUnknown(t *testing.T) {
	for st, want := range map[corev1.ConditionStatus]bool{corev1.ConditionTrue: true, corev1.ConditionFalse: false, corev1.ConditionUnknown: false} {
		if got := nodeReady(node("n", st)); got != want {
			t.Errorf("Ready=%s: nodeReady = %v, want %v", st, got, want)
		}
	}
	if nodeReady(&corev1.Node{}) {
		t.Error("a node without a Ready condition must not count as ready")
	}
}

func TestHelpers(t *testing.T) {
	long := strings.Repeat("é", 200)
	if c := clip(long); len(c) > 245 || !strings.HasSuffix(c, "…") || !strings.HasPrefix(c, "é") {
		t.Errorf("clip = %q", c)
	}
	if clip("a \n  b") != "a b" {
		t.Error("clip must fold whitespace")
	}
	if got := first([]string{"a", "b", "c", "d", "e"}, 3); strings.Join(got, ",") != "a,b,c,and 2 more" {
		t.Errorf("first = %v", got)
	}
	e := &corev1.Event{EventTime: metav1.NewMicroTime(now)}
	if !eventTime(e).Equal(now) {
		t.Error("eventTime from EventTime")
	}
	e = &corev1.Event{FirstTimestamp: metav1.NewTime(now)}
	if !eventTime(e).Equal(now) {
		t.Error("eventTime from FirstTimestamp")
	}
}

type recorder struct {
	sent []protocol.Payload
	err  error
}

func (r *recorder) Send(_ string, p protocol.Payload) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, p)
	return nil
}

func TestPusher(t *testing.T) {
	f := demo()
	rec := &recorder{}
	clock := now
	p := &Pusher{Src: f, Graph: graph.NewBuilder(f), Sender: rec, K8sVersion: "v1", Refresh: time.Minute, Now: func() time.Time { return clock }}
	tick := func() bool {
		sent, err := p.Tick()
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	if !tick() {
		t.Fatal("first tick must send")
	}
	clock = clock.Add(5 * time.Second)
	if tick() {
		t.Error("no change and no refresh due: must not send")
	}
	f.gen++
	if !tick() {
		t.Error("cache changed: must send")
	}
	clock = clock.Add(61 * time.Second)
	if !tick() {
		t.Error("refresh due: must send")
	}
	if len(rec.sent) != 3 || rec.sent[0].(*protocol.Summary).KubernetesVersion != "v1" {
		t.Errorf("sent %d", len(rec.sent))
	}
	rec.err = errors.New("tunnel closed")
	f.gen++
	if _, err := p.Tick(); err == nil {
		t.Error("send error must be returned")
	}
}

func TestPusherRun(t *testing.T) {
	f := demo()
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pusher{Src: f, Sender: rec, Interval: 5 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
	if len(rec.sent) != 1 {
		t.Errorf("unchanged cache sent %d summaries, want 1", len(rec.sent))
	}
	rec.err = errors.New("closed")
	p2 := &Pusher{Src: f, Sender: rec}
	if err := p2.Run(context.Background()); err == nil || err.Error() != "closed" {
		t.Errorf("Run with failing sender = %v", err)
	}
}

// review-km-24 B1: an event without a reason, or without an object, must not
// make the summary invalid (that stopped the pusher for good).
func TestEventWithoutReasonOrObject(t *testing.T) {
	f := &fakeSource{objs: map[schema.GroupVersionResource][]runtime.Object{}}
	f.objs[eventsGVR] = []runtime.Object{
		warning("Pod", "shop", "web-1", "", "something odd", time.Minute, 1),
		warning("Pod", "shop", "", "BackOff", "no object name", time.Minute, 1),
		warning("", "shop", "x", "BackOff", "no object kind", time.Minute, 1),
	}
	s := build(f)
	if err := s.Validate(); err != nil {
		t.Fatalf("summary invalid: %v", err)
	}
	if len(s.Problems) != 1 || s.Problems[0].Name != "web-1" || s.Problems[0].Reason != "Unknown" {
		t.Errorf("problems = %+v", s.Problems)
	}
	rec := &recorder{}
	p := &Pusher{Src: f, Sender: rec, Now: func() time.Time { return now }}
	if sent, err := p.Tick(); !sent || err != nil {
		t.Errorf("tick = %v, %v", sent, err)
	}
}

// An invalid summary is skipped and logged; the pusher keeps running.
func TestPusherSkipsInvalidSummary(t *testing.T) {
	f := demo()
	rec := &recorder{}
	var logged strings.Builder
	p := &Pusher{Src: f, Sender: rec, Now: func() time.Time { return time.Time{} }, // zero time: invalid
		Log: slog.New(slog.NewTextHandler(&logged, nil))}
	sent, err := p.Tick()
	if sent || err != nil || len(rec.sent) != 0 {
		t.Errorf("tick = %v, %v, sent %d", sent, err, len(rec.sent))
	}
	if !strings.Contains(logged.String(), "summary: invalid") {
		t.Errorf("log = %q", logged.String())
	}
	p.Now = func() time.Time { return now }
	if sent, err := p.Tick(); !sent || err != nil {
		t.Errorf("next valid tick = %v, %v", sent, err)
	}
}
