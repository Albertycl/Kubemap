// Package diagnose explains a crashing or stuck pod (KM-56): it gathers the
// pod's status, restarts, last exit code, previous logs, events, and
// requests and limits next to usage, and turns them into findings an SRE
// can act on ("killed for memory: the 256Mi limit was reached").
//
// Analyse is pure (inputs in, diagnosis out) so every rule is testable;
// Collect does the reads, all get and list.
package diagnose

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

// Inputs is what Collect gathered.
type Inputs struct {
	Pod    *corev1.Pod
	Owner  string         // top controller, or ""
	Events []corev1.Event // about this pod, any order
	Logs   map[string]Log // by container name
	// Usage by container, from the metrics API; nil when not available.
	Usage map[string]Usage
	Notes []string
}

// Log is the tail of one container's log.
type Log struct {
	Text      string
	From      string // "previous" or "current"
	Truncated bool
}

// Usage is one container's current usage.
type Usage struct {
	CPUMillis   int64
	MemoryBytes int64
}

// Analyse builds the diagnosis.
func Analyse(in Inputs) *protocol.PodDiagnosis {
	p := in.Pod
	d := &protocol.PodDiagnosis{
		Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Owner: in.Owner,
		Phase: string(p.Status.Phase), Status: status(p), Reason: p.Status.Reason, Message: p.Status.Message,
		QOS: string(p.Status.QOSClass), Notes: in.Notes, Findings: []protocol.DiagFinding{},
		Containers: []protocol.DiagContainer{}, Events: events(in.Events),
	}
	statuses := map[string]corev1.ContainerStatus{}
	for _, cs := range append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...) {
		statuses[cs.Name] = cs
	}
	add := func(c corev1.Container, init bool) {
		dc := container(c, init, statuses[c.Name], in)
		d.Containers = append(d.Containers, dc)
	}
	for _, c := range p.Spec.InitContainers {
		add(c, true)
	}
	for _, c := range p.Spec.Containers {
		add(c, false)
	}
	d.Findings = findings(d, p)
	return d
}

// status is the kubectl-style word, with the pod-level reason (Evicted)
// first, since the containers of an evicted pod look merely "Completed".
func status(p *corev1.Pod) string {
	if p.Status.Reason != "" && p.DeletionTimestamp == nil {
		return p.Status.Reason
	}
	if title, _ := disruption(p); title == "Evicted" {
		return title
	}
	return graph.PodStatus(p)
}

func container(c corev1.Container, init bool, cs corev1.ContainerStatus, in Inputs) protocol.DiagContainer {
	dc := protocol.DiagContainer{Name: c.Name, Init: init, Image: c.Image, Ready: cs.Ready, Restarts: cs.RestartCount,
		State: stateText(cs.State), Requests: quantities(c.Resources.Requests), Limits: quantities(c.Resources.Limits)}
	if t := cs.LastTerminationState.Terminated; t != nil {
		dc.Last = termination(t)
	} else if t := cs.State.Terminated; t != nil {
		dc.Last = termination(t)
	}
	if u, ok := in.Usage[c.Name]; ok {
		du := &protocol.DiagUsage{CPUMillis: u.CPUMillis, MemoryBytes: u.MemoryBytes}
		if l, ok := c.Resources.Limits[corev1.ResourceCPU]; ok && l.MilliValue() > 0 {
			du.CPUOfLimit = round1(float64(u.CPUMillis) / float64(l.MilliValue()) * 100)
		}
		if l, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && l.Value() > 0 {
			du.MemOfLimit = round1(float64(u.MemoryBytes) / float64(l.Value()) * 100)
		}
		dc.Usage = du
	}
	for _, pr := range []struct {
		name string
		p    *corev1.Probe
	}{{"startup", c.StartupProbe}, {"liveness", c.LivenessProbe}, {"readiness", c.ReadinessProbe}} {
		if pr.p != nil {
			dc.Probes = append(dc.Probes, pr.name+": "+probeText(pr.p))
		}
	}
	if l, ok := in.Logs[c.Name]; ok {
		dc.Logs, dc.LogsFrom, dc.LogsTruncated = l.Text, l.From, l.Truncated
	}
	return dc
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func quantities(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func stateText(s corev1.ContainerState) string {
	switch {
	case s.Running != nil:
		return "running since " + s.Running.StartedAt.UTC().Format(time.RFC3339)
	case s.Waiting != nil:
		return "waiting: " + s.Waiting.Reason
	case s.Terminated != nil:
		return fmt.Sprintf("terminated: %s (exit %d)", s.Terminated.Reason, s.Terminated.ExitCode)
	}
	return "unknown"
}

func termination(t *corev1.ContainerStateTerminated) *protocol.DiagTermination {
	return &protocol.DiagTermination{Reason: t.Reason, ExitCode: t.ExitCode, Signal: t.Signal,
		Meaning: ExitMeaning(t.ExitCode, t.Reason), Started: t.StartedAt.UTC(), Finished: t.FinishedAt.UTC(),
		Message: clip(t.Message, 500)}
}

// ExitMeaning explains a container exit code.
func ExitMeaning(code int32, reason string) string {
	if reason == "OOMKilled" {
		return fmt.Sprintf("OOMKilled (%d): the container went over its memory limit and the kernel killed it", code)
	}
	switch code {
	case 0:
		return "0: exited normally"
	case 1:
		return "1: the application exited with an error; the log says why"
	case 2:
		return "2: misuse of a shell command or bad arguments"
	case 126:
		return "126: the command is not executable"
	case 127:
		return "127: command not found; check the image and the command"
	case 130:
		return "SIGINT (130): interrupted"
	case 134:
		return "SIGABRT (134): the process aborted itself"
	case 137:
		return "SIGKILL (137): killed, almost always by the OOM killer at the memory limit (some runtimes report the reason as Error); otherwise a liveness kill after the grace period or a forced stop"
	case 139:
		return "SIGSEGV (139): segmentation fault"
	case 143:
		return "SIGTERM (143): asked to stop (liveness probe failure, rollout, eviction or node drain)"
	case 255:
		return "255: exit status out of range, often exit(-1)"
	}
	if code > 128 && code < 160 {
		return fmt.Sprintf("signal %d (%d)", code-128, code)
	}
	return fmt.Sprintf("%d: application-specific exit code", code)
}

func probeText(p *corev1.Probe) string {
	what := "exec"
	switch {
	case p.HTTPGet != nil:
		what = fmt.Sprintf("GET %s :%s", cmp.Or(p.HTTPGet.Path, "/"), p.HTTPGet.Port.String())
	case p.TCPSocket != nil:
		what = "TCP :" + p.TCPSocket.Port.String()
	case p.GRPC != nil:
		what = fmt.Sprintf("gRPC :%d", p.GRPC.Port)
	case p.Exec != nil:
		what = "exec " + clip(strings.Join(p.Exec.Command, " "), 60)
	}
	return fmt.Sprintf("%s every %ds, timeout %ds, %d failures, first after %ds", what,
		max(p.PeriodSeconds, 1), max(p.TimeoutSeconds, 1), max(p.FailureThreshold, 1), p.InitialDelaySeconds)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// events returns the pod's events newest first, at most 20.
func events(evs []corev1.Event) []protocol.DiagEvent {
	out := []protocol.DiagEvent{}
	for _, e := range evs {
		last := e.LastTimestamp.Time
		if last.IsZero() {
			last = e.EventTime.Time
		}
		if last.IsZero() {
			last = e.FirstTimestamp.Time
		}
		count := max(e.Count, 1)
		if e.Series != nil && e.Series.Count > count {
			count = e.Series.Count
		}
		out = append(out, protocol.DiagEvent{Type: e.Type, Reason: e.Reason, Message: clip(e.Message, 500), Count: count, Last: last.UTC()})
	}
	slices.SortStableFunc(out, func(a, b protocol.DiagEvent) int { return b.Last.Compare(a.Last) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func eventCount(evs []protocol.DiagEvent, reason, contains string) int32 {
	var n int32
	for _, e := range evs {
		if e.Reason == reason && strings.Contains(e.Message, contains) {
			n += e.Count
		}
	}
	return n
}
