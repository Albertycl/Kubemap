package diagnose

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemap/pkg/protocol"
)

const (
	critical = "critical"
	warning  = "warning"
	info     = "info"
)

// findings turns the collected facts into conclusions, most severe first.
func findings(d *protocol.PodDiagnosis, p *corev1.Pod) []protocol.DiagFinding {
	var out []protocol.DiagFinding
	add := func(sev, container, title, detail string) {
		out = append(out, protocol.DiagFinding{Severity: sev, Container: container, Title: title, Detail: detail})
	}

	if title, msg := disruption(p); title != "" {
		detail := msg
		if title == "Evicted" {
			detail += fmt.Sprintf(" The kubelet evicts pods when node %s runs low on memory or disk; the controller creates a replacement, "+
				"which may land in the same trouble. Check the node's pressure conditions.", nodeName(p.Spec.NodeName))
		}
		add(critical, "", title, strings.TrimSpace(detail))
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			add(critical, "", "Not scheduled: "+c.Reason, strings.TrimSpace(c.Message)+
				" The scheduler found no node with enough free resources, matching affinity or tolerated taints.")
		}
	}

	for _, dc := range d.Containers {
		out = append(out, containerFindings(dc, p, d.Events)...)
	}

	if n := eventCount(d.Events, "Unhealthy", "Liveness probe failed"); n > 0 {
		add(warning, "", "Liveness probe failing", fmt.Sprintf("%d liveness failures in recent events: the kubelet restarts the container "+
			"after the failure threshold (exit 137 or 143). Check the probe's path, port and timeout against the app's startup time.", n))
	}
	if n := eventCount(d.Events, "Unhealthy", "Readiness probe failed"); n > 0 {
		add(warning, "", "Readiness probe failing", fmt.Sprintf("%d readiness failures in recent events: the pod is taken out of its "+
			"Services' endpoints while it fails, so clients may get 503.", n))
	}
	if n := eventCount(d.Events, "FailedMount", ""); n > 0 {
		add(critical, "", "Volume mount failing", fmt.Sprintf("%d FailedMount events: %s", n, firstMessage(d.Events, "FailedMount")))
	}

	slices.SortStableFunc(out, func(a, b protocol.DiagFinding) int { return rank(a.Severity) - rank(b.Severity) })
	if len(out) == 0 {
		out = append(out, protocol.DiagFinding{Severity: info, Title: "No problem found",
			Detail: "The containers run, are ready and have not restarted; no warning events."})
	}
	return out
}

func containerFindings(dc protocol.DiagContainer, p *corev1.Pod, evs []protocol.DiagEvent) []protocol.DiagFinding {
	var out []protocol.DiagFinding
	add := func(sev, title, detail string) {
		out = append(out, protocol.DiagFinding{Severity: sev, Container: dc.Name, Title: title, Detail: detail})
	}
	var cs corev1.ContainerStatus
	for _, s := range append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...) {
		if s.Name == dc.Name {
			cs = s
		}
	}
	memLimit := dc.Limits["memory"]

	if w := cs.State.Waiting; w != nil {
		msg := strings.TrimSpace(w.Message)
		switch w.Reason {
		case "CrashLoopBackOff":
			detail := fmt.Sprintf("The container crashed %d times and the kubelet waits longer before each restart.", dc.Restarts)
			if dc.Last != nil {
				detail += " Last run: " + dc.Last.Meaning + "."
			}
			if dc.LogsFrom == "previous" {
				detail += " The previous run's last log lines are below."
			}
			add(critical, "CrashLoopBackOff", detail)
		case "ErrImageNeverPull":
			add(critical, "Image not on the node", fmt.Sprintf("%s The image %s must already be on node %s because imagePullPolicy is Never. "+
				"A locally built or imported image is lost when the kubelet's image garbage collection runs (for example under disk pressure) "+
				"or when the pod moves to another node. Import the image on the node again, or push it to a registry and use IfNotPresent.",
				msg, dc.Image, nodeName(p.Spec.NodeName)))
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			add(critical, "Image cannot be pulled", fmt.Sprintf("%s Check the image name and tag, the registry's reachability from the node, and imagePullSecrets.", msg))
		case "CreateContainerConfigError":
			add(critical, "Configuration missing", msg+" A ConfigMap or Secret (or a key in it) that the container references does not exist.")
		case "CreateContainerError", "RunContainerError", "StartError":
			add(critical, "Container cannot start", msg)
		case "ContainerCreating", "PodInitializing", "":
		default:
			add(warning, "Waiting: "+w.Reason, msg)
		}
	}

	if l := dc.Last; l != nil && (dc.Restarts > 0 || cs.State.Terminated != nil) {
		switch {
		case l.ExitCode == 137 && probeKilled(evs, dc.Name, l, grace(p)) != "":
			probe := probeKilled(evs, dc.Name, l, grace(p))
			add(critical, "Killed by the "+probe+" probe (exit 137)", "The events show the kubelet killing this container for a "+
				"failed "+probe+" probe, just before it ended; it did not stop within its grace period, so it got SIGKILL. Check the "+
				"probe's path, port and timeout against how long the app takes to start and to answer, and the app's own health.")
		case l.Reason == "OOMKilled" || l.ExitCode == 137:
			add(critical, "Killed for memory (exit 137)", memoryKill(dc, l, memLimit))
		case l.ExitCode != 0 && l.ExitCode != 143 && dc.Restarts > 0 && cs.State.Waiting == nil:
			add(warning, fmt.Sprintf("Restarted %d times", dc.Restarts), "Last run ended: "+l.Meaning+".")
		case l.ExitCode == 143 && dc.Restarts > 0:
			add(warning, fmt.Sprintf("Restarted %d times", dc.Restarts), "Last run ended with SIGTERM (143): it was asked to stop, "+
				"usually by a failing liveness probe.")
		}
	}

	if u := dc.Usage; u != nil {
		if u.MemOfLimit >= 90 {
			add(warning, "Near the memory limit", fmt.Sprintf("%s used, %.0f%% of the %s limit: the next spike gets it killed for memory.",
				bytes(u.MemoryBytes), u.MemOfLimit, memLimit))
		}
		if u.CPUOfLimit >= 90 {
			add(warning, "At the CPU limit", fmt.Sprintf("%dm used, %.0f%% of the %s limit: the container is being throttled, "+
				"which slows responses and can fail probes with short timeouts.", u.CPUMillis, u.CPUOfLimit, dc.Limits["cpu"]))
		}
	}
	if memLimit == "" && !dc.Init {
		add(info, "No memory limit", "Without a limit the container can use the node's memory until the node is under pressure, "+
			"and then it is among the first to be evicted or OOM-killed.")
	}
	return out
}

// disruption says why the pod was stopped from outside: the Evicted
// status reason (older Kubernetes, or a pod that failed) or the
// DisruptionTarget condition (1.26+, set also when the container exits
// cleanly and the pod ends Succeeded).
func disruption(p *corev1.Pod) (title, message string) {
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.DisruptionTarget || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case "TerminationByKubelet":
			return "Evicted", c.Message
		case "PreemptionByScheduler":
			return "Preempted", "The scheduler removed this pod to make room for a pod with a higher priority. " + c.Message
		case "EvictionByEvictionAPI":
			return "Evicted by the Eviction API", "The pod was evicted through the API, usually by kubectl drain or an autoscaler. " + c.Message
		case "DeletionByTaintManager":
			return "Deleted for a NoExecute taint", c.Message
		case "DeletionByPodGC":
			return "Deleted by pod garbage collection", "Its node no longer exists. " + c.Message
		}
		return "Disrupted: " + c.Reason, c.Message
	}
	if p.Status.Reason == "Evicted" {
		return "Evicted", p.Status.Message
	}
	return "", ""
}

// memoryKill explains an exit 137. The reason field is not reliable: on
// k3s with containerd 2.2 (the lab node) a memory cgroup OOM kill is
// reported as "Error, exit 137", not "OOMKilled" (KM-71 F1). Without other
// evidence (a liveness kill in the events), SIGKILL means the OOM killer.
func memoryKill(dc protocol.DiagContainer, l *protocol.DiagTermination, memLimit string) string {
	var b strings.Builder
	if memLimit != "" {
		fmt.Fprintf(&b, "The container was killed with SIGKILL (exit 137), almost certainly by the kernel's OOM killer when it reached its %s memory limit.", memLimit)
	} else {
		b.WriteString("The container was killed with SIGKILL (exit 137). It has no memory limit, so the likely cause is the node running out of memory (the kernel's OOM killer), or a forced stop.")
	}
	if l.Reason == "OOMKilled" {
		b.WriteString(" The runtime confirms it: reason OOMKilled.")
	} else {
		fmt.Fprintf(&b, " The runtime reports reason %q; some runtimes (k3s with containerd 2.2 among them) do not set OOMKilled, so the exit code is the evidence. The node's kernel log proves it: journalctl -k | grep -i 'out of memory'.", cmpOr(l.Reason, "none"))
	}
	if dc.Usage != nil && dc.Usage.MemOfLimit > 0 {
		fmt.Fprintf(&b, " It uses %s now, %.0f%% of the limit.", bytes(dc.Usage.MemoryBytes), dc.Usage.MemOfLimit)
	}
	b.WriteString(" Raise the limit if the app needs the memory, or look for a leak or an unbounded cache.")
	return b.String()
}

// probeKilled returns "liveness" or "startup" when an event says the
// kubelet killed this container for that failed probe shortly before the
// last run ended: within the grace period plus a margin. An older kill (events
// live about an hour) does not explain this run. "" otherwise.
func probeKilled(evs []protocol.DiagEvent, container string, l *protocol.DiagTermination, grace time.Duration) string {
	for _, e := range evs {
		if e.Reason != "Killing" || (strings.Contains(e.Message, "Container ") && !strings.Contains(e.Message, "Container "+container+" ")) {
			continue
		}
		probe := ""
		switch {
		case strings.Contains(e.Message, "failed liveness probe"):
			probe = "liveness"
		case strings.Contains(e.Message, "failed startup probe"):
			probe = "startup"
		default:
			continue
		}
		if !l.Finished.IsZero() && !e.Last.IsZero() {
			if d := l.Finished.Sub(e.Last); d < -5*time.Second || d > grace+10*time.Second {
				continue // a kill at another time
			}
		}
		return probe
	}
	return ""
}

// grace is the pod's termination grace period (30 s by default).
func grace(p *corev1.Pod) time.Duration {
	if g := p.Spec.TerminationGracePeriodSeconds; g != nil {
		return time.Duration(*g) * time.Second
	}
	return 30 * time.Second
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func rank(sev string) int {
	switch sev {
	case critical:
		return 0
	case warning:
		return 1
	}
	return 2
}

func firstMessage(evs []protocol.DiagEvent, reason string) string {
	for _, e := range evs {
		if e.Reason == reason {
			return e.Message
		}
	}
	return ""
}

func nodeName(node string) string {
	if node == "" {
		return "(not scheduled)"
	}
	return node
}

func bytes(b int64) string {
	const mi = 1 << 20
	if b >= mi {
		return fmt.Sprintf("%dMi", (b+mi/2)/mi)
	}
	return fmt.Sprintf("%dKi", b>>10)
}
