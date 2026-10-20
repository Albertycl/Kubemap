package debug

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/diagnose"
	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, ns string, _ labels.Selector) ([]runtime.Object, error) {
	l, ok := f[g]
	if !ok {
		return nil, os.ErrNotExist
	}
	return l, nil
}
func (f fakeSource) Generation() uint64 { return 1 }

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

func load[T any, P interface {
	*T
	runtime.Object
}](t *testing.T, file string) []runtime.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []T }
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := make([]runtime.Object, len(list.Items))
	for i := range list.Items {
		out[i] = P(&list.Items[i])
	}
	return out
}

// demo is the kubemap-demo namespace with its five broken scenarios, and
// the node, exported read-only on 2026-09-28 (KM-71).
func demo(t *testing.T) fakeSource {
	t.Helper()
	return fakeSource{
		podsGVR:                     load[corev1.Pod](t, "pods.json"),
		nodesGVR:                    load[corev1.Node](t, "nodes.json"),
		gvr("", "v1", "services"):   load[corev1.Service](t, "services.json"),
		gvr("", "v1", "namespaces"): load[corev1.Namespace](t, "namespaces.json"),
		gvr("", "v1", "configmaps"): load[corev1.ConfigMap](t, "configmaps.json"),
		gvr("discovery.k8s.io", "v1", "endpointslices"):   load[discoveryv1.EndpointSlice](t, "endpointslices.json"),
		gvr("networking.k8s.io", "v1", "networkpolicies"): load[networkingv1.NetworkPolicy](t, "networkpolicies.json"),
		gvr("apps", "v1", "deployments"):                  load[appsv1.Deployment](t, "deployments.json"),
		gvr("apps", "v1", "replicasets"):                  load[appsv1.ReplicaSet](t, "replicasets.json"),
	}
}

// walk debugs the pod of one app, the way the handler does: KM-56's
// diagnosis (with the saved events and logs), then the checklist.
func walk(t *testing.T, src fakeSource, app string) *protocol.PodDebug {
	t.Helper()
	var pod *corev1.Pod
	for _, o := range src[podsGVR] {
		if p := o.(*corev1.Pod); p.Labels["app.kubernetes.io/name"] == app {
			pod = p
		}
	}
	if pod == nil {
		t.Fatalf("no pod for %s", app)
	}
	var evs []corev1.Event
	for _, o := range load[corev1.Event](t, "events.json") {
		if e := o.(*corev1.Event); e.InvolvedObject.Name == pod.Name {
			evs = append(evs, *e)
		}
	}
	var saved map[string]struct{ From, Text string }
	b, _ := os.ReadFile("testdata/logs.json")
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	logs := map[string]diagnose.Log{}
	for _, c := range pod.Spec.Containers {
		if l, ok := saved[pod.Name+"/"+c.Name]; ok {
			logs[c.Name] = diagnose.Log{Text: l.Text, From: l.From}
		}
	}
	g := graph.Build(src)
	owner := g.Owner(graph.ID("Pod", pod.Namespace, pod.Name))
	d := diagnose.Analyse(diagnose.Inputs{Pod: pod, Owner: owner, Events: evs, Logs: logs})
	return Walk(FromCache(src, g, pod, d))
}

func stepOf(t *testing.T, d *protocol.PodDebug, key string) protocol.DebugStep {
	t.Helper()
	for _, s := range d.Steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no step %s", key)
	return protocol.DebugStep{}
}

func verdicts(d *protocol.PodDebug) string {
	var v []string
	for _, s := range d.Steps {
		v = append(v, s.Key+"="+s.Verdict)
	}
	return strings.Join(v, " ")
}

// TestDemoScenarios runs the checklist on all five kubemap-demo scenarios
// and checks that the cause line names the real root cause.
func TestDemoScenarios(t *testing.T) {
	src := demo(t)
	for _, tc := range []struct {
		scenario, app, causeStep string
		causeHas                 []string
		problems                 []string // steps that must say "problem"
	}{
		{"oom", "oom", protocol.StepResources, []string{"Killed for memory (exit 137)", "24Mi memory limit"},
			[]string{protocol.StepStatus, protocol.StepExitCode, protocol.StepResources}},
		{"crashloop", "checkout", protocol.StepLogs, []string{"FATAL: required setting DATABASE_URL is not set"},
			[]string{protocol.StepStatus, protocol.StepLogs, protocol.StepExitCode}},
		{"netpol (the client)", "web", protocol.StepNetpol, []string{"kubemap-demo/api-allow-monitoring-only", "this pod → Service kubemap-demo/api:80"},
			[]string{protocol.StepLogs, protocol.StepNetpol}},
		{"netpol (the server)", "api", protocol.StepNetpol, []string{"kubemap-demo/api-allow-monitoring-only", "caller kubemap-demo/web-"},
			[]string{protocol.StepNetpol}},
		{"badsvc", "catalog", protocol.StepService, []string{"Service kubemap-demo/catalog selects app.kubernetes.io/name=catalog-api", "app.kubernetes.io/name=catalog"},
			[]string{protocol.StepService}},
		{"pending", "batch-worker", protocol.StepNode, []string{"No node can ever run it: it requests 8 CPU", "k8server"},
			[]string{protocol.StepStatus, protocol.StepEvents, protocol.StepNode}},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			d := walk(t, src, tc.app)
			t.Logf("%s: %s\n  cause [%s] %s", tc.scenario, verdicts(d), d.CauseStep, d.Cause)
			if len(d.Steps) != 8 {
				t.Fatalf("%d steps", len(d.Steps))
			}
			if d.CauseStep != tc.causeStep {
				t.Errorf("cause step %q, want %q: %s", d.CauseStep, tc.causeStep, d.Cause)
			}
			for _, want := range tc.causeHas {
				if !strings.Contains(d.Cause, want) {
					t.Errorf("cause %q lacks %q", d.Cause, want)
				}
			}
			for _, key := range tc.problems {
				if s := stepOf(t, d, key); s.Verdict != protocol.VerdictProblem {
					t.Errorf("step %s = %s (%s)", key, s.Verdict, s.Finding)
				}
			}
			for _, s := range d.Steps {
				if s.Finding == "" || len(s.Commands) == 0 || s.Title == "" {
					t.Errorf("step %s incomplete: %+v", s.Key, s)
				}
				for _, c := range s.Commands {
					if !strings.HasPrefix(c, "kubectl ") {
						t.Errorf("step %s command %q", s.Key, c)
					}
				}
			}
		})
	}
}

// The steps a healthy pod passes: the api pod is fine apart from the
// NetworkPolicy, so status, events, logs, exit code, node and Service are ok.
func TestHealthySteps(t *testing.T) {
	d := walk(t, demo(t), "api")
	for _, key := range []string{protocol.StepStatus, protocol.StepEvents, protocol.StepLogs, protocol.StepExitCode, protocol.StepNode, protocol.StepService} {
		if s := stepOf(t, d, key); s.Verdict != protocol.VerdictOK {
			t.Errorf("%s = %s: %s %v", key, s.Verdict, s.Finding, s.Evidence)
		}
	}
	// No metrics in the fixture: resources is unknown, not ok.
	if s := stepOf(t, d, protocol.StepResources); s.Verdict != protocol.VerdictUnknown {
		t.Errorf("resources = %s", s.Verdict)
	}
	if s := stepOf(t, d, protocol.StepNode); !strings.Contains(s.Finding, "Ready with no memory, disk or PID pressure") {
		t.Errorf("node = %s", s.Finding)
	}
}

func minimal(p *corev1.Pod) Inputs {
	return Inputs{Pod: p, Diagnosis: diagnose.Analyse(diagnose.Inputs{Pod: p})}
}

func TestNodeSteps(t *testing.T) {
	src := demo(t)
	var node *corev1.Node
	for _, o := range src[nodesGVR] {
		node = o.(*corev1.Node).DeepCopy()
	}
	p := &corev1.Pod{}
	p.Namespace, p.Name, p.Spec.NodeName = "shop", "p", node.Name
	p.Status.Phase = corev1.PodRunning
	// Under disk pressure.
	for i, c := range node.Status.Conditions {
		if c.Type == corev1.NodeDiskPressure {
			node.Status.Conditions[i].Status, node.Status.Conditions[i].Message = corev1.ConditionTrue, "kubelet has disk pressure"
		}
	}
	node.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/disk-pressure", Effect: corev1.TaintEffectNoSchedule}}
	in := minimal(p)
	in.Nodes = []*corev1.Node{node}
	d := Walk(in)
	if s := stepOf(t, d, protocol.StepNode); s.Verdict != protocol.VerdictProblem || !strings.Contains(s.Finding, "DiskPressure") ||
		!strings.Contains(strings.Join(s.Evidence, " "), "taint node.kubernetes.io/disk-pressure") {
		t.Errorf("node = %+v", s)
	}
	// The node is not in the cache.
	in.Nodes = nil
	if s := stepOf(t, Walk(in), protocol.StepNode); s.Verdict != protocol.VerdictUnknown {
		t.Errorf("no node = %+v", s)
	}
	// A pending pod that would fit an empty node, but the node is full.
	big := &corev1.Pod{}
	big.Namespace, big.Name = "shop", "hog"
	big.Spec.NodeName = node.Name
	big.Status.Phase = corev1.PodRunning
	big.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3900m")}}}}
	pend := &corev1.Pod{}
	pend.Namespace, pend.Name = "shop", "wants-1"
	pend.Status.Phase = corev1.PodPending
	pend.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}}
	in = minimal(pend)
	in.Nodes, in.Pods = []*corev1.Node{node}, []*corev1.Pod{big, pend}
	if s := stepOf(t, Walk(in), protocol.StepNode); s.Verdict != protocol.VerdictProblem || !strings.HasPrefix(s.Finding, "Every node is full") {
		t.Errorf("full node = %+v", s)
	}
	// It fits by CPU: then something else keeps it out.
	in.Pods = []*corev1.Pod{pend}
	if s := stepOf(t, Walk(in), protocol.StepNode); !strings.Contains(s.Finding, "check taints, affinity") {
		t.Errorf("fits = %+v", s)
	}
	in.Nodes = nil
	if s := stepOf(t, Walk(in), protocol.StepNode); s.Verdict != protocol.VerdictUnknown {
		t.Errorf("no nodes = %+v", s)
	}
}

func TestStatusVariants(t *testing.T) {
	p := &corev1.Pod{}
	p.Namespace, p.Name = "shop", "p"
	p.Spec.Containers = []corev1.Container{{Name: "c"}}
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if s := stepOf(t, Walk(minimal(p)), protocol.StepStatus); s.Verdict != protocol.VerdictProblem || !strings.Contains(s.Finding, "not ready") {
		t.Errorf("not ready = %+v", s)
	}
	p.Status.Phase = corev1.PodSucceeded
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}
	d := Walk(minimal(p))
	if s := stepOf(t, d, protocol.StepStatus); s.Verdict != protocol.VerdictOK {
		t.Errorf("succeeded = %+v", s)
	}
	if s := stepOf(t, d, protocol.StepExitCode); s.Verdict != protocol.VerdictOK || !strings.Contains(s.Finding, "normally") {
		t.Errorf("exit 0 = %+v", s)
	}
	if d.Cause != "" && d.CauseStep != protocol.StepResources && d.CauseStep != protocol.StepService && d.CauseStep != protocol.StepNetpol {
		t.Errorf("cause = %q", d.Cause)
	}
}

func TestResourceUsageSteps(t *testing.T) {
	p := &corev1.Pod{}
	p.Namespace, p.Name = "shop", "p"
	p.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi"), corev1.ResourceCPU: resource.MustParse("100m")}}}}
	p.Status.Phase = corev1.PodRunning
	for _, tc := range []struct {
		usage   diagnose.Usage
		verdict string
		has     string
	}{
		{diagnose.Usage{CPUMillis: 5, MemoryBytes: 20 << 20}, protocol.VerdictOK, "within the limits"},
		{diagnose.Usage{CPUMillis: 5, MemoryBytes: 95 << 20}, protocol.VerdictProblem, "Near the memory limit"},
		{diagnose.Usage{CPUMillis: 99, MemoryBytes: 20 << 20}, protocol.VerdictProblem, "At the CPU limit"},
	} {
		in := Inputs{Pod: p, Diagnosis: diagnose.Analyse(diagnose.Inputs{Pod: p, Usage: map[string]diagnose.Usage{"c": tc.usage}})}
		s := stepOf(t, Walk(in), protocol.StepResources)
		if s.Verdict != tc.verdict || !strings.Contains(s.Finding, tc.has) || !strings.Contains(strings.Join(s.Evidence, " "), "usage") {
			t.Errorf("%+v: %+v", tc.usage, s)
		}
	}
}
