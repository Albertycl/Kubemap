package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"kubemap/pkg/protocol"
)

func readJSON(t *testing.T, file string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// real loads a pod and its events exported read-only from the lab cluster
// on 2026-09-28 (env, args, volumes and annotations removed).
func real(t *testing.T, name string) Inputs {
	t.Helper()
	var p corev1.Pod
	readJSON(t, "pod-"+name+".json", &p)
	var evs corev1.EventList
	readJSON(t, "events-"+name+".json", &evs)
	return Inputs{Pod: &p, Events: evs.Items}
}

func titles(d *protocol.PodDiagnosis) []string {
	var out []string
	for _, f := range d.Findings {
		out = append(out, f.Severity+" "+f.Title)
	}
	return out
}

func has(d *protocol.PodDiagnosis, sevTitle string) bool { return slices.Contains(titles(d), sevTitle) }

// The first real case: after the disk-pressure evictions, url-shortener's
// new pods cannot start because its locally imported image (pull policy
// Never) was removed by the kubelet's image garbage collection.
func TestRealErrImageNeverPull(t *testing.T) {
	d := Analyse(real(t, "url-shortener-6f89bdc657-rgwsv"))
	if d.Status != "ErrImageNeverPull" || d.Findings[0].Title != "Image not on the node" || d.Findings[0].Severity != "critical" {
		t.Fatalf("status %s, findings %v", d.Status, titles(d))
	}
	f := d.Findings[0].Detail
	for _, want := range []string{"docker.io/library/url-shortener:v0.2", "k8server", "imagePullPolicy is Never", "garbage collection"} {
		if !strings.Contains(f, want) {
			t.Errorf("detail lacks %q: %s", want, f)
		}
	}
	if len(d.Events) == 0 || d.Events[0].Reason == "" {
		t.Errorf("events = %+v", d.Events)
	}
	// Newest first.
	for i := 1; i < len(d.Events); i++ {
		if d.Events[i].Last.After(d.Events[i-1].Last) {
			t.Errorf("events not newest first: %v after %v", d.Events[i].Last, d.Events[i-1].Last)
		}
	}
}

// The old url-shortener pod was evicted for ephemeral-storage: on 1.34 it
// ends Succeeded (exit 0) with only a DisruptionTarget condition to say so.
func TestRealEvictedPod(t *testing.T) {
	d := Analyse(real(t, "url-shortener-6f89bdc657-qptcg"))
	if d.Status != "Evicted" || d.Phase != "Succeeded" || d.Findings[0].Title != "Evicted" {
		t.Fatalf("status %s phase %s findings %v", d.Status, d.Phase, titles(d))
	}
	if !strings.Contains(d.Findings[0].Detail, "ephemeral-storage") {
		t.Errorf("detail = %s", d.Findings[0].Detail)
	}
	c := d.Containers[0]
	if c.Restarts != 20 || c.Last == nil || c.Last.ExitCode != 0 || !strings.HasPrefix(c.Last.Meaning, "0:") {
		t.Errorf("container = %+v", c)
	}
	// Restarts from before, but it ended cleanly: no crash finding.
	if has(d, "warning Restarted 20 times") {
		t.Errorf("a clean exit is not a crash: %v", titles(d))
	}
}

// flask-vue-backend, recreated after the evictions, is healthy: the report
// says so and shows its limits and probes.
func TestRealHealthyBackend(t *testing.T) {
	in := real(t, "flask-vue-backend-6688dd7cbf-6tspt")
	in.Usage = map[string]Usage{"backend": {CPUMillis: 1, MemoryBytes: 45 << 20}}
	d := Analyse(in)
	if !has(d, "info No problem found") || len(d.Findings) != 1 {
		t.Errorf("findings = %v", titles(d))
	}
	c := d.Containers[0]
	if c.Limits["memory"] != "256Mi" || c.Requests["cpu"] != "50m" || c.Usage == nil || c.Usage.MemOfLimit != 17.6 || c.Usage.CPUOfLimit != 0.4 {
		t.Errorf("resources = %+v %+v %+v", c.Requests, c.Limits, c.Usage)
	}
	if len(c.Probes) != 2 || !strings.HasPrefix(c.Probes[0], "liveness: GET /api/hello :http every 10s, timeout 1s, 3 failures") {
		t.Errorf("probes = %v", c.Probes)
	}
}

func cont(name string, limit string) corev1.Container {
	c := corev1.Container{Name: name, Image: "app:1"}
	if limit != "" {
		c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit), corev1.ResourceCPU: resource.MustParse("100m")}
	}
	return c
}

func podWith(c corev1.Container, cs corev1.ContainerStatus) *corev1.Pod {
	cs.Name = c.Name
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "p", UID: "u1"},
		Spec:   corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{c}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

func crash(reason string, code int32, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{RestartCount: restarts,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}}
}

func TestOOMKilledCrashLoop(t *testing.T) {
	in := Inputs{Pod: podWith(cont("app", "64Mi"), crash("OOMKilled", 137, 7)),
		Usage: map[string]Usage{"app": {CPUMillis: 95, MemoryBytes: 60 << 20}},
		Logs:  map[string]Log{"app": {Text: "loading cache\n", From: "previous"}}}
	d := Analyse(in)
	want := []string{"critical CrashLoopBackOff", "critical Killed for memory (exit 137)", "warning Near the memory limit", "warning At the CPU limit"}
	if got := titles(d); !slices.Equal(got, want) {
		t.Errorf("findings = %v, want %v", got, want)
	}
	if f := d.Findings[1].Detail; !strings.Contains(f, "its 64Mi memory limit") || !strings.Contains(f, "60Mi now, 94%") || !strings.Contains(f, "reason OOMKilled") {
		t.Errorf("OOM detail = %s", d.Findings[1].Detail)
	}
	if !strings.Contains(d.Findings[0].Detail, "crashed 7 times") || !strings.Contains(d.Findings[0].Detail, "previous run's last log lines") {
		t.Errorf("crashloop detail = %s", d.Findings[0].Detail)
	}
	if c := d.Containers[0]; c.Logs != "loading cache\n" || c.LogsFrom != "previous" || c.State != "waiting: CrashLoopBackOff" {
		t.Errorf("container = %+v", c)
	}
}

func TestLivenessKill(t *testing.T) {
	c := cont("app", "128Mi")
	c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}}
	cs := crash("Error", 137, 3)
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	now := metav1.Now()
	in := Inputs{Pod: podWith(c, cs), Events: []corev1.Event{
		{Type: "Warning", Reason: "Unhealthy", Message: "Liveness probe failed: dial tcp 10.0.0.1:8080: connect: connection refused", Count: 9, LastTimestamp: now},
		{Type: "Normal", Reason: "Killing", Message: "Container app failed liveness probe, will be restarted", LastTimestamp: now},
	}}
	d := Analyse(in)
	for _, want := range []string{"critical Killed by the liveness probe (exit 137)", "warning Liveness probe failing"} {
		if !has(d, want) {
			t.Errorf("missing %q in %v", want, titles(d))
		}
	}
	if !strings.Contains(d.Findings[len(d.Findings)-1].Detail, "9 liveness failures") {
		t.Errorf("detail = %v", d.Findings)
	}
	if d.Containers[0].Probes[0] != "liveness: TCP :8080 every 1s, timeout 1s, 1 failures, first after 0s" {
		t.Errorf("probe = %q", d.Containers[0].Probes[0])
	}
}

func TestOtherWaitingReasonsAndSchedule(t *testing.T) {
	for reason, title := range map[string]string{
		"ImagePullBackOff":           "Image cannot be pulled",
		"CreateContainerConfigError": "Configuration missing",
		"RunContainerError":          "Container cannot start",
		"SomethingNew":               "Waiting: SomethingNew",
	} {
		cs := corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "m"}}}
		d := Analyse(Inputs{Pod: podWith(cont("app", "1Gi"), cs)})
		if len(d.Findings) == 0 || d.Findings[0].Title != title {
			t.Errorf("%s: %v", reason, titles(d))
		}
	}
	p := podWith(cont("app", ""), corev1.ContainerStatus{})
	p.Status.Phase = corev1.PodPending
	p.Spec.NodeName = ""
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available: 1 node(s) had untolerated taint(s)."}}
	d := Analyse(Inputs{Pod: p})
	if d.Findings[0].Title != "Not scheduled: Unschedulable" || !has(d, "info No memory limit") || d.Status != "Pending (Unschedulable)" {
		t.Errorf("pending: %s %v", d.Status, titles(d))
	}
}

func TestRestartsAndDisruptions(t *testing.T) {
	cs := crash("Error", 1, 4)
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	d := Analyse(Inputs{Pod: podWith(cont("app", "1Gi"), cs)})
	if !has(d, "warning Restarted 4 times") {
		t.Errorf("exit 1: %v", titles(d))
	}
	cs = crash("Error", 143, 2)
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if d := Analyse(Inputs{Pod: podWith(cont("app", "1Gi"), cs)}); !strings.Contains(d.Findings[0].Detail, "SIGTERM") {
		t.Errorf("exit 143: %v", d.Findings)
	}
	for reason, title := range map[string]string{
		"PreemptionByScheduler": "Preempted", "EvictionByEvictionAPI": "Evicted by the Eviction API",
		"DeletionByTaintManager": "Deleted for a NoExecute taint", "DeletionByPodGC": "Deleted by pod garbage collection", "Other": "Disrupted: Other",
	} {
		p := podWith(cont("app", "1Gi"), corev1.ContainerStatus{})
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reason}}
		if d := Analyse(Inputs{Pod: p}); d.Findings[0].Title != title {
			t.Errorf("%s: %v", reason, titles(d))
		}
	}
	p := podWith(cont("app", "1Gi"), corev1.ContainerStatus{})
	p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, "Evicted", "The node was low on resource: memory."
	if d := Analyse(Inputs{Pod: p}); d.Status != "Evicted" || d.Findings[0].Title != "Evicted" {
		t.Errorf("old-style eviction: %s %v", d.Status, titles(d))
	}
	// A FailedMount event is critical.
	p = podWith(cont("app", "1Gi"), corev1.ContainerStatus{})
	d = Analyse(Inputs{Pod: p, Events: []corev1.Event{{Type: "Warning", Reason: "FailedMount", Message: `configmap "x" not found`, Count: 3}}})
	if d.Findings[0].Title != "Volume mount failing" || !strings.Contains(d.Findings[0].Detail, `configmap "x" not found`) {
		t.Errorf("failed mount: %v", d.Findings)
	}
}

func TestExitMeaning(t *testing.T) {
	for code, prefix := range map[int32]string{0: "0:", 1: "1:", 2: "2:", 126: "126:", 127: "127:", 130: "SIGINT", 134: "SIGABRT",
		137: "SIGKILL", 139: "SIGSEGV", 143: "SIGTERM", 255: "255:", 131: "signal 3", 42: "42: application"} {
		if m := ExitMeaning(code, ""); !strings.HasPrefix(m, prefix) {
			t.Errorf("%d: %q", code, m)
		}
	}
	if m := ExitMeaning(137, "OOMKilled"); !strings.HasPrefix(m, "OOMKilled (137)") {
		t.Errorf("oom: %q", m)
	}
}

func TestCollect(t *testing.T) {
	var p corev1.Pod
	readJSON(t, "pod-flask-vue-backend-6688dd7cbf-6tspt.json", &p)
	var evs corev1.EventList
	readJSON(t, "events-flask-vue-backend-6688dd7cbf-6tspt.json", &evs)
	other := evs.Items[0]
	other.Name, other.InvolvedObject.UID = "older-pod-event", "another-uid" // same name, older pod
	objs := []runtime.Object{&other}
	for i := range evs.Items {
		objs = append(objs, &evs.Items[i])
	}
	kube := kubefake.NewClientset(objs...)
	metrics := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata":   map[string]any{"namespace": p.Namespace, "name": p.Name},
		"containers": []any{map[string]any{"name": "backend", "usage": map[string]any{"cpu": "1500000n", "memory": "46080Ki"}}},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{PodMetrics: "PodMetricsList"})
	// The metrics API serves PodMetrics as resource "pods"; the fake would
	// guess "podmetrics" from the kind.
	if err := dyn.Tracker().Create(PodMetrics, metrics, p.Namespace); err != nil {
		t.Fatal(err)
	}
	c := &Collector{Kube: kube, Dynamic: dyn,
		Pod:   func(ns, name string) (*corev1.Pod, error) { return &p, nil },
		Owner: func(*corev1.Pod) string { return "Deployment/sre-lab-app/flask-vue-backend" }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := c.Diagnose(ctx, p.Namespace, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != len(evs.Items) || d.Owner != "Deployment/sre-lab-app/flask-vue-backend" {
		t.Errorf("events %d (want %d, the older pod's left out), owner %q", len(d.Events), len(evs.Items), d.Owner)
	}
	bc := d.Containers[0]
	if bc.Usage == nil || bc.Usage.CPUMillis != 2 || bc.Usage.MemoryBytes != 45<<20 {
		t.Errorf("usage = %+v", bc.Usage)
	}
	if bc.LogsFrom != "current" || bc.Logs == "" { // the fake clientset serves "fake logs"
		t.Errorf("logs = %q from %q", bc.Logs, bc.LogsFrom)
	}
	var logOpts *corev1.PodLogOptions
	for _, a := range kube.Actions() {
		if a.GetSubresource() == "log" {
			logOpts = a.(clienttesting.GenericActionImpl).Value.(*corev1.PodLogOptions)
		}
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("a %s call: diagnose must only read", v)
		}
	}
	if logOpts == nil || *logOpts.TailLines != 100 || logOpts.Previous {
		t.Errorf("log options = %+v", logOpts)
	}

	// Forbidden metrics and events become notes, not errors.
	dyn.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, p.Name, errors.New("RBAC"))
	})
	kube.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("RBAC"))
	})
	d, err = c.Diagnose(ctx, p.Namespace, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Notes) != 2 || !strings.Contains(d.Notes[0], "events not read: forbidden") || !strings.Contains(d.Notes[1], "usage not available: forbidden") {
		t.Errorf("notes = %v", d.Notes)
	}
	// No clients at all.
	d, _ = (&Collector{Pod: c.Pod}).Diagnose(ctx, p.Namespace, p.Name)
	if len(d.Notes) != 2 {
		t.Errorf("notes without clients = %v", d.Notes)
	}
	// An unknown pod is an error.
	if _, err := (&Collector{Pod: func(string, string) (*corev1.Pod, error) { return nil, errors.New("no pod") }}).Diagnose(ctx, "a", "b"); err == nil {
		t.Error("unknown pod: no error")
	}
}

func TestCollectPreviousLogsAndNeverStarted(t *testing.T) {
	p := podWith(cont("app", "64Mi"), crash("OOMKilled", 137, 2))
	init := corev1.Container{Name: "init"}
	p.Spec.InitContainers = []corev1.Container{init}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
	kube := kubefake.NewClientset()
	c := &Collector{Kube: kube, Pod: func(string, string) (*corev1.Pod, error) { return p, nil }}
	d, err := c.Diagnose(context.Background(), "shop", "p")
	if err != nil {
		t.Fatal(err)
	}
	var app, ini protocol.DiagContainer
	for _, x := range d.Containers {
		if x.Init {
			ini = x
		} else {
			app = x
		}
	}
	if app.LogsFrom != "previous" || ini.Logs != "" || ini.LogsFrom != "" {
		t.Errorf("app logs from %q, init logs %q", app.LogsFrom, ini.Logs)
	}
	for _, a := range kube.Actions() {
		if a.GetSubresource() == "log" && !a.(clienttesting.GenericActionImpl).Value.(*corev1.PodLogOptions).Previous {
			t.Error("a crashed container's logs must come from the previous run")
		}
	}
}

// KM-71 F1: the demo's oom pod on k8server. The kernel OOM-kills it, but
// k3s with containerd 2.2 reports "Error, exit 137", not "OOMKilled". The
// crash helper must still say "killed for memory", and must not blame a
// liveness probe the pod does not have. Unlike the other real fixtures,
// this one keeps the container's command on purpose: it is the demo's own
// awk line that needs 32 MiB (deploy/demo/10-oom.yaml), and has no secret.
func TestRealDemoOOMReportedAsError(t *testing.T) {
	d := Analyse(real(t, "demo-oom"))
	c := d.Containers[0]
	if c.Last == nil || c.Last.Reason != "Error" || c.Last.ExitCode != 137 {
		t.Fatalf("fixture: last = %+v", c.Last)
	}
	if !has(d, "critical Killed for memory (exit 137)") {
		t.Fatalf("findings = %v", titles(d))
	}
	for _, f := range d.Findings {
		if strings.Contains(f.Title, "liveness") || strings.Contains(f.Detail, "liveness probe failed") {
			t.Errorf("blames a liveness probe the pod does not have: %+v", f)
		}
		if f.Title == "Killed for memory (exit 137)" {
			for _, want := range []string{"24Mi memory limit", `reason "Error"`, "journalctl -k"} {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("detail lacks %q: %s", want, f.Detail)
				}
			}
		}
	}
	if !strings.Contains(c.Last.Meaning, "OOM killer") {
		t.Errorf("meaning = %q", c.Last.Meaning)
	}
}

func TestExit137Variants(t *testing.T) {
	// A liveness probe exists, but nothing shows it killed the container:
	// the evidence still points to memory.
	c := cont("app", "128Mi")
	c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}}
	d := Analyse(Inputs{Pod: podWith(c, crash("Error", 137, 2))})
	if !has(d, "critical Killed for memory (exit 137)") {
		t.Errorf("probe without a kill event: %v", titles(d))
	}
	// No memory limit: the node ran out of memory, or a forced stop.
	d = Analyse(Inputs{Pod: podWith(cont("app", ""), crash("", 137, 1))})
	if f := d.Findings[1]; f.Title != "Killed for memory (exit 137)" || !strings.Contains(f.Detail, "no memory limit") || !strings.Contains(f.Detail, `reason "none"`) {
		t.Errorf("no limit: %+v", d.Findings)
	}
	// Another container's liveness kill is not this container's.
	in := Inputs{Pod: podWith(cont("app", "64Mi"), crash("Error", 137, 1)), Events: []corev1.Event{
		{Type: "Normal", Reason: "Killing", Message: "Container sidecar failed liveness probe, will be restarted"}}}
	if d := Analyse(in); !has(d, "critical Killed for memory (exit 137)") {
		t.Errorf("sidecar's kill: %v", titles(d))
	}
}

// KM-71 F2: the previous-run log read is retried once.
func TestPreviousLogsRetry(t *testing.T) {
	old := PreviousLogRetry
	PreviousLogRetry = 0
	defer func() { PreviousLogRetry = old }()
	p := podWith(cont("app", "64Mi"), crash("Error", 137, 2))
	calls, alwaysFail := 0, false
	c := &Collector{Kube: kubefake.NewClientset(), Pod: func(string, string) (*corev1.Pod, error) { return p, nil },
		LogStream: func(_ context.Context, _, _ string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
			calls++
			if calls == 1 || alwaysFail {
				return nil, errors.New("unable to retrieve container logs for containerd://abc")
			}
			return io.NopCloser(strings.NewReader("building monthly report (needs 32 MiB)\n")), nil
		}}
	d, err := c.Diagnose(context.Background(), "shop", "p")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || d.Containers[0].LogsFrom != "previous" || !strings.Contains(d.Containers[0].Logs, "32 MiB") {
		t.Errorf("calls %d, logs %+v, notes %v", calls, d.Containers[0], d.Notes)
	}
	// Two failures: a note, no error.
	alwaysFail = true
	d, _ = c.Diagnose(context.Background(), "shop", "p")
	if len(d.Notes) == 0 || !strings.Contains(d.Notes[0], "logs of app not read: unable to retrieve") {
		t.Errorf("notes = %v", d.Notes)
	}
}

// review-km-56-exit137 N-1 and N-2: a startup-probe kill counts too, and a
// probe kill only explains the run it ended.
func TestProbeKillEvidence(t *testing.T) {
	end := metav1.NewTime(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))
	cs := crash("Error", 137, 2)
	cs.LastTerminationState.Terminated.FinishedAt = end
	for _, tc := range []struct {
		name, msg string
		at        time.Time
		want      string
	}{
		{"startup probe, just before", "Container app failed startup probe, will be restarted", end.Add(-20 * time.Second), "critical Killed by the startup probe (exit 137)"},
		{"liveness probe, just before", "Container app failed liveness probe, will be restarted", end.Add(-31 * time.Second), "critical Killed by the liveness probe (exit 137)"},
		{"liveness probe, 40 minutes earlier", "Container app failed liveness probe, will be restarted", end.Add(-40 * time.Minute), "critical Killed for memory (exit 137)"},
		{"a Killing event for another reason", "Stopping container app", end.Add(-2 * time.Second), "critical Killed for memory (exit 137)"},
	} {
		in := Inputs{Pod: podWith(cont("app", "64Mi"), cs), Events: []corev1.Event{{Type: "Normal", Reason: "Killing", Message: tc.msg, LastTimestamp: metav1.NewTime(tc.at)}}}
		if d := Analyse(in); !has(d, tc.want) {
			t.Errorf("%s: %v", tc.name, titles(d))
		}
	}
	// A custom grace period widens the window.
	g := int64(300)
	p := podWith(cont("app", "64Mi"), cs)
	p.Spec.TerminationGracePeriodSeconds = &g
	in := Inputs{Pod: p, Events: []corev1.Event{{Type: "Normal", Reason: "Killing", Message: "Container app failed liveness probe, will be restarted",
		LastTimestamp: metav1.NewTime(end.Add(-4 * time.Minute))}}}
	if d := Analyse(in); !has(d, "critical Killed by the liveness probe (exit 137)") {
		t.Errorf("300 s grace: %v", titles(d))
	}
}
