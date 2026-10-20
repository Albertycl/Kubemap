package health

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemap/pkg/protocol"
)

var now = time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)

// k8server's kubelet: evictionHard 5 %, minimum reclaim 10 % (incident doc).
func k3sThresholds(t *testing.T) Thresholds {
	th, err := Configure("nodefs.available<5%,imagefs.available<5%", "nodefs.available=10%,imagefs.available=10%", 0)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

const gb = uint64(1) << 30

func node(conds ...corev1.NodeCondition) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k8server"}}
	n.Status.Capacity = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("12Gi")}
	n.Status.Conditions = append([]corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, conds...)
	return n
}

func fs(capacity, avail uint64) protocol.FilesystemStats {
	return fsAt("/", capacity, avail)
}

func fsAt(mount string, capacity, avail uint64) protocol.FilesystemStats {
	return protocol.FilesystemStats{MountPoint: mount, TotalBytes: capacity, UsedBytes: capacity - avail, AvailableBytes: avail,
		InodesTotal: 6_000_000, InodesFree: 5_000_000}
}

// report is a healthy node agent report: disk 30 % free (images on the same
// disk), 4 GiB of 12 GiB RAM available, swap half used, calm PSI.
func report() *protocol.Metrics {
	calm := protocol.Pressure{Some: protocol.PressureAvg{Avg60: 1}, Full: &protocol.PressureAvg{}}
	return &protocol.Metrics{NodeName: "k8server", Time: now.Add(-10 * time.Second),
		Filesystems: []protocol.FilesystemStats{fs(100*gb, 30*gb), fsAt("/boot", 2*gb, gb)},
		Memory:      protocol.MemoryStats{TotalBytes: 12 * gb, AvailableBytes: 4 * gb},
		Swap:        protocol.SwapStats{TotalBytes: 4 * gb, UsedBytes: 2 * gb},
		PSI:         &protocol.PSIStats{CPU: calm, Memory: calm, IO: calm}}
}

func check(t *testing.T, n *corev1.Node, m *protocol.Metrics, th Thresholds, pods ...*corev1.Pod) *protocol.HealthReport {
	t.Helper()
	reports := map[string]*protocol.Metrics{}
	if m != nil {
		reports[n.Name] = m
	}
	return Check(Inputs{Now: now, Nodes: []*corev1.Node{n}, Reports: reports, Pods: pods, Thresholds: th})
}

func find(r *protocol.HealthReport, rule string) []protocol.HealthFinding {
	var out []protocol.HealthFinding
	for _, f := range r.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

// Every finding carries all KM-62 fields.
func complete(t *testing.T, r *protocol.HealthReport) {
	t.Helper()
	for _, f := range r.Findings {
		if f.Rule == "" || f.Severity == "" || f.Object == "" || f.Title == "" || f.Evidence == "" || f.Meaning == "" || f.Why == "" || f.Fix == "" || f.Verify == "" {
			t.Errorf("incomplete finding: %+v", f)
		}
	}
}

func TestHealthyNode(t *testing.T) {
	r := check(t, node(), report(), k3sThresholds(t))
	if len(r.Findings) != 0 {
		t.Errorf("findings on a healthy node: %+v", r.Findings)
	}
	if len(r.Rules) != len(Rules) || r.Thresholds.Source != "agent settings" || r.Thresholds.EvictionMinimumReclaim["nodefs.available"] != "10%" {
		t.Errorf("report = %+v", r)
	}
}

func TestThresholds(t *testing.T) {
	d := Defaults()
	if d.EvictionHard[SignalNodeFS].Percent != 10 || d.EvictionHard[SignalMemory].Bytes != 100<<20 || d.ImageGCHigh != 85 || len(d.MinReclaim) != 0 {
		t.Errorf("defaults = %+v", d)
	}
	th := k3sThresholds(t)
	if th.EvictionHard[SignalNodeFS].Percent != 5 || th.EvictionHard[SignalMemory].Raw != "100Mi" || th.MinReclaim[SignalNodeFS].Percent != 10 {
		t.Errorf("configured = %+v", th)
	}
	if v := (Value{Bytes: 5 * int64(gb)}); v.PercentOf(100*gb) != 5 || v.Of(100*gb) != float64(5*gb) {
		t.Errorf("bytes value: %v %v", v.PercentOf(100*gb), v.Of(100*gb))
	}
	for _, bad := range [][2]string{{"nodefs.available=5%", ""}, {"nodefs.available<150%", ""}, {"nodefs.available<abc", ""}, {"", "nodefs.available<10%"}} {
		if _, err := Configure(bad[0], bad[1], 0); err == nil {
			t.Errorf("%q %q must fail", bad[0], bad[1])
		}
	}
	if _, err := Configure("", "", 101); err == nil {
		t.Error("image GC 101 must fail")
	}
	if c, _ := Configure("", "", 0); c.Source != "kubelet defaults" {
		t.Errorf("empty settings: %s", c.Source)
	}
}

// The 2026-09-28 incident, step by step, on a 100 GiB disk.
func TestDiskIncident(t *testing.T) {
	th := k3sThresholds(t)
	dp := corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Date(2026, 9, 27, 23, 32, 0, 0, time.UTC))}
	for _, c := range []struct {
		name     string
		freePct  uint64
		pressure bool
		sev      string
		want     []string
	}{
		{"healthy", 23, false, "", nil},
		{"after a big build", 14, false, protocol.SeverityWarning, []string{"14.0% free", "evicts pods below 5%", "until 15% free (15.0 GiB)", "Eviction starts after 9.0 GiB"}},
		{"close", 9, false, protocol.SeverityCritical, []string{"Free at least 6.0 GiB now to get above 15% free"}},
		{"evicting", 4, true, protocol.SeverityCritical, []string{"DiskPressure is True since 2026-09-27 23:32 UTC", "Eviction continues until 15% free"}},
		// Recovered to 13.7 %, but the kubelet still evicts until 15 %.
		{"still evicting", 14, true, protocol.SeverityCritical, []string{"kubelet is evicting pods", "free at least 1.0 GiB"}},
	} {
		s := report()
		s.Filesystems[0] = fs(100*gb, c.freePct*gb)
		n := node()
		if c.pressure {
			n = node(dp)
		}
		r := check(t, n, s, th)
		got := find(r, RuleDiskEviction)
		if c.sev == "" {
			if len(got) != 0 {
				t.Errorf("%s: %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Severity != c.sev {
			t.Fatalf("%s: %+v", c.name, got)
		}
		text := got[0].Title + " " + got[0].Evidence + " " + got[0].Fix + " " + got[0].Why
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: missing %q in %s", c.name, w, text)
			}
		}
		if !strings.Contains(got[0].Evidence, "Container images are on the same filesystem") || len(find(r, RuleImageFSEviction)) != 0 {
			t.Errorf("%s: a shared imagefs must not be a second finding", c.name)
		}
		complete(t, r)
	}
}

// With kubelet defaults (10 %, no reclaim) the warning line is 20 % free.
func TestDiskDefaultsAndSeparateImageFS(t *testing.T) {
	s := report()
	s.Filesystems[0] = fs(100*gb, 18*gb)
	s.Filesystems = append(s.Filesystems, fsAt("/var/lib/rancher", 50*gb, 9*gb)) // 18 % free, imagefs threshold 15 %
	r := check(t, node(), s, Defaults())
	d := find(r, RuleDiskEviction)
	if len(d) != 1 || d[0].Severity != protocol.SeverityWarning || !strings.Contains(d[0].Evidence, "nodefs.available<10%") || strings.Contains(d[0].Evidence, "keeps evicting") {
		t.Errorf("nodefs = %+v", d)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "KUBEMAP_EVICTION_HARD") {
		t.Errorf("notes = %v", r.Notes)
	}
	im := find(r, RuleImageFSEviction)
	if len(im) != 1 || im[0].Severity != protocol.SeverityCritical || !strings.Contains(im[0].Evidence, "image filesystem") {
		t.Errorf("imagefs = %+v", im)
	}
}

func TestImageGC(t *testing.T) {
	local := pod("sre-lab-app", "url-shortener-1")
	local.Spec.Containers[0].ImagePullPolicy = corev1.PullNever
	s := report()
	s.Filesystems[0] = fs(100*gb, 17*gb) // 83 % used: approaching 85 %
	g := find(check(t, node(), s, Defaults()), RuleImageGC)
	if len(g) != 1 || g[0].Severity != protocol.SeverityInfo || !strings.Contains(g[0].Title, "starts at 85%") {
		t.Errorf("approaching: %+v", g)
	}
	r := check(t, node(), s, Defaults(), local)
	g = find(r, RuleImageGC)
	if len(g) != 1 || g[0].Severity != protocol.SeverityWarning || !strings.Contains(g[0].Evidence, "sre-lab-app/url-shortener-1") {
		t.Errorf("with a local-only image: %+v", g)
	}
	if l := find(r, RuleLocalImages); len(l) != 1 || l[0].Count != 1 {
		t.Errorf("local images: %+v", l)
	}
	s.Filesystems[0] = fs(100*gb, 10*gb)
	if g := find(check(t, node(), s, Defaults()), RuleImageGC); len(g) != 1 || !strings.Contains(g[0].Title, "is running") {
		t.Errorf("over: %+v", g)
	}
}

func TestInodesMemorySwapPIDs(t *testing.T) {
	s := report()
	s.Filesystems[0].InodesFree = 6_000_000 * 6 / 100 // 6 %: threshold 5 % + 2
	s.Memory.AvailableBytes = 1 * gb                  // 8 % of 12 GiB
	s.Swap = protocol.SwapStats{TotalBytes: 4 * gb, UsedBytes: 4*gb - 3<<20}
	r := check(t, node(), s, k3sThresholds(t))
	if f := find(r, RuleInodes); len(f) != 1 || f[0].Severity != protocol.SeverityCritical {
		t.Errorf("inodes = %+v", f)
	}
	if f := find(r, RuleMemory); len(f) != 1 || f[0].Severity != protocol.SeverityWarning || !strings.Contains(f[0].Evidence, "1.0 GiB available of 12.0 GiB") {
		t.Errorf("memory = %+v", f)
	}
	if f := find(r, RuleSwap); len(f) != 1 || f[0].Severity != protocol.SeverityCritical || !strings.Contains(f[0].Evidence, "3 MiB left") {
		t.Errorf("swap with little RAM = %+v", f)
	}
	if f := find(r, RulePIDs); len(f) != 0 {
		t.Errorf("pids without PIDPressure = %+v", f)
	}
	complete(t, r)

	// k8server now: swap full, but a third of RAM available: warning only.
	s = report()
	s.Swap = protocol.SwapStats{TotalBytes: 4 * gb, UsedBytes: 4*gb - 3<<20}
	if f := find(check(t, node(), s, k3sThresholds(t)), RuleSwap); len(f) != 1 || f[0].Severity != protocol.SeverityWarning || !strings.Contains(f[0].Evidence, "RAM available: 4.0 GiB (33%)") {
		t.Errorf("swap with RAM left = %+v", f)
	}
	s.Swap = protocol.SwapStats{}
	if f := find(check(t, node(), s, k3sThresholds(t)), RuleSwap); len(f) != 0 {
		t.Errorf("no swap: %+v", f)
	}
	mp := corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}
	pp := corev1.NodeCondition{Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue}
	r = check(t, node(mp, pp), report(), k3sThresholds(t))
	if f := find(r, RuleMemory); len(f) != 1 || f[0].Severity != protocol.SeverityCritical {
		t.Errorf("MemoryPressure = %+v", f)
	}
	if f := find(r, RulePIDs); len(f) != 1 || f[0].Severity != protocol.SeverityCritical {
		t.Errorf("PIDPressure = %+v", f)
	}
	if f := find(r, RulePressureMemory); len(f) != 0 {
		t.Errorf("a True condition is left to the memory rule: %+v", f)
	}
}

func TestPressureAndReadiness(t *testing.T) {
	s := report()
	s.PSI.CPU = protocol.Pressure{Some: protocol.PressureAvg{Avg60: 27.26}}
	s.PSI.IO = protocol.Pressure{Some: protocol.PressureAvg{Avg60: 12}, Full: &protocol.PressureAvg{}}
	r := check(t, node(), s, k3sThresholds(t))
	if f := find(r, RulePressureCPU); len(f) != 1 || f[0].Severity != protocol.SeverityWarning || !strings.Contains(f[0].Title, "27%") {
		t.Errorf("cpu = %+v", f)
	}
	if f := find(r, RulePressureIO); len(f) != 1 || f[0].Severity != protocol.SeverityInfo {
		t.Errorf("io = %+v", f)
	}
	complete(t, r)
	// No summary at all: CPU cannot be seen; DiskPressure still reported.
	notReady := node(corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	r = check(t, notReady, nil, k3sThresholds(t))
	if f := find(r, RulePressureCPU); len(f) != 1 || f[0].Title != "CPU pressure cannot be seen" {
		t.Errorf("no PSI = %+v", f)
	}
	if f := find(r, RuleNodeNotReady); len(f) != 1 || f[0].Severity != protocol.SeverityCritical {
		t.Errorf("not ready = %+v", f)
	}
	if f := find(r, RuleDiskEviction); len(f) != 1 || !strings.Contains(f[0].Evidence, "free space is unknown") {
		t.Errorf("DiskPressure without summary = %+v", f)
	}
	complete(t, r)
}

func pod(ns, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Spec: corev1.PodSpec{NodeName: "k8server", Containers: []corev1.Container{{Name: "app", Image: "app:1",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("24Mi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func crashing(p *corev1.Pod, code int32, reason string, finished time.Time) *corev1.Pod {
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Image: "app:1", RestartCount: 4,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, FinishedAt: metav1.NewTime(finished)}}}}
	return p
}

// The kubemap-demo scenarios as the cache holds them.
func TestPodRules(t *testing.T) {
	oom := crashing(pod("kubemap-demo", "oom-1"), 137, "Error", now.Add(-time.Minute))
	crash := crashing(pod("kubemap-demo", "checkout-1"), 1, "Error", now.Add(-time.Minute))
	oldKill := pod("shop", "old-1")
	oldKill.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-48 * time.Hour))}}}}
	pull := pod("sre-lab-app", "url-shortener-2")
	pull.Status.Phase = corev1.PodPending
	pull.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Image: "url-shortener:v6", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImageNeverPull", Message: `Container image "url-shortener:v6" is not present with pull policy of Never`}}}}
	pending := pod("kubemap-demo", "batch-worker-1")
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/1 nodes are available: 1 Insufficient cpu.", LastTransitionTime: metav1.NewTime(now.Add(-5 * time.Minute))}}
	justCreated := pod("kubemap-demo", "new-1")
	justCreated.CreationTimestamp = metav1.NewTime(now.Add(-30 * time.Second))
	justCreated.Status = pending.Status
	evicted := pod("argocd", "argocd-server-x")
	evicted.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node was low on resource: ephemeral-storage."}
	done := pod("batch", "job-1")
	done.Status.Phase = corev1.PodSucceeded
	done.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	var noLimit []*corev1.Pod
	for i := range 25 {
		p := pod(fmt.Sprintf("ns%d", i%3), fmt.Sprintf("web-%02d", i))
		p.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		noLimit = append(noLimit, p)
	}
	all := append([]*corev1.Pod{oom, crash, oldKill, pull, pending, justCreated, evicted, done}, noLimit...)
	r := check(t, node(), report(), k3sThresholds(t), all...)
	complete(t, r)

	k := find(r, RuleExit137)
	if len(k) != 1 || k[0].Object != "Pod/kubemap-demo/oom-1" || k[0].Severity != protocol.SeverityCritical ||
		!strings.Contains(k[0].Evidence, "memory limit 24Mi") || !strings.Contains(k[0].Evidence, "reports Error") || !strings.Contains(k[0].Title, "Killed for memory") {
		t.Errorf("exit 137 = %+v", k)
	}
	if c := find(r, RuleCrashLoop); len(c) != 1 || c[0].Object != "Pod/kubemap-demo/checkout-1" || !strings.Contains(c[0].Evidence, "last exit 1:") ||
		!strings.Contains(c[0].Fix, "kubectl logs -n kubemap-demo checkout-1 -c app --previous") {
		t.Errorf("crashloop = %+v", c)
	}
	if im := find(r, RuleImagePull); len(im) != 1 || !strings.Contains(im[0].Fix, "k3s ctr images import") {
		t.Errorf("image pull = %+v", im)
	}
	if p := find(r, RulePending); len(p) != 1 || p[0].Object != "Pod/kubemap-demo/batch-worker-1" || !strings.Contains(p[0].Title, "requests more") ||
		!strings.Contains(p[0].Evidence, "Pending for 5m0s") {
		t.Errorf("pending = %+v", p)
	}
	if e := find(r, RuleEvicted); len(e) != 1 || e[0].Count != 1 || e[0].Items[0] != "argocd/argocd-server-x" {
		t.Errorf("evicted = %+v", e)
	}
	nl := find(r, RuleNoMemoryLimit)
	if len(nl) != 1 || nl[0].Count != 25 || len(nl[0].Items) != protocol.MaxFindingItems || nl[0].Items[0] != "ns0/web-00 (app)" ||
		!strings.Contains(nl[0].Evidence, "3 namespaces (ns0, ns1, ns2)") {
		t.Errorf("no limit = %+v", nl)
	}
	// Order: critical first, then by rule order.
	if r.Findings[0].Severity != protocol.SeverityCritical || r.Findings[len(r.Findings)-1].Severity == protocol.SeverityCritical {
		t.Errorf("order: %v", r.Findings)
	}

	// Namespace narrows the pod rules only.
	r = Check(Inputs{Now: now, Nodes: []*corev1.Node{node()}, Pods: all, Namespace: "kubemap-demo", Thresholds: k3sThresholds(t)})
	for _, f := range r.Findings {
		if strings.HasPrefix(f.Object, "Pod/") && !strings.HasPrefix(f.Object, "Pod/kubemap-demo/") {
			t.Errorf("outside the namespace: %+v", f)
		}
	}
}

func TestFindingCap(t *testing.T) {
	var pods []*corev1.Pod
	for i := range protocol.MaxHealthFindings + 10 {
		pods = append(pods, crashing(pod("ns", fmt.Sprintf("p%03d", i)), 1, "Error", now))
	}
	r := check(t, node(), report(), k3sThresholds(t), pods...)
	if len(r.Findings) != protocol.MaxHealthFindings || !strings.Contains(strings.Join(r.Notes, " "), "10 more findings") {
		t.Errorf("%d findings, notes %v", len(r.Findings), r.Notes)
	}
}

// review-km-61 B1: never advise image pruning blindly while images exist
// only on the node, even for pods that are not running.
func TestFreeSpaceAdviceProtectsLocalImages(t *testing.T) {
	local := pod("sre-lab-app", "url-shortener-1")
	local.Spec.Containers[0].ImagePullPolicy = corev1.PullNever
	local.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, Conditions: []corev1.PodCondition{
		{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet"}}}
	s := report()
	s.Filesystems[0] = fs(100*gb, 9*gb)
	s.Filesystems[0].InodesFree = 6_000_000 * 6 / 100
	dp := corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}
	for name, r := range map[string]*protocol.HealthReport{
		"summary":    check(t, node(), s, k3sThresholds(t), local),
		"no summary": check(t, node(dp), nil, k3sThresholds(t), local),
	} {
		fixes := 0
		for _, f := range r.Findings {
			if f.Rule != RuleDiskEviction && f.Rule != RuleInodes {
				continue
			}
			fixes++
			warn := strings.Index(f.Fix, "Do not prune images yet")
			prune := strings.Index(f.Fix, "crictl rmi --prune")
			if warn < 0 || prune < warn || !strings.Contains(f.Fix, "sre-lab-app/url-shortener-1") || !strings.Contains(f.Fix, "k3s ctr images export") {
				t.Errorf("%s %s: fix = %s", name, f.Rule, f.Fix)
			}
			if strings.Index(f.Fix, "go clean -cache") > prune || strings.Index(f.Fix, "crictl rmi <image>") > prune {
				t.Errorf("%s %s: caches and named images must come before pruning: %s", name, f.Rule, f.Fix)
			}
		}
		if (name == "summary" && fixes != 2) || (name == "no summary" && fixes != 1) {
			t.Errorf("%s: %d disk/inode findings", name, fixes)
		}
	}
	// Without local-only images: no warning, but pruning still last, with its caveat.
	r := check(t, node(), s, k3sThresholds(t))
	for _, f := range find(r, RuleDiskEviction) {
		if strings.Contains(f.Fix, "Do not prune") || !strings.Contains(f.Fix, "only then crictl rmi --prune, which deletes every image") {
			t.Errorf("fix = %s", f.Fix)
		}
	}
}

// review-km-61 N-1: on Kubernetes 1.26+ an evicted pod may end Succeeded
// with DisruptionTarget/TerminationByKubelet; the fix deletes by name.
func TestEvictedOnNewKubernetes(t *testing.T) {
	var pods []*corev1.Pod
	for i, ns := range []string{"argocd", "argocd", "monitoring"} {
		p := pod(ns, fmt.Sprintf("x-%d", i))
		p.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, Conditions: []corev1.PodCondition{
			{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet", Message: "The node was low on resource: ephemeral-storage."}}}
		pods = append(pods, p)
	}
	job := pod("batch", "job-1")
	job.Status.Phase = corev1.PodFailed // a failed Job pod, not evicted
	preempted := pod("batch", "low-1")
	preempted.Status = corev1.PodStatus{Phase: corev1.PodFailed, Conditions: []corev1.PodCondition{
		{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "PreemptionByScheduler"}}}
	r := check(t, node(), report(), k3sThresholds(t), append(pods, job, preempted)...)
	e := find(r, RuleEvicted)
	if len(e) != 1 || e[0].Count != 3 {
		t.Fatalf("evicted = %+v", e)
	}
	if !strings.Contains(e[0].Fix, "kubectl delete pod -n argocd x-0 x-1; kubectl delete pod -n monitoring x-2") || strings.Contains(e[0].Fix, "-A --field-selector") {
		t.Errorf("fix = %s", e[0].Fix)
	}
	if e[0].Verify != "kubectl get pod -n argocd x-0 x-1; kubectl get pod -n monitoring x-2   # NotFound" {
		t.Errorf("verify = %s", e[0].Verify)
	}
	var many []string
	for i := range 15 {
		many = append(many, fmt.Sprintf("ns%d/p%02d", i%5, i))
	}
	if c := deleteCommands(many); !strings.Contains(c, "and the same for 2 more namespaces") || strings.Count(c, "kubectl delete") != 3 {
		t.Errorf("deleteCommands = %s", c)
	}
}

// The node agent reports every mount; the rules pick the right ones.
func TestMountSelection(t *testing.T) {
	root, boot, rancher, kubelet := fsAt("/", 100*gb, 30*gb), fsAt("/boot", 2*gb, gb), fsAt("/var/lib/rancher", 50*gb, 9*gb), fsAt("/var/lib/kubelet", 20*gb, 5*gb)
	if m := MountFor([]protocol.FilesystemStats{boot, root}, KubeletDir); m == nil || m.MountPoint != "/" {
		t.Errorf("nodefs on root: %+v", m)
	}
	if m := MountFor([]protocol.FilesystemStats{root, kubelet}, KubeletDir); m == nil || m.MountPoint != "/var/lib/kubelet" {
		t.Errorf("own kubelet disk: %+v", m)
	}
	if m := MountFor([]protocol.FilesystemStats{fsAt("/var/lib/kube", gb, gb)}, KubeletDir); m != nil {
		t.Errorf("/var/lib/kube does not hold /var/lib/kubelet: %+v", m)
	}
	if m := ImageFS([]protocol.FilesystemStats{root, rancher, boot}); m == nil || m.MountPoint != "/var/lib/rancher" {
		t.Errorf("imagefs = %+v", m)
	}
	if m := ImageFS([]protocol.FilesystemStats{root, boot}); m == nil || m.MountPoint != "/" {
		t.Errorf("imagefs on root = %+v", m)
	}
	// No filesystems in the report: no disk findings, no crash.
	s := report()
	s.Filesystems = nil
	if r := check(t, node(), s, k3sThresholds(t)); len(find(r, RuleDiskEviction))+len(find(r, RuleImageGC)) != 0 {
		t.Errorf("findings without filesystems: %+v", r.Findings)
	}
}
