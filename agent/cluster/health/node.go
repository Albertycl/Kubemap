package health

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"kubemap/agent/cluster/pressure"
	"kubemap/pkg/protocol"
)

// Node rule IDs.
const (
	RuleNodeNotReady    = "node.not-ready"
	RuleDiskEviction    = "node.disk.eviction"
	RuleImageFSEviction = "node.imagefs.eviction"
	RuleImageGC         = "node.image-gc"
	RuleInodes          = "node.inodes"
	RuleMemory          = "node.memory"
	RuleSwap            = "node.swap"
	RulePressureCPU     = "node.pressure.cpu"
	RulePressureMemory  = "node.pressure.memory"
	RulePressureIO      = "node.pressure.io"
	RulePIDs            = "node.pids"
)

// Margins above an eviction threshold, in percentage points of the disk:
// warn at threshold + DiskWarnMargin, critical at threshold + DiskCritMargin.
// On k8server (5 %) that is 15 % and 10 % free; the build rule stops at 18 %.
const (
	DiskWarnMargin  = 10.0
	DiskCritMargin  = 5.0
	InodeWarnMargin = 5.0
	InodeCritMargin = 2.0
	SwapWarn        = 90.0
	SwapCrit        = 98.0
	MemoryWarnShare = 10.0 // percent of capacity available
	// SwapCritMemShare: full swap is critical only when less than this
	// share of RAM is available too; otherwise the kernel just moved idle
	// pages out.
	SwapCritMemShare = 20.0
)

func nodeFindings(n *corev1.Node, m *protocol.Metrics, in Inputs) []protocol.HealthFinding {
	obj := "Node/" + n.Name
	var out []protocol.HealthFinding
	cond := func(t corev1.NodeConditionType) *corev1.NodeCondition {
		for i := range n.Status.Conditions {
			if n.Status.Conditions[i].Type == t {
				return &n.Status.Conditions[i]
			}
		}
		return nil
	}
	if c := cond(corev1.NodeReady); c == nil || c.Status != corev1.ConditionTrue {
		status, msg := "missing", ""
		if c != nil {
			status, msg = string(c.Status), c.Message
		}
		out = append(out, protocol.HealthFinding{Rule: RuleNodeNotReady, Severity: protocol.SeverityCritical, Object: obj,
			Title:    "Node not Ready",
			Evidence: fmt.Sprintf("Ready is %s: %s", status, msg),
			Meaning:  "The kubelet on this node stopped reporting, or reports that it cannot run pods.",
			Why:      "After about 5 minutes Kubernetes evicts the node's pods and starts them elsewhere; on a single-node cluster nothing can run.",
			Fix:      "On the node: check the kubelet or k3s service (systemctl status k3s; journalctl -u k3s --since -15min), disk, memory and the network to the API server.",
			Verify:   fmt.Sprintf("kubectl get node %s   # STATUS Ready", n.Name),
		})
	}
	diskPressure := cond(corev1.NodeDiskPressure)
	local := localOnlyPods(in.Pods, n.Name)
	if m != nil {
		nodefs := MountFor(m.Filesystems, KubeletDir)
		imagefs := ImageFS(m.Filesystems)
		if nodefs != nil {
			shared := imagefs == nodefs
			out = append(out, diskRule(RuleDiskEviction, obj, n.Name, "root filesystem (nodefs, "+nodefs.MountPoint+")", nodefs, SignalNodeFS, shared, diskPressure, in.Thresholds, local)...)
			out = append(out, inodeRule(obj, n.Name, nodefs, SignalNodeFSInodes, in.Thresholds, local)...)
		}
		if imagefs != nil {
			out = append(out, imageGCRule(obj, n.Name, imagefs, in)...)
			if imagefs != nodefs {
				out = append(out, diskRule(RuleImageFSEviction, obj, n.Name, "image filesystem (imagefs, "+imagefs.MountPoint+")", imagefs, SignalImageFS, false, diskPressure, in.Thresholds, local)...)
			}
		}
		out = append(out, memoryRule(obj, n, m.Memory, cond(corev1.NodeMemoryPressure), in.Thresholds)...)
		out = append(out, swapRule(obj, m.Swap, m.Memory)...)
	} else if c := diskPressure; c != nil && c.Status == corev1.ConditionTrue {
		out = append(out, diskPressureOnly(obj, n.Name, c, local))
	}
	out = append(out, pidRule(obj, n.Name, cond(corev1.NodePIDPressure))...)
	out = append(out, pressureRules(obj, n, m)...)
	return out
}

// Where the kubelet and the container runtime keep their data. The node
// agent reports every mounted filesystem; the one that holds the directory
// is the longest mount point that is a prefix of it.
const KubeletDir = "/var/lib/kubelet"

var imageDirs = []string{"/var/lib/rancher", "/var/lib/containerd", "/var/lib/docker"}

// MountFor returns the filesystem that holds path, or nil.
func MountFor(fss []protocol.FilesystemStats, path string) *protocol.FilesystemStats {
	var best *protocol.FilesystemStats
	for i := range fss {
		mp := fss[i].MountPoint
		if mp == path || mp == "/" || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/") {
			if best == nil || len(mp) > len(best.MountPoint) {
				best = &fss[i]
			}
		}
	}
	return best
}

// ImageFS is the filesystem of the container runtime's images: the most
// specific mount among the usual image directories (k3s, containerd,
// Docker); the root filesystem when none has its own.
func ImageFS(fss []protocol.FilesystemStats) *protocol.FilesystemStats {
	var best *protocol.FilesystemStats
	for _, d := range imageDirs {
		if f := MountFor(fss, d); f != nil && (best == nil || len(f.MountPoint) > len(best.MountPoint)) {
			best = f
		}
	}
	return best
}

// diskRule is the lesson of the 2026-09-28 incident: warn before free space
// reaches the eviction threshold, and say how much the kubelet will insist
// on freeing (threshold + minimum reclaim) once it starts.
func diskRule(rule, obj, node, what string, fs *protocol.FilesystemStats, signal string, shared bool, dp *corev1.NodeCondition, t Thresholds, local []string) []protocol.HealthFinding {
	capacity, avail := fs.TotalBytes, fs.AvailableBytes
	if capacity == 0 {
		return nil
	}
	th, ok := t.EvictionHard[signal]
	if !ok {
		return nil
	}
	freePct := pct(float64(avail), float64(capacity))
	evictPct := th.PercentOf(capacity)
	reclaim := t.MinReclaim[signal]
	targetPct := evictPct + reclaim.PercentOf(capacity)
	targetBytes := float64(capacity) * targetPct / 100
	evicting := dp != nil && dp.Status == corev1.ConditionTrue
	sev := ""
	switch {
	case evicting || freePct < evictPct+DiskCritMargin:
		sev = protocol.SeverityCritical
	case freePct < evictPct+DiskWarnMargin:
		sev = protocol.SeverityWarning
	default:
		return nil
	}
	target := fmt.Sprintf("%.0f%% free (%s)", targetPct, gib(targetBytes))
	if reclaim.Raw == "" {
		target = fmt.Sprintf("%.0f%% free (%s; no minimum reclaim set)", targetPct, gib(targetBytes))
	}
	ev := fmt.Sprintf("%s: %s free of %s (%.1f%%). The kubelet evicts pods below %.0f%% (%s<%s)", what,
		gib(float64(avail)), gib(float64(capacity)), freePct, evictPct, signal, th.Raw)
	if reclaim.Raw != "" {
		ev += fmt.Sprintf(" and then keeps evicting until %s (minimum reclaim %s)", target, reclaim.Raw)
	}
	ev += "."
	if shared {
		ev += " Container images are on the same filesystem."
	}
	title := fmt.Sprintf("Disk %.1f%% free: pods are evicted at %.0f%%", freePct, evictPct)
	// Aim for the warning line, or the reclaim target if that is higher.
	safePct := max(targetPct, evictPct+DiskWarnMargin)
	toFree := float64(capacity)*safePct/100 - float64(avail)
	fix := fmt.Sprintf("Free at least %s now to get above %.0f%% free. ", gib(max(toFree, 0)), safePct)
	if evicting {
		title = fmt.Sprintf("DiskPressure: the kubelet is evicting pods (%.1f%% free)", freePct)
		ev += fmt.Sprintf(" DiskPressure is True since %s.", dp.LastTransitionTime.UTC().Format("2006-01-02 15:04 MST"))
		fix = fmt.Sprintf("Eviction continues until %s: free at least %s now to get above %.0f%% free. ", target, gib(max(toFree, 0)), safePct)
	} else {
		ev += fmt.Sprintf(" Eviction starts after %s more is used.", gib(float64(avail)-float64(capacity)*evictPct/100))
	}
	fix += freeSpaceAdvice(local) + " For good: keep build caches off this disk, or give /var/lib/rancher (or /var/lib/kubelet and the container runtime) its own disk."
	why := fmt.Sprintf("At the threshold the kubelet sets DiskPressure, evicts pods (BestEffort first) and deletes unused images, and it does not stop at the threshold: it frees until %s.", target)
	if reclaim.Raw == "" {
		why = "At the threshold the kubelet sets DiskPressure, evicts pods (BestEffort first) and deletes unused images until it is above the threshold again (no minimum reclaim is set, so it may flap)."
	}
	why += " On 2026-09-28 this stopped every app on k8server for about 40 minutes, and image clean-up deleted an image that existed only on the node."
	mount := "/"
	if signal == SignalImageFS {
		mount = "<image filesystem, for example /var/lib/rancher>"
	}
	return []protocol.HealthFinding{{Rule: rule, Severity: sev, Object: obj, Title: title, Evidence: ev,
		Meaning: "The disk that holds container logs, emptyDir volumes and writable layers (and on k3s also the images) is close to the kubelet's hard eviction threshold.",
		Why:     why,
		Fix:     fix,
		Verify:  fmt.Sprintf("df -h %s   # above %.0f%% free\nkubectl describe node %s | grep DiskPressure   # False", mount, safePct, node),
	}}
}

// diskPressureOnly is DiskPressure without a summary to measure.
func diskPressureOnly(obj, node string, c *corev1.NodeCondition, local []string) protocol.HealthFinding {
	return protocol.HealthFinding{Rule: RuleDiskEviction, Severity: protocol.SeverityCritical, Object: obj,
		Title:    "DiskPressure: the kubelet is evicting pods",
		Evidence: fmt.Sprintf("DiskPressure is True since %s (%s). There is no fresh node agent report, so free space is unknown.", c.LastTransitionTime.UTC().Format("2006-01-02 15:04 MST"), c.Message),
		Meaning:  "A disk of the node is below the kubelet's hard eviction threshold.",
		Why:      "The kubelet evicts pods and deletes unused images until the minimum-reclaim target is free.",
		Fix:      "Free space on the node. " + freeSpaceAdvice(local),
		Verify:   fmt.Sprintf("df -h /\nkubectl describe node %s | grep DiskPressure   # False", node),
	}
}

// imageGCRule compares the image filesystem with imageGCHighThresholdPercent.
func imageGCRule(obj, node string, img *protocol.FilesystemStats, in Inputs) []protocol.HealthFinding {
	capacity, avail := img.TotalBytes, img.AvailableBytes
	if capacity == 0 || in.Thresholds.ImageGCHigh == 0 {
		return nil
	}
	used := pct(float64(capacity-avail), float64(capacity))
	high := float64(in.Thresholds.ImageGCHigh)
	if used < high-5 {
		return nil
	}
	sev := protocol.SeverityInfo
	title := fmt.Sprintf("Image clean-up starts at %.0f%% disk use (now %.1f%%)", high, used)
	if used >= high {
		sev = protocol.SeverityWarning
		title = fmt.Sprintf("Image clean-up is running: disk %.1f%% used, threshold %.0f%%", used, high)
	}
	local := localOnlyPods(in.Pods, node)
	ev := fmt.Sprintf("Image filesystem %.1f%% used; the kubelet deletes unused images from %.0f%% (imageGCHighThresholdPercent) until it is under the low threshold.", used, high)
	if len(local) > 0 {
		sev = protocol.SeverityWarning
		ev += fmt.Sprintf(" %s on this node use images that exist only here (imagePullPolicy: Never): %s.", plural(len(local), "pod"), joinMax(local, 5))
	}
	return []protocol.HealthFinding{{Rule: RuleImageGC, Severity: sev, Object: obj, Title: title, Evidence: ev,
		Meaning: "The kubelet removes images that no running container uses when the image disk is this full.",
		Why:     "An image that is not in a registry cannot be pulled again: its pods fail with ErrImageNeverPull after their next restart (url-shortener on 2026-09-28). Pulling deleted images again also slows every restart.",
		Fix:     "Push local images to a registry and use imagePullPolicy IfNotPresent; free disk space (see the disk finding) so usage stays under the threshold.",
		Verify:  fmt.Sprintf("df -h /var/lib/rancher   # under %.0f%% used\nkubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.containers[*].imagePullPolicy}{\"\\n\"}{end}' | grep Never   # nothing", high),
	}}
}

func inodeRule(obj, node string, fs *protocol.FilesystemStats, signal string, t Thresholds, local []string) []protocol.HealthFinding {
	total, free := fs.InodesTotal, fs.InodesFree
	th, ok := t.EvictionHard[signal]
	if total == 0 || !ok {
		return nil
	}
	freePct := pct(float64(free), float64(total))
	evictPct := th.PercentOf(total)
	sev := ""
	switch {
	case freePct < evictPct+InodeCritMargin:
		sev = protocol.SeverityCritical
	case freePct < evictPct+InodeWarnMargin:
		sev = protocol.SeverityWarning
	default:
		return nil
	}
	return []protocol.HealthFinding{{Rule: RuleInodes, Severity: sev, Object: obj,
		Title:    fmt.Sprintf("Inodes %.1f%% free: pods are evicted at %.0f%%", freePct, evictPct),
		Evidence: fmt.Sprintf("%d of %d inodes free (%.1f%%); %s<%s.", free, total, freePct, signal, th.Raw),
		Meaning:  "The filesystem is running out of file entries, even if bytes are left: usually millions of small files (caches, logs, node_modules, container layers).",
		Why:      "With no free inodes nothing can create a file: pods fail to start, logs stop, and the kubelet evicts pods as for low disk space.",
		Fix:      "Find the directories with the most files (sudo du --inodes -x / | sort -n | tail) and delete caches there. " + freeSpaceAdvice(local),
		Verify:   fmt.Sprintf("df -i /   # IUse%% under %.0f%%", 100-evictPct-InodeWarnMargin),
	}}
}

func memoryRule(obj string, n *corev1.Node, m protocol.MemoryStats, mp *corev1.NodeCondition, t Thresholds) []protocol.HealthFinding {
	capacity, avail := m.TotalBytes, m.AvailableBytes
	if capacity == 0 {
		return nil // the report has no memory figures
	}
	th := t.EvictionHard[SignalMemory]
	evictAt := th.Of(capacity)
	share := pct(float64(avail), float64(capacity))
	pressure := mp != nil && mp.Status == corev1.ConditionTrue
	sev := ""
	switch {
	case pressure || float64(avail) < 2*evictAt:
		sev = protocol.SeverityCritical
	case capacity > 0 && share < MemoryWarnShare:
		sev = protocol.SeverityWarning
	default:
		return nil
	}
	ev := fmt.Sprintf("%s available of %s (%.1f%%); the kubelet evicts pods below %s (memory.available<%s).", gib(float64(avail)), gib(float64(capacity)), share, gib(evictAt), th.Raw)
	if pressure {
		ev += " MemoryPressure is True."
	}
	return []protocol.HealthFinding{{Rule: RuleMemory, Severity: sev, Object: obj,
		Title:    fmt.Sprintf("Memory available %s (%.1f%%)", gib(float64(avail)), share),
		Evidence: ev,
		Meaning:  "Little memory is left for new work after the page cache that can be dropped.",
		Why:      "Near the threshold the kubelet evicts pods (BestEffort, then Burstable over their requests); before that, the kernel OOM killer may kill containers or system services.",
		Fix:      "Find the biggest users (kubectl top pods -A --sort-by=memory), set memory limits, scale down what is not needed, or add memory.",
		Verify:   fmt.Sprintf("kubectl describe node %s | grep MemoryPressure   # False\nfree -h   # on the node: 'available' above %.0f%%", n.Name, MemoryWarnShare),
	}}
}

func swapRule(obj string, s protocol.SwapStats, m protocol.MemoryStats) []protocol.HealthFinding {
	total, used := float64(s.TotalBytes), float64(s.UsedBytes)
	if total == 0 {
		return nil
	}
	avail := max(total-used, 0)
	share := pct(used, total)
	if share < SwapWarn {
		return nil
	}
	ev := fmt.Sprintf("%s of %s swap used, %s left.", gib(used), gib(total), gib(avail))
	sev := protocol.SeverityWarning
	if m.TotalBytes > 0 {
		memShare := pct(float64(m.AvailableBytes), float64(m.TotalBytes))
		ev += fmt.Sprintf(" RAM available: %s (%.0f%%).", gib(float64(m.AvailableBytes)), memShare)
		if share >= SwapCrit && memShare < SwapCritMemShare {
			sev = protocol.SeverityCritical
		}
	}
	return []protocol.HealthFinding{{Rule: RuleSwap, Severity: sev, Object: obj,
		Title:    fmt.Sprintf("Swap %.0f%% full", share),
		Evidence: ev,
		Meaning:  "Memory demand is larger than RAM: the kernel has moved that much memory to swap and has almost no room left.",
		Why:      "When swap is full and RAM runs short, the next memory spike has nowhere to go and the kernel OOM killer kills processes, pods or system services. Pages in swap also make the processes that own them slow when they touch them (see memory and I/O pressure). Kubernetes schedules as if swap did not exist, so it does not see this.",
		Fix:      "Find the biggest memory users (kubectl top pods -A --sort-by=memory, and top processes on the node), set memory limits on pods without them, stop what is not needed, or add RAM.",
		Verify:   "free -h   # on the node: swap 'used' well below 'total'\nswapon --show",
	}}
}

// pidRule reports PIDPressure. The node agent does not report process
// counts against the PID limit, so the condition is the only signal.
func pidRule(obj, node string, pp *corev1.NodeCondition) []protocol.HealthFinding {
	if pp == nil || pp.Status != corev1.ConditionTrue {
		return nil
	}
	return []protocol.HealthFinding{{Rule: RulePIDs, Severity: protocol.SeverityCritical, Object: obj,
		Title:    "PIDPressure: the node is running out of process IDs",
		Evidence: fmt.Sprintf("PIDPressure is True since %s: %s", pp.LastTransitionTime.UTC().Format("2006-01-02 15:04 MST"), pp.Message),
		Meaning:  "The node is close to its limit of processes and threads.",
		Why:      "When no PID is left, nothing can start a process or thread: containers fail to start, and the kubelet evicts pods.",
		Fix:      "Find the pod or process with the most threads (ps -eLf | awk '{print $2}' | sort | uniq -c | sort -n | tail, on the node) and fix the leak; set podPidsLimit in the kubelet.",
		Verify:   fmt.Sprintf("kubectl describe node %s | grep PIDPressure   # False", node),
	}}
}

// pressureRules turn KM-60 verdicts into findings for cpu, memory and io
// PSI. A condition that is True is left to the disk, memory and PID rules.
func pressureRules(obj string, n *corev1.Node, m *protocol.Metrics) []protocol.HealthFinding {
	np := pressure.Evaluate(n, m)
	var out []protocol.HealthFinding
	for _, r := range np.Resources {
		rule, ok := map[string]string{"cpu": RulePressureCPU, "memory": RulePressureMemory, "io": RulePressureIO}[r.Resource]
		if !ok || r.ConditionStatus == string(corev1.ConditionTrue) {
			continue
		}
		f := protocol.HealthFinding{Rule: rule, Object: obj, Evidence: r.Detail}
		switch r.Verdict {
		case protocol.VerdictPressure:
			f.Severity = protocol.SeverityWarning
		case protocol.VerdictElevated:
			f.Severity = protocol.SeverityInfo
		case protocol.VerdictUnknown:
			if r.Resource != "cpu" || r.PSI != nil {
				continue
			}
			out = append(out, protocol.HealthFinding{Rule: rule, Severity: protocol.SeverityInfo, Object: obj,
				Title: "CPU pressure cannot be seen", Evidence: r.Detail,
				Meaning: "The kubelet reports no pressure stall information (PSI), and Kubernetes has no CPU pressure condition.",
				Why:     "Pods may wait for CPU without any sign in Kubernetes.",
				Fix:     "Use a kernel with PSI (Linux 4.20+, boot option psi=1 where it is off) and Kubernetes 1.33 or newer.",
				Verify:  "cat /proc/pressure/cpu   # on the node",
			})
			continue
		default:
			continue
		}
		some := 0.0
		if r.PSI != nil {
			some = r.PSI.Some.Avg60
		}
		switch r.Resource {
		case "cpu":
			f.Title = fmt.Sprintf("CPU pressure: tasks wait for CPU %.0f%% of the time", some)
			f.Meaning = "Runnable tasks waited for a CPU: the node's CPUs are oversubscribed."
			f.Why = "Every pod on the node gets slower (latency, timeouts, failed probes), and Kubernetes shows no condition for it: CPU requests are a scheduling promise, not a guarantee."
			f.Fix = "Find the top CPU users (kubectl top pods -A --sort-by=cpu, and top processes on the node), limit or move them, lower build parallelism, or add CPUs."
			f.Verify = "cat /proc/pressure/cpu   # on the node: some avg60 under 10"
		case "memory":
			f.Title = fmt.Sprintf("Memory pressure: tasks stall on memory %.0f%% of the time", some)
			f.Meaning = "Tasks waited for memory: the kernel is reclaiming page cache or swapping to make room."
			f.Why = "Everything on the node slows down long before MemoryPressure turns True or the OOM killer acts."
			f.Fix = "Reduce memory use on the node (limits, fewer replicas, stop what is not needed) or add memory; check swap."
			f.Verify = "cat /proc/pressure/memory   # on the node: some avg60 under 10, full avg60 under 5"
		case "io":
			f.Title = fmt.Sprintf("I/O pressure: tasks wait for I/O %.0f%% of the time", some)
			f.Meaning = "Tasks waited for disk (or network storage) I/O."
			f.Why = "Slow disks make databases, logging and image pulls slow; heavy swapping also shows up here."
			f.Fix = "Find the processes doing the most I/O (sudo iotop -o on the node), move heavy jobs off this disk, and check swap."
			f.Verify = "cat /proc/pressure/io   # on the node: some avg60 under 10, full avg60 under 5"
		}
		out = append(out, f)
	}
	return out
}

// freeSpaceAdvice is the safe order for freeing disk space on a node. Image
// pruning comes last and is held back while pods use images that exist only
// on this node: crictl rmi --prune deletes every image no running container
// uses, including those of crash-looping, scaled-down or evicted pods, and an
// image that is in no registry cannot come back (url-shortener, 2026-09-28).
func freeSpaceAdvice(local []string) string {
	adv := ""
	if len(local) > 0 {
		adv = fmt.Sprintf("Do not prune images yet: %s use images that exist only on this node (imagePullPolicy: Never): %s. Push them to a registry or save them first (k3s ctr images export), and remove only named images. ",
			plural(len(local), "pod"), joinMax(local, 5))
	}
	adv += "In this order: (1) what is outside Kubernetes: build caches (go clean -cache, docker builder prune) and old logs (journalctl --vacuum-size=500M); " +
		"(2) named images you know a registry still has (crictl images, then crictl rmi <image>); " +
		"(3) only then crictl rmi --prune, which deletes every image that no running container uses, including the images of evicted, crash-looping or scaled-down workloads."
	return adv
}
