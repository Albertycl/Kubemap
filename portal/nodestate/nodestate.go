// Package nodestate tells "node down" from "node agent down" (KM-65).
//
// Two independent witnesses report on every node:
//
//   - the node agent, over its own connection to the portal: a heartbeat
//     every protocol.HeartbeatInterval (10 s); the portal counts it silent
//     after protocol.DisconnectAfter (30 s) without any message;
//   - Kubernetes, through the cluster agent's summary: the node's Ready
//     condition. The kubelet stops updating it when the node dies, and the
//     control plane marks it NotReady or Unknown after its node monitor
//     grace period (40 to 50 s).
//
// Crossing both gives the state. When the node agent goes silent but
// Kubernetes still says Ready, the portal waits ConfirmAfter before it
// calls the node agent (not the node) the problem, because Kubernetes
// notices a dead node later than the portal notices a dead agent.
package nodestate

import (
	"fmt"
	"time"

	"kubemap/pkg/protocol"
)

// Codes.
const (
	OK           = "ok"           // agent reporting, Kubernetes Ready (or no Kubernetes view)
	Reconnecting = "reconnecting" // agent connection dropped less than SilentAfter ago
	Unreachable  = "unreachable"  // agent silent, Kubernetes has not decided yet
	NodeDown     = "node-down"    // agent silent (or none) and Kubernetes NotReady
	AgentDown    = "agent-down"   // agent silent, but Kubernetes still sees the node Ready
	NotReady     = "not-ready"    // agent reporting, but Kubernetes says NotReady
	NoAgent      = "no-agent"     // Kubernetes knows the node, no node agent ever registered
	Removed      = "removed"      // a node agent reported once, Kubernetes no longer lists the node
)

// Severities, for colour.
const (
	SevOK   = "ok"
	SevWarn = "warn"
	SevFail = "fail"
)

// SilentAfter is how long without a message counts as silent.
const SilentAfter = protocol.DisconnectAfter

// ConfirmAfter is how long after the node agent's last message the portal
// waits for Kubernetes to notice a dead node (the kubelet's node monitor
// grace period is 40 s, 50 s on newer versions, plus the summary delay)
// before it concludes that only the node agent is down.
const ConfirmAfter = 90 * time.Second

// Agent is what the portal knows about the node agent.
type Agent struct {
	Known     bool // a node agent registered at some time
	Connected bool // it holds a connection now
	LastSeen  time.Time
	// Since is when the portal started watching (its start time). A node
	// agent cannot reach a portal that is not running, so silence is
	// counted from the later of LastSeen and Since: right after a portal
	// restart, agents that have not reconnected yet read "reconnecting",
	// not "node agent down". Zero means no restart to allow for.
	Since time.Time
}

// Kube is the cluster agent's view of the node.
type Kube struct {
	// Available is false when the cluster agent is not connected (or has
	// sent no summary): then Kubernetes' opinion is unknown.
	Available bool
	Listed    bool // the node is in the summary's node list
	Ready     bool
}

// State is the verdict for one node.
type State struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	// SilentFor is how long the node agent has not been heard from, in
	// seconds; 0 while it reports.
	SilentFor int64 `json:"silentForSeconds,omitempty"`
	// KubernetesReady is the node's Ready condition; nil when there is no
	// Kubernetes view (cluster agent offline) or the node is not listed.
	KubernetesReady *bool `json:"kubernetesReady,omitempty"`
}

// Classify crosses the two witnesses.
func Classify(a Agent, k Kube, now time.Time) State {
	var silent time.Duration
	restarted := a.Since.After(a.LastSeen)
	if a.Known && !a.Connected {
		from := a.LastSeen
		if restarted {
			from = a.Since
		}
		silent = max(now.Sub(from), 0)
	}
	reporting := a.Known && (a.Connected || silent < SilentAfter)
	s := State{}
	if a.Known && !reporting {
		s.SilentFor = int64(silent / time.Second)
	}
	if k.Available && k.Listed {
		ready := k.Ready
		s.KubernetesReady = &ready
	}
	ago := func() string {
		if restarted {
			return fmt.Sprintf("the portal started, %s ago (it was last heard from before that)", silent.Round(time.Second))
		}
		return fmt.Sprintf("%s ago", silent.Round(time.Second))
	}

	switch {
	case !a.Known && !k.Available:
		return fill(s, NoAgent, SevWarn, "No node agent",
			"No node agent has registered for this node, and the cluster agent is not connected, so nothing is known about it.")
	case !a.Known && !k.Listed:
		return fill(s, Removed, SevWarn, "Not in the cluster", "Kubernetes does not list this node.")
	case !a.Known && k.Ready:
		return fill(s, NoAgent, SevWarn, "No node agent",
			"Kubernetes sees the node Ready, but no node agent has registered for it: is the node agent DaemonSet scheduled there (taints, node selector)?")
	case !a.Known:
		return fill(s, NodeDown, SevFail, "Node down",
			"Kubernetes says NotReady, and no node agent reports for this node: the node is down or its kubelet has stopped.")

	case k.Available && !k.Listed:
		return fill(s, Removed, SevWarn, "Removed from the cluster",
			"A node agent reported for this node before, but Kubernetes no longer lists it: the node was deleted or renamed.")

	case reporting && !a.Connected && restarted:
		return fill(s, Reconnecting, SevWarn, "Node agent reconnecting",
			fmt.Sprintf("The portal started %s ago and the node agent has not reconnected yet. It is counted silent after %s.", silent.Round(time.Second), SilentAfter))
	case reporting && !a.Connected:
		return fill(s, Reconnecting, SevWarn, "Node agent reconnecting",
			fmt.Sprintf("The node agent's connection dropped; last heard from %s. It is counted silent after %s.", ago(), SilentAfter))
	case reporting && k.Available && !k.Ready:
		return fill(s, NotReady, SevFail, "Up, but NotReady",
			"The node agent reports, so the machine and its network are up, but Kubernetes says NotReady: the kubelet, the container runtime or the network plugin has a problem.")
	case reporting && !k.Available:
		return fill(s, OK, SevOK, "Reporting",
			"The node agent reports. The cluster agent is not connected, so Kubernetes' view of the node is unknown.")
	case reporting:
		return fill(s, OK, SevOK, "Up", "The node agent reports and Kubernetes says Ready.")

	case !k.Available:
		return fill(s, Unreachable, SevFail, "Unreachable",
			fmt.Sprintf("The node agent has been silent since %s, and the cluster agent is not connected either: the node, its network, or the path to the portal is down.", ago()))
	case !k.Ready:
		return fill(s, NodeDown, SevFail, "Node down",
			fmt.Sprintf("The node agent has been silent since %s, and Kubernetes says NotReady: the node is down or unreachable.", ago()))
	case silent < ConfirmAfter:
		return fill(s, Unreachable, SevFail, "Unreachable",
			fmt.Sprintf("The node agent has been silent since %s. Kubernetes still says Ready; it notices a dead node after about 40 to 50 s, so this is not confirmed yet.", ago()))
	default:
		return fill(s, AgentDown, SevWarn, "Node agent down",
			fmt.Sprintf("Kubernetes still sees the node Ready, but the node agent has been silent since %s: the node agent pod has stopped, or its network path to the portal is blocked.", ago()))
	}
}

func fill(s State, code, sev, title, detail string) State {
	s.Code, s.Severity, s.Title, s.Detail = code, sev, title, detail
	return s
}

// KubeView builds the Kube witness for every node from a cluster agent's
// summary. connected is whether the cluster agent holds its connection.
//
// The summary is trusted at any age while the cluster agent is connected.
// That holds only because the cluster agent pushes a new summary within
// protocol.SummaryInterval (5 s) of any change in its cache, a node's Ready
// condition included (agent/cluster/summary, Pusher.Tick). If it ever sends
// summaries only on a fixed, longer period, add that period to
// ConfirmAfter, or a dead node would read "node agent down" until the next
// summary.
func KubeView(sum *protocol.Summary, connected bool) func(node string) Kube {
	ready := map[string]bool{}
	available := connected && sum != nil
	// The list is complete only when it has every node: an older agent
	// sends none, and a very large cluster is cut at MaxSummaryNodes. A
	// node missing from an incomplete list is unknown, not removed.
	complete := available && len(sum.NodeList) >= sum.Nodes.Total
	if available {
		for _, n := range sum.NodeList {
			ready[n.Name] = n.Ready
		}
	}
	return func(node string) Kube {
		r, listed := ready[node]
		if !listed && !complete {
			return Kube{}
		}
		return Kube{Available: available, Listed: listed, Ready: r}
	}
}

// Listed returns the node names in a summary (for nodes without a node agent).
func Listed(sum *protocol.Summary, connected bool) []string {
	if !connected || sum == nil {
		return nil
	}
	out := make([]string, 0, len(sum.NodeList))
	for _, n := range sum.NodeList {
		out = append(out, n.Name)
	}
	return out
}
