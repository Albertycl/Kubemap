// Package pressure decides whether a node's workloads are really waiting for
// a resource (KM-60). It combines the kubelet's node conditions
// (MemoryPressure, DiskPressure, PIDPressure, from the cluster agent's
// cache) with kernel pressure stall information (PSI) from the node agent's
// latest report, which the portal passes along with the request. The
// cluster agent makes no kubelet calls, so its token never goes to a node.
//
//   - A condition that is True wins: the kubelet is already acting (evicting
//     pods, refusing new ones).
//   - Otherwise PSI decides: "some" over 60 s at PSISomeElevated or
//     PSISomePressure, or "full" at PSIFullPressure.
//   - CPU and I/O have no condition, so without PSI (no fresh report, or a
//     kernel without PSI) they are unknown, never "ok": Kubernetes cannot see
//     CPU starvation, and saying "all clear" would be wrong.
package pressure

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemap/pkg/protocol"
)

// Reports indexes node agent reports by node name.
func Reports(ms []protocol.Metrics) map[string]*protocol.Metrics {
	out := make(map[string]*protocol.Metrics, len(ms))
	for i := range ms {
		out[ms[i].NodeName] = &ms[i]
	}
	return out
}

// Fresh returns the node's report if it is younger than NodeReportMaxAge,
// else nil and why not (for a note).
func Fresh(reports map[string]*protocol.Metrics, node string, now time.Time) (*protocol.Metrics, string) {
	m, ok := reports[node]
	switch {
	case !ok:
		return nil, fmt.Sprintf("node %s: no node agent report (no node agent on this node, or it is not connected)", node)
	case now.Sub(m.Time) > protocol.NodeReportMaxAge:
		return nil, fmt.Sprintf("node %s: the node agent report is %s old (taken %s); PSI not used", node,
			now.Sub(m.Time).Round(time.Second), m.Time.UTC().Format(time.RFC3339))
	}
	return m, ""
}

// Report evaluates the nodes with the node agents' reports. A node without a
// fresh report gives a note and is judged from its conditions only.
func Report(nodes []*corev1.Node, reports map[string]*protocol.Metrics, now time.Time) *protocol.NodePressureReport {
	out := &protocol.NodePressureReport{Time: now.UTC(), Nodes: []protocol.NodePressure{}}
	for _, n := range nodes {
		m, note := Fresh(reports, n.Name, now)
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
		out.Nodes = append(out.Nodes, Evaluate(n, m))
	}
	sort.Slice(out.Nodes, func(a, b int) bool { return out.Nodes[a].Node < out.Nodes[b].Node })
	sort.Strings(out.Notes)
	return out
}

// Evaluate is one node. m is its node agent report, nil when there is none
// (or it is too old).
func Evaluate(n *corev1.Node, m *protocol.Metrics) protocol.NodePressure {
	np := protocol.NodePressure{Node: n.Name, Ready: condStatus(n, corev1.NodeReady) == corev1.ConditionTrue}
	var cpuPSI, memPSI, ioPSI *protocol.Pressure
	if m != nil {
		np.ReportTime = m.Time.UTC()
		if m.PSI != nil {
			cpuPSI, memPSI, ioPSI = &m.PSI.CPU, &m.PSI.Memory, &m.PSI.IO
		}
	}
	np.PSIAvailable = cpuPSI != nil
	np.Resources = []protocol.ResourcePressure{
		resource(n, "cpu", "", cpuPSI, np.PSIAvailable),
		resource(n, "memory", corev1.NodeMemoryPressure, memPSI, np.PSIAvailable),
		resource(n, "io", "", ioPSI, np.PSIAvailable),
		resource(n, "disk", corev1.NodeDiskPressure, nil, np.PSIAvailable),
		resource(n, "pids", corev1.NodePIDPressure, nil, np.PSIAvailable),
	}
	np.Verdict, np.Summary = overall(np)
	return np
}

// what a person reads for each resource: the stalled activity, and what the
// kubelet does when its condition is True.
var words = map[string]struct{ waiting, acting string }{
	"cpu":    {"waited for CPU", ""},
	"memory": {"stalled on memory (reclaim, swap)", "the kubelet evicts pods to free memory and admits no new BestEffort pods"},
	"io":     {"waited for disk or network I/O", ""},
	"disk":   {"", "the kubelet evicts pods and deletes unused images until the minimum-reclaim target is free"},
	"pids":   {"", "the kubelet evicts pods to free process IDs"},
}

func resource(n *corev1.Node, name string, cond corev1.NodeConditionType, psi *protocol.Pressure, psiOnNode bool) protocol.ResourcePressure {
	rp := protocol.ResourcePressure{Resource: name, PSI: psi}
	w := words[name]
	var c *corev1.NodeCondition
	if cond != "" {
		rp.Condition = string(cond)
		for i := range n.Status.Conditions {
			if n.Status.Conditions[i].Type == cond {
				c = &n.Status.Conditions[i]
			}
		}
		if c != nil {
			rp.ConditionStatus = string(c.Status)
			rp.ConditionSince = c.LastTransitionTime.UTC()
		}
	}
	switch {
	case c != nil && c.Status == corev1.ConditionTrue:
		rp.Verdict = protocol.VerdictPressure
		rp.Detail = fmt.Sprintf("%s is True since %s: %s", cond, c.LastTransitionTime.UTC().Format(time.RFC3339), w.acting)
		if c.Message != "" {
			rp.Detail += " (" + c.Message + ")"
		}
		return rp
	case psi != nil:
		rp.Verdict, rp.Detail = psiVerdict(name, psi, w.waiting)
		if cond != "" && c != nil && c.Status == corev1.ConditionFalse && rp.Verdict != protocol.VerdictOK {
			rp.Detail += fmt.Sprintf("; %s is still False: the kubelet only reacts at its eviction threshold", cond)
		}
		if cond == "" && rp.Verdict != protocol.VerdictOK {
			rp.Detail += fmt.Sprintf("; Kubernetes has no %s pressure condition, so it reports all clear", name)
		}
		return rp
	case c != nil && c.Status == corev1.ConditionFalse:
		rp.Verdict = protocol.VerdictOK
		rp.Detail = fmt.Sprintf("%s is False", cond)
		if name == "memory" {
			rp.Detail += "; PSI not available, so stalls below the eviction threshold cannot be seen"
		}
		return rp
	case c != nil:
		rp.Verdict = protocol.VerdictUnknown
		rp.Detail = fmt.Sprintf("%s is %s: the kubelet stopped reporting (node down or unreachable)", cond, c.Status)
		return rp
	}
	rp.Verdict = protocol.VerdictUnknown
	if cond != "" {
		rp.Detail = fmt.Sprintf("the node reports no %s condition", cond)
	} else if psiOnNode {
		rp.Detail = "not available: the node agent reports no PSI for " + name
	} else {
		rp.Detail = "not available: no fresh node agent report with PSI (needs the node agent, and a kernel with PSI), and Kubernetes has no " + name + " pressure condition"
	}
	return rp
}

// psiVerdict judges the 60-second averages. "full" (all tasks stalled)
// counts for memory and io; for cpu the kernel reports it only for cgroups,
// so the node-level value is 0 or absent and is ignored.
func psiVerdict(name string, p *protocol.Pressure, waiting string) (string, string) {
	some := p.Some.Avg60
	full := 0.0
	if p.Full != nil && name != "cpu" {
		full = p.Full.Avg60
	}
	detail := fmt.Sprintf("tasks %s %.1f%% of the last minute (10 s: %.1f%%, 5 min: %.1f%%)", waiting, some, p.Some.Avg10, p.Some.Avg300)
	if full > 0 {
		detail += fmt.Sprintf(", all tasks stalled %.1f%%", full)
	}
	switch {
	case full >= protocol.PSIFullPressure || some >= protocol.PSISomePressure:
		return protocol.VerdictPressure, detail
	case some >= protocol.PSISomeElevated:
		return protocol.VerdictElevated, detail
	}
	return protocol.VerdictOK, detail
}

var rank = map[string]int{protocol.VerdictOK: 0, protocol.VerdictUnknown: 1, protocol.VerdictElevated: 2, protocol.VerdictPressure: 3}

func overall(np protocol.NodePressure) (string, string) {
	worst := protocol.VerdictOK
	var names []string
	for _, r := range np.Resources {
		if rank[r.Verdict] > rank[worst] {
			worst, names = r.Verdict, nil
		}
		if r.Verdict == worst && worst != protocol.VerdictOK {
			names = append(names, r.Resource)
		}
	}
	var s string
	switch worst {
	case protocol.VerdictOK:
		s = "no pressure"
	case protocol.VerdictUnknown:
		s = fmt.Sprintf("no pressure seen; not available: %v", names)
	default:
		s = fmt.Sprintf("%s: %v", worst, names)
	}
	if !np.PSIAvailable {
		s += " (PSI not available: CPU and I/O stalls cannot be seen)"
	}
	if !np.Ready {
		s = "node not Ready; " + s
	}
	return worst, s
}

func condStatus(n *corev1.Node, t corev1.NodeConditionType) corev1.ConditionStatus {
	for _, c := range n.Status.Conditions {
		if c.Type == t {
			return c.Status
		}
	}
	return ""
}
