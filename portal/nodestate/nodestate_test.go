package nodestate

import (
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

var now = time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func TestClassify(t *testing.T) {
	up := Kube{Available: true, Listed: true, Ready: true}
	down := Kube{Available: true, Listed: true, Ready: false}
	noView := Kube{}
	gone := Kube{Available: true, Listed: false}
	live := Agent{Known: true, Connected: true, LastSeen: ago(3 * time.Second)}
	silent := func(d time.Duration) Agent { return Agent{Known: true, LastSeen: ago(d)} }
	for _, c := range []struct {
		name     string
		a        Agent
		k        Kube
		code     string
		sev      string
		inDetail string
	}{
		{"both fine", live, up, OK, SevOK, "Kubernetes says Ready"},
		{"agent fine, no Kubernetes view", live, noView, OK, SevOK, "cluster agent is not connected"},
		{"kubelet broken, machine up", live, down, NotReady, SevFail, "kubelet"},
		{"connection just dropped", silent(12 * time.Second), up, Reconnecting, SevWarn, "12s ago"},
		{"silent 30 s, Kubernetes not yet", silent(30 * time.Second), up, Unreachable, SevFail, "not confirmed yet"},
		{"silent 89 s, Kubernetes still Ready", silent(89 * time.Second), up, Unreachable, SevFail, "not confirmed yet"},
		{"silent 90 s, Kubernetes Ready: agent down", silent(90 * time.Second), up, AgentDown, SevWarn, "node agent pod has stopped"},
		{"silent and NotReady: node down", silent(45 * time.Second), down, NodeDown, SevFail, "node is down or unreachable"},
		{"silent, no Kubernetes view", silent(5 * time.Minute), noView, Unreachable, SevFail, "cluster agent is not connected either"},
		{"known agent, node deleted", silent(time.Hour), gone, Removed, SevWarn, "no longer lists it"},
		{"no agent, node Ready", Agent{}, up, NoAgent, SevWarn, "DaemonSet"},
		{"no agent, node NotReady", Agent{}, down, NodeDown, SevFail, "NotReady"},
		{"no agent, no view", Agent{}, noView, NoAgent, SevWarn, "nothing is known"},
	} {
		s := Classify(c.a, c.k, now)
		if s.Code != c.code || s.Severity != c.sev || !strings.Contains(s.Detail, c.inDetail) || s.Title == "" {
			t.Errorf("%s: got %s/%s %q, want %s/%s containing %q", c.name, s.Code, s.Severity, s.Detail, c.code, c.sev, c.inDetail)
		}
	}
	// After a portal restart (review N-1): silence counts from the portal's
	// start, not from a last_seen written before the stop.
	restart := func(started, last time.Duration) Agent {
		return Agent{Known: true, LastSeen: ago(last), Since: ago(started)}
	}
	for _, c := range []struct {
		name     string
		a        Agent
		code     string
		inDetail string
	}{
		{"portal started 20 s ago, last seen 10 min ago", restart(20*time.Second, 10*time.Minute), Reconnecting, "portal started 20s ago"},
		{"portal started 40 s ago, agent not back", restart(40*time.Second, 10*time.Minute), Unreachable, "the portal started, 40s ago"},
		{"portal started 2 min ago, agent not back", restart(2*time.Minute, 10*time.Minute), AgentDown, "the portal started, 2m0s ago"},
		{"started long ago, agent dropped later", restart(time.Hour, 12*time.Second), Reconnecting, "last heard from 12s ago"},
	} {
		s := Classify(c.a, up, now)
		if s.Code != c.code || !strings.Contains(s.Detail, c.inDetail) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, s.Code, s.Detail, c.code, c.inDetail)
		}
	}
	if s := Classify(restart(20*time.Second, 10*time.Minute), up, now); s.SilentFor != 0 {
		t.Errorf("reconnecting after a restart is not silent yet: %+v", s)
	}
	if s := Classify(restart(40*time.Second, 10*time.Minute), up, now); s.SilentFor != 40 {
		t.Errorf("silence counts from the portal start: %+v", s)
	}

	// Evidence fields.
	s := Classify(silent(95*time.Second), up, now)
	if s.SilentFor != 95 || s.KubernetesReady == nil || !*s.KubernetesReady {
		t.Errorf("evidence: %+v", s)
	}
	if s := Classify(live, noView, now); s.SilentFor != 0 || s.KubernetesReady != nil {
		t.Errorf("reporting, no view: %+v", s)
	}
}

func TestKubeView(t *testing.T) {
	sum := &protocol.Summary{Nodes: protocol.NodeCounts{Total: 2, Ready: 1},
		NodeList: []protocol.NodeStatus{{Name: "a", Ready: true}, {Name: "b", Ready: false}}}
	k := KubeView(sum, true)
	if got := k("a"); !got.Available || !got.Listed || !got.Ready {
		t.Errorf("a: %+v", got)
	}
	if got := k("b"); !got.Listed || got.Ready {
		t.Errorf("b: %+v", got)
	}
	if got := k("c"); !got.Available || got.Listed {
		t.Errorf("c (complete list, not in it): %+v", got)
	}
	if got := KubeView(sum, false)("a"); got.Available {
		t.Errorf("cluster agent offline: %+v", got)
	}
	if got := KubeView(nil, true)("a"); got.Available {
		t.Errorf("no summary: %+v", got)
	}
	// An older agent sends no list, a large cluster a cut one: a missing
	// node is then unknown, never "removed".
	old := &protocol.Summary{Nodes: protocol.NodeCounts{Total: 3}}
	if got := KubeView(old, true)("a"); got.Available {
		t.Errorf("no node list: %+v", got)
	}
	cut := &protocol.Summary{Nodes: protocol.NodeCounts{Total: 3}, NodeList: sum.NodeList}
	if got := KubeView(cut, true)("c"); got.Available {
		t.Errorf("cut list: %+v", got)
	}
	if got := KubeView(cut, true)("a"); !got.Available || !got.Ready {
		t.Errorf("listed in a cut list: %+v", got)
	}
	if n := Listed(sum, true); len(n) != 2 || Listed(sum, false) != nil {
		t.Errorf("listed: %v", n)
	}
}
