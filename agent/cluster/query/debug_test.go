package query

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

func TestPodDebugOp(t *testing.T) {
	p := pod("shop", "web-1", "n1", map[string]string{"app": "web"})
	p.Spec.Containers = []corev1.Container{{Name: "app", Image: "web:1"}}
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
	h, kube, _ := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: {p}})
	r := do(h, &protocol.Request{Op: protocol.OpPodDebug, Namespace: "shop", Name: "web-1"})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var d protocol.PodDebug
	if err := json.Unmarshal(r.Object, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Steps) != 8 || d.Steps[0].Verdict != protocol.VerdictProblem || d.Cause == "" {
		t.Errorf("debug = %+v", d)
	}
	// Services and NetworkPolicies are not in this cache: unknown, with notes.
	if d.Steps[6].Verdict != protocol.VerdictUnknown || d.Steps[7].Verdict != protocol.VerdictUnknown || len(d.Notes) < 2 {
		t.Errorf("steps 7-8 = %s %s, notes %v", d.Steps[6].Verdict, d.Steps[7].Verdict, d.Notes)
	}
	for _, a := range kube.Actions() {
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("pod.debug made a %s call", v)
		}
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodDebug, Namespace: "shop", Name: "nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown pod: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpPodDebug, Name: "web-1"}); code(r) != protocol.CodeBadRequest {
		t.Errorf("no namespace: %+v", r.Error)
	}
}
