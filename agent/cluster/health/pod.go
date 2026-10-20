package health

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemap/agent/cluster/diagnose"
	"kubemap/pkg/protocol"
)

// Pod rule IDs.
const (
	RuleCrashLoop     = "pod.crashloop"
	RuleExit137       = "pod.exit-137"
	RuleImagePull     = "pod.image-pull"
	RuleLocalImages   = "pod.image-local-only"
	RulePending       = "pod.pending"
	RuleEvicted       = "pod.evicted"
	RuleNoMemoryLimit = "pod.no-memory-limit"
)

// Exit137Window is how recent a kill with exit 137 must be to count.
const Exit137Window = 24 * time.Hour

// PendingAfter is how long a pod may wait for a node before it is a finding.
const PendingAfter = 2 * time.Minute

var imagePullReasons = []string{"ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName"}

func podFindings(pods []*corev1.Pod, in Inputs) []protocol.HealthFinding {
	var out []protocol.HealthFinding
	var evicted, noLimit, localOnly []string
	for _, p := range pods {
		obj := "Pod/" + p.Namespace + "/" + p.Name
		if evictedByKubelet(p) {
			evicted = append(evicted, p.Namespace+"/"+p.Name)
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, containerFindings(p, obj, in.Now)...)
		if f, ok := pendingFinding(p, obj, in.Now); ok {
			out = append(out, f)
		}
		var missing []string
		for _, c := range p.Spec.Containers {
			if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
				missing = append(missing, c.Name)
			}
		}
		if len(missing) > 0 {
			noLimit = append(noLimit, fmt.Sprintf("%s/%s (%s)", p.Namespace, p.Name, strings.Join(missing, ", ")))
		}
		if neverPull(p) {
			localOnly = append(localOnly, p.Namespace+"/"+p.Name)
		}
	}
	if len(localOnly) > 0 {
		out = append(out, group(protocol.HealthFinding{Rule: RuleLocalImages, Severity: protocol.SeverityWarning, Object: scope(in.Namespace),
			Title:    fmt.Sprintf("%s use images that exist only on the node", plural(len(localOnly), "pod")),
			Evidence: fmt.Sprintf("imagePullPolicy: Never on %s.", joinMax(localOnly, 5)),
			Meaning:  "These images were loaded onto the node by hand and are not pulled from a registry.",
			Why:      "When the kubelet cleans up images (disk over the image GC threshold, or disk pressure) or the pod moves to another node, the image is gone and the pod fails with ErrImageNeverPull. This happened to url-shortener on 2026-09-28.",
			Fix:      "Push the image to a registry the cluster can reach and use imagePullPolicy IfNotPresent (or pin by digest).",
			Verify:   "kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.containers[*].imagePullPolicy}{\"\\n\"}{end}' | grep Never   # nothing",
		}, localOnly))
	}
	if len(evicted) > 0 {
		out = append(out, group(protocol.HealthFinding{Rule: RuleEvicted, Severity: protocol.SeverityWarning, Object: scope(in.Namespace),
			Title:    fmt.Sprintf("%s were evicted", plural(len(evicted), "pod")),
			Evidence: fmt.Sprintf("Stopped by the kubelet (DisruptionTarget TerminationByKubelet, or reason Evicted): %s.", joinMax(evicted, 5)),
			Meaning:  "The kubelet stopped these pods to protect the node (disk, memory or PID pressure). Their records stay until deleted.",
			Why:      "Evictions mean the node ran out of something; the records also hide the real pods in lists (2,840 of them after the disk incident).",
			Fix:      "Find what ran out (kubectl describe pod shows the message: 'The node was low on resource: ephemeral-storage'), fix that, then delete only these records by name: " + deleteCommands(evicted) + ". Do not delete by phase across all namespaces: that also removes failed Job pods kept for debugging.",
			Verify:   strings.ReplaceAll(deleteCommands(evicted), "kubectl delete pod", "kubectl get pod") + "   # NotFound",
		}, evicted))
	}
	if len(noLimit) > 0 {
		out = append(out, group(protocol.HealthFinding{Rule: RuleNoMemoryLimit, Severity: protocol.SeverityWarning, Object: scope(in.Namespace),
			Title:    fmt.Sprintf("%s have containers without a memory limit", plural(len(noLimit), "pod")),
			Evidence: fmt.Sprintf("In %s: %s.", namespaces(noLimit), joinMax(noLimit, 5)),
			Meaning:  "These containers may use all the memory of their node.",
			Why:      "One leak then pushes the node into swap and memory pressure; the kernel OOM killer may hit other pods or system services, and a ResourceQuota cannot account for them.",
			Fix:      "Set resources.requests.memory and resources.limits.memory on each container (from its real peak: kubectl top pods), or a LimitRange with defaults in each namespace.",
			Verify:   "kubectl get pods -n <namespace> -o custom-columns=POD:.metadata.name,MEMORY_LIMITS:.spec.containers[*].resources.limits.memory   # no <none>",
		}, noLimit))
	}
	return out
}

func scope(ns string) string {
	if ns == "" {
		return "Cluster"
	}
	return "Namespace/" + ns
}

// namespaces summarises "ns/pod (...)" items as "3 namespaces (a, b, c)".
func namespaces(items []string) string {
	var ns []string
	for _, it := range items {
		n, _, _ := strings.Cut(it, "/")
		if !slices.Contains(ns, n) {
			ns = append(ns, n)
		}
	}
	slices.Sort(ns)
	return fmt.Sprintf("%s (%s)", plural(len(ns), "namespace"), joinMax(ns, 6))
}

func neverPull(p *corev1.Pod) bool {
	for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
		if c.ImagePullPolicy == corev1.PullNever {
			return true
		}
	}
	return false
}

// localOnlyPods lists the pods on a node that use imagePullPolicy Never, in
// any phase: an evicted or crash-looping pod's image is not in use by a
// running container, so it is the first one image pruning deletes.
func localOnlyPods(pods []*corev1.Pod, node string) []string {
	var out []string
	for _, p := range pods {
		if p.Spec.NodeName == node && neverPull(p) {
			out = append(out, p.Namespace+"/"+p.Name)
		}
	}
	slices.Sort(out)
	return out
}

func memLimit(p *corev1.Pod, container string) string {
	for _, c := range p.Spec.Containers {
		if c.Name == container {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				return q.String()
			}
		}
	}
	return ""
}

func containerFindings(p *corev1.Pod, obj string, now time.Time) []protocol.HealthFinding {
	var out []protocol.HealthFinding
	for _, cs := range p.Status.ContainerStatuses {
		waiting := ""
		if cs.State.Waiting != nil {
			waiting = cs.State.Waiting.Reason
		}
		last := cs.LastTerminationState.Terminated
		if last == nil {
			last = cs.State.Terminated
		}
		killed := last != nil && (last.ExitCode == 137 || last.Reason == "OOMKilled") &&
			(last.FinishedAt.IsZero() || now.Sub(last.FinishedAt.Time) <= Exit137Window)
		switch {
		case killed:
			sev := protocol.SeverityWarning
			if waiting == "CrashLoopBackOff" {
				sev = protocol.SeverityCritical
			}
			limit := memLimit(p, cs.Name)
			ev := fmt.Sprintf("container %s: last run ended with exit code %d, reason %q, at %s; restarts %d", cs.Name, last.ExitCode, last.Reason,
				last.FinishedAt.UTC().Format("2006-01-02 15:04 MST"), cs.RestartCount)
			if limit != "" {
				ev += "; memory limit " + limit
			} else {
				ev += "; no memory limit (the node's memory ran out)"
			}
			if last.Reason != "OOMKilled" {
				ev += ". The runtime does not always say OOMKilled (k3s 1.34 with containerd 2.2 reports Error); exit 137 is SIGKILL"
			}
			if waiting != "" {
				ev += "; now " + waiting
			}
			out = append(out, protocol.HealthFinding{Rule: RuleExit137, Severity: sev, Object: obj,
				Title:    fmt.Sprintf("Killed for memory (exit 137): %s", cs.Name),
				Evidence: ev + ".",
				Meaning:  "The kernel killed the container with SIGKILL. With a memory limit this is almost always the memory limit (the OOM killer); otherwise the node ran out of memory. Rarely it is a liveness probe that failed past its grace period.",
				Why:      "The work in progress is lost and the container restarts; with each crash the back-off grows up to 5 minutes.",
				Fix:      "Compare the limit with the real peak (kubectl top pod, Kubemap pod usage) and raise the limit, or fix the leak or the batch size. Check the events for liveness probe failures first.",
				Verify: fmt.Sprintf("kubectl get pod -n %s %s -w   # RESTARTS stops growing\njournalctl -k | grep -i 'memory cgroup out of memory'   # on the node: no new kills",
					p.Namespace, p.Name),
			})
		case waiting == "CrashLoopBackOff":
			ev := fmt.Sprintf("container %s: CrashLoopBackOff, restarts %d", cs.Name, cs.RestartCount)
			if last != nil {
				ev += "; last exit " + diagnose.ExitMeaning(last.ExitCode, last.Reason)
			}
			out = append(out, protocol.HealthFinding{Rule: RuleCrashLoop, Severity: protocol.SeverityCritical, Object: obj,
				Title:    "CrashLoopBackOff: " + cs.Name,
				Evidence: ev + ".",
				Meaning:  "The container starts, exits and is restarted again and again, with a growing wait (up to 5 minutes) between tries.",
				Why:      "The pod serves nothing while it crashes; if it belongs to a Service, that Service has fewer or no ready endpoints.",
				Fix:      fmt.Sprintf("Read why it exited: kubectl logs -n %s %s -c %s --previous (or Kubemap pod.diagnose). Fix the cause (a missing setting, a failing dependency, a wrong command), then roll out again.", p.Namespace, p.Name, cs.Name),
				Verify:   fmt.Sprintf("kubectl get pod -n %s %s -w   # READY and RESTARTS stops growing", p.Namespace, p.Name),
			})
		case slices.Contains(imagePullReasons, waiting):
			msg := ""
			if cs.State.Waiting != nil {
				msg = cs.State.Waiting.Message
			}
			f := protocol.HealthFinding{Rule: RuleImagePull, Severity: protocol.SeverityCritical, Object: obj,
				Title:    fmt.Sprintf("%s: %s", waiting, cs.Name),
				Evidence: fmt.Sprintf("container %s, image %s: %s %.200s", cs.Name, cs.Image, waiting, msg),
				Meaning:  "The container cannot start because its image cannot be pulled.",
				Why:      "The pod never runs until the image is available.",
				Fix:      "Check the image name and tag, the registry credentials (imagePullSecrets) and that the node can reach the registry.",
				Verify:   fmt.Sprintf("kubectl get pod -n %s %s   # Running", p.Namespace, p.Name),
			}
			if waiting == "ErrImageNeverPull" {
				f.Meaning = "imagePullPolicy is Never and the image is not on the node, so the kubelet may not pull it."
				f.Why = "The pod never runs. Usually the image was loaded by hand and then deleted by the kubelet's image clean-up (disk pressure or the image GC threshold)."
				f.Fix = "Short term: import the image into the node's runtime again (k3s ctr images import). For good: push it to a registry and use imagePullPolicy IfNotPresent."
			}
			out = append(out, f)
		}
	}
	return out
}

func pendingFinding(p *corev1.Pod, obj string, now time.Time) (protocol.HealthFinding, bool) {
	if p.Status.Phase != corev1.PodPending {
		return protocol.HealthFinding{}, false
	}
	var sched *corev1.PodCondition
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == corev1.PodScheduled {
			sched = &p.Status.Conditions[i]
		}
	}
	if sched == nil || sched.Status != corev1.ConditionFalse {
		return protocol.HealthFinding{}, false // scheduled: waiting for images or volumes, covered elsewhere
	}
	since := p.CreationTimestamp.Time
	if !sched.LastTransitionTime.IsZero() && sched.LastTransitionTime.After(since) {
		since = sched.LastTransitionTime.Time
	}
	if now.Sub(since) < PendingAfter {
		return protocol.HealthFinding{}, false
	}
	msg := sched.Message
	f := protocol.HealthFinding{Rule: RulePending, Severity: protocol.SeverityWarning, Object: obj,
		Title:    "Pending: no node can run it",
		Evidence: fmt.Sprintf("Pending for %s: %s: %s", now.Sub(since).Round(time.Minute), sched.Reason, msg),
		Meaning:  "The scheduler found no node that fits the pod.",
		Why:      "The pod does not run at all; its Deployment or StatefulSet stays short of replicas.",
		Fix:      "Read the scheduler's reason in kubectl describe pod (FailedScheduling), then change the pod or add capacity.",
		Verify:   fmt.Sprintf("kubectl get pod -n %s %s   # Running\nkubectl describe pod -n %s %s | grep -A3 Events", p.Namespace, p.Name, p.Namespace, p.Name),
	}
	switch {
	case strings.Contains(msg, "Insufficient cpu") || strings.Contains(msg, "Insufficient memory"):
		f.Title = "Pending: requests more than any node has free"
		f.Fix = "Lower the pod's resources.requests to what it really uses (kubectl top pod), free capacity (kubectl describe node: Allocated resources), or add a node."
	case strings.Contains(msg, "untolerated taint"):
		f.Title = "Pending: nodes are tainted"
		f.Fix = "Add a toleration for the taint, or remove the taint from a node (kubectl describe node | grep Taints)."
	case strings.Contains(msg, "node affinity") || strings.Contains(msg, "node selector"):
		f.Title = "Pending: no node matches its node selector or affinity"
		f.Fix = "Fix the nodeSelector or affinity, or label a node to match (kubectl get nodes --show-labels)."
	case strings.Contains(msg, "unbound") || strings.Contains(msg, "PersistentVolumeClaim"):
		f.Title = "Pending: its volume claim is not bound"
		f.Fix = "Check the PVC (kubectl get pvc -n " + p.Namespace + "): its StorageClass and provisioner."
	}
	return f, true
}

// evictedByKubelet is a pod the kubelet stopped to protect its node. On
// Kubernetes 1.26+ such a pod has the DisruptionTarget condition with reason
// TerminationByKubelet and may end Succeeded, not Failed (k3s 1.34 does);
// older versions set status.reason Evicted.
func evictedByKubelet(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded {
		return false
	}
	if p.Status.Reason == "Evicted" {
		return true
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue && c.Reason == "TerminationByKubelet" {
			return true
		}
	}
	return false
}

// deleteCommands turns "ns/pod" items into one kubectl delete per namespace
// (at most 3 namespaces and 10 pods each shown).
func deleteCommands(items []string) string {
	byNS := map[string][]string{}
	var order []string
	for _, it := range items {
		ns, name, _ := strings.Cut(it, "/")
		if _, ok := byNS[ns]; !ok {
			order = append(order, ns)
		}
		byNS[ns] = append(byNS[ns], name)
	}
	slices.Sort(order)
	var cmds []string
	for _, ns := range order[:min(len(order), 3)] {
		names := byNS[ns]
		cmd := "kubectl delete pod -n " + ns + " " + strings.Join(names[:min(len(names), 10)], " ")
		if len(names) > 10 {
			cmd += fmt.Sprintf(" … (%d more)", len(names)-10)
		}
		cmds = append(cmds, cmd)
	}
	out := strings.Join(cmds, "; ")
	if len(order) > 3 {
		out += fmt.Sprintf("; and the same for %d more namespaces", len(order)-3)
	}
	return out
}
