// Package health runs Kubemap's built-in health rules in the cluster agent
// (KM-61) and explains every finding (KM-62): what it means, why it
// matters, the suggested fix and the command that proves the fix.
//
// Node rules read the node agent's latest report (sent along by the portal;
// the cluster agent makes no kubelet calls) and the node conditions: disk
// space before the eviction threshold (with the minimum-reclaim target the
// kubelet frees once it starts), image clean-up, inodes, memory available,
// swap, pressure stall information (KM-60), process IDs, and Ready. Pod rules
// read the cache: CrashLoopBackOff, containers killed with exit 137, images
// that cannot be pulled or exist only on the node, Pending pods, evicted
// pods and containers without a memory limit.
//
// Nothing here calls the Kubernetes API or a kubelet: the caller passes the
// objects and reports in, so the rules are plain functions and easy to test.
package health

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemap/pkg/protocol"
)

// Inputs are what the rules look at.
type Inputs struct {
	Now   time.Time
	Nodes []*corev1.Node
	// Reports are the node agents' latest reports by node name, already
	// checked for age (pressure.Fresh); a missing node has none (see Notes).
	Reports map[string]*protocol.Metrics
	Pods    []*corev1.Pod
	// Namespace, when set, limits the pod rules to it.
	Namespace  string
	Thresholds Thresholds
	// Notes from reading the inputs, passed through to the report.
	Notes []string
}

// Rule IDs, in the order they run.
var Rules = []string{
	RuleNodeNotReady, RuleDiskEviction, RuleImageFSEviction, RuleImageGC, RuleInodes,
	RuleMemory, RuleSwap, RulePressureCPU, RulePressureMemory, RulePressureIO, RulePIDs,
	RuleCrashLoop, RuleExit137, RuleImagePull, RuleLocalImages, RulePending, RuleEvicted, RuleNoMemoryLimit,
}

// Check runs every rule.
func Check(in Inputs) *protocol.HealthReport {
	if in.Thresholds.EvictionHard == nil {
		in.Thresholds = Defaults()
	}
	var fs []protocol.HealthFinding
	for _, n := range in.Nodes {
		fs = append(fs, nodeFindings(n, in.Reports[n.Name], in)...)
	}
	pods := in.Pods
	if in.Namespace != "" {
		pods = slices.DeleteFunc(slices.Clone(pods), func(p *corev1.Pod) bool { return p.Namespace != in.Namespace })
	}
	fs = append(fs, podFindings(pods, in)...)
	sort.SliceStable(fs, func(a, b int) bool {
		if ra, rb := sevRank[fs[a].Severity], sevRank[fs[b].Severity]; ra != rb {
			return ra < rb
		}
		if ia, ib := slices.Index(Rules, fs[a].Rule), slices.Index(Rules, fs[b].Rule); ia != ib {
			return ia < ib
		}
		return fs[a].Object < fs[b].Object
	})
	notes := slices.Clone(in.Notes)
	if len(fs) > protocol.MaxHealthFindings {
		notes = append(notes, fmt.Sprintf("%d more findings not shown (at most %d per report); ask for one namespace", len(fs)-protocol.MaxHealthFindings, protocol.MaxHealthFindings))
		fs = fs[:protocol.MaxHealthFindings]
	}
	if in.Thresholds.Source == "kubelet defaults" {
		notes = append(notes, "eviction thresholds are the kubelet defaults: the agent cannot read the kubelet configuration (that needs nodes/proxy); set KUBEMAP_EVICTION_HARD and KUBEMAP_EVICTION_MINIMUM_RECLAIM on the cluster agent if the kubelet is configured differently")
	}
	if fs == nil {
		fs = []protocol.HealthFinding{}
	}
	return &protocol.HealthReport{Time: in.Now.UTC(), Findings: fs, Rules: slices.Clone(Rules), Thresholds: in.Thresholds.protocol(), Notes: notes}
}

var sevRank = map[string]int{protocol.SeverityCritical: 0, protocol.SeverityWarning: 1, protocol.SeverityInfo: 2}

// group fills Items (at most MaxFindingItems, sorted) and Count.
func group(f protocol.HealthFinding, items []string) protocol.HealthFinding {
	sort.Strings(items)
	f.Count = len(items)
	f.Items = items[:min(len(items), protocol.MaxFindingItems)]
	return f
}

// gib formats bytes for people.
func gib(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", b/(1<<20))
	}
	return fmt.Sprintf("%.0f KiB", b/(1<<10))
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part * 100 / whole
}

// plural: "1 pod", "3 pods".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func joinMax(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}
