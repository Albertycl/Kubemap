package query

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"kubemap/pkg/protocol"
)

func TestPodDiagnoseOp(t *testing.T) {
	p := pod("shop", "web-1", "n1", map[string]string{"app": "web"})
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs"}}
	p.Spec.Containers = []corev1.Container{{Name: "app", Image: "web:1"}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
	objs := map[schema.GroupVersionResource][]runtime.Object{gPods: {p}}
	h, kube, _ := newHandler(t, objs)
	r := do(h, &protocol.Request{Op: protocol.OpPodDiagnose, Namespace: "shop", Name: "web-1"})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var d protocol.PodDiagnosis
	if err := json.Unmarshal(r.Object, &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != "CrashLoopBackOff" || d.Findings[0].Title != "CrashLoopBackOff" || d.Containers[0].LogsFrom != "previous" {
		t.Errorf("diagnosis = %+v", d)
	}
	for _, a := range kube.Actions() {
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("diagnose made a %s call", v)
		}
	}
	// Not in the cache and not in the API: not_found.
	if r := do(h, &protocol.Request{Op: protocol.OpPodDiagnose, Namespace: "shop", Name: "nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown pod: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodDiagnose, Namespace: "shop"}); code(r) != protocol.CodeBadRequest {
		t.Errorf("no name: %+v", r.Error)
	}
}

func TestNodePressureOp(t *testing.T) {
	n1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}
	h, kube, _ := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{nodesGVR: {n1, n2}})
	busy := protocol.Metrics{NodeName: "n1", Time: time.Now().Add(-5 * time.Second),
		PSI: &protocol.PSIStats{CPU: protocol.Pressure{Some: protocol.PressureAvg{Avg60: 40}}}}
	r := do(h, &protocol.Request{Op: protocol.OpNodePressure, NodeMetrics: []protocol.Metrics{busy}})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var rep protocol.NodePressureReport
	if err := json.Unmarshal(r.Object, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Nodes) != 2 || rep.Nodes[0].Verdict != protocol.VerdictPressure || rep.Nodes[1].PSIAvailable || len(rep.Notes) != 1 {
		t.Errorf("report = %+v", rep)
	}
	r = do(h, &protocol.Request{Op: protocol.OpNodePressure, Name: "n2"})
	if err := json.Unmarshal(r.Object, &rep); err != nil || len(rep.Nodes) != 1 || rep.Nodes[0].Node != "n2" {
		t.Errorf("one node: %+v %v", rep, err)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpNodePressure, Name: "nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown node: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpNodePressure, Namespace: "x"}); code(r) != protocol.CodeBadRequest {
		t.Errorf("namespace: %+v", r.Error)
	}
	// No Kubernetes API call, and (there being no kubelet client) no kubelet call.
	if len(kube.Actions()) != 0 {
		t.Errorf("node.pressure called the API: %v", kube.Actions())
	}
}

func TestHealthCheckOp(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	crash := pod("shop", "web-1", "n1", nil)
	crash.Spec.Containers = []corev1.Container{{Name: "app", Image: "web:1"}}
	crash.Status.Phase = corev1.PodRunning
	crash.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
	other := pod("other", "db-1", "n1", nil)
	other.Spec.Containers = []corev1.Container{{Name: "db", Image: "db:1"}}
	other.Status.Phase = corev1.PodRunning
	objs := map[schema.GroupVersionResource][]runtime.Object{nodesGVR: {n}, gPods: {crash, other}}
	h, kube, _ := newHandler(t, objs)
	swapFull := []protocol.Metrics{{NodeName: "n1", Time: time.Now(), Swap: protocol.SwapStats{TotalBytes: 100, UsedBytes: 99}}}
	r := do(h, &protocol.Request{Op: protocol.OpHealthCheck, NodeMetrics: swapFull})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var rep protocol.HealthReport
	if err := json.Unmarshal(r.Object, &rep); err != nil {
		t.Fatal(err)
	}
	rules := map[string]bool{}
	for _, f := range rep.Findings {
		rules[f.Rule] = true
	}
	if !rules["pod.crashloop"] || !rules["node.swap"] || !rules["pod.no-memory-limit"] || rep.Thresholds.Source != "kubelet defaults" {
		t.Errorf("report = %+v", rep)
	}
	r = do(h, &protocol.Request{Op: protocol.OpHealthCheck, Namespace: "other"})
	if err := json.Unmarshal(r.Object, &rep); err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Rule == "pod.crashloop" {
			t.Errorf("namespace other: %+v", f)
		}
	}
	if len(kube.Actions()) != 0 {
		t.Errorf("health.check called the API: %v", kube.Actions())
	}
}

func TestPodUsageOp(t *testing.T) {
	web := pod("shop", "web-1", "n1", nil)
	web.Spec.Containers = []corev1.Container{{Name: "app", Image: "web:1"}}
	web.Status.Phase = corev1.PodRunning
	pending := pod("shop", "wait-1", "", nil)
	other := pod("other", "db-1", "n2", nil)
	metrics := func(ns, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
			"metadata":  map[string]any{"namespace": ns, "name": name},
			"timestamp": "2026-09-28T02:12:32Z", "window": "15s",
			"containers": []any{map[string]any{"name": "app", "usage": map[string]any{"cpu": "5m", "memory": "10Mi"}}},
		}}
	}
	h, kube, dyn := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: {web, pending, other}})
	for _, m := range []*unstructured.Unstructured{metrics("shop", "web-1"), metrics("other", "db-1")} {
		if err := dyn.Tracker().Create(gMetrics, m, m.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	var rep protocol.PodUsageReport
	r := do(h, &protocol.Request{Op: protocol.OpPodUsage, Namespace: "shop"})
	if err := json.Unmarshal(r.Object, &rep); err != nil || len(rep.Pods) != 1 || rep.Pods[0].Node != "n1" || rep.Pods[0].MemoryBytes != 10<<20 {
		t.Errorf("namespace shop: %+v %v %v", rep, err, r.Error)
	}
	r = do(h, &protocol.Request{Op: protocol.OpPodUsage})
	if err := json.Unmarshal(r.Object, &rep); err != nil || len(rep.Pods) != 2 {
		t.Errorf("all: %+v %v", rep, err)
	}
	r = do(h, &protocol.Request{Op: protocol.OpPodUsage, Namespace: "shop", Name: "web-1"})
	if err := json.Unmarshal(r.Object, &rep); err != nil || len(rep.Pods) != 1 || rep.Pods[0].Containers[0].CPUMillis != 5 {
		t.Errorf("one pod: %+v %v %v", rep, err, r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodUsage, Namespace: "shop", Name: "wait-1"}); code(r) != protocol.CodeNotFound {
		t.Errorf("pod without metrics: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodUsage, Namespace: "shop", Name: "nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown pod: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodUsage, Name: "web-1"}); code(r) != protocol.CodeBadRequest {
		t.Errorf("name without namespace: %+v", r.Error)
	}
	for _, a := range dyn.Actions() {
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("pod.usage made a %s call", v)
		}
	}
	if len(kube.Actions()) != 0 {
		t.Errorf("pod.usage called the core API: %v", kube.Actions())
	}
	// RBAC missing: a clear forbidden answer.
	dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, "", errors.New("RBAC"))
	})
	if r := do(h, &protocol.Request{Op: protocol.OpPodUsage}); code(r) != protocol.CodeForbidden || !strings.Contains(r.Error.Message, "pods.metrics.k8s.io") {
		t.Errorf("forbidden: %+v", r.Error)
	}
}
