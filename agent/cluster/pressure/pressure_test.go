package pressure

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemap/pkg/protocol"
)

var since = metav1.NewTime(time.Date(2026, 9, 28, 7, 32, 0, 0, time.UTC))

func node(name string, conds ...corev1.NodeCondition) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Status.Conditions = append([]corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, conds...)
	return n
}

func allClear(name string) *corev1.Node {
	return node(name,
		corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastTransitionTime: since},
		corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, LastTransitionTime: since},
		corev1.NodeCondition{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, LastTransitionTime: since})
}

var now = time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)

func psi(some, full float64) *protocol.Pressure {
	return &protocol.Pressure{Some: protocol.PressureAvg{Avg10: some, Avg60: some, Avg300: some}, Full: &protocol.PressureAvg{Avg60: full}}
}

// report is a node agent metrics message taken 10 s ago; a nil PSI part is
// all zero, as the kernel reports an idle resource.
func report(cpu, mem, io *protocol.Pressure) *protocol.Metrics {
	calm := protocol.Pressure{Full: &protocol.PressureAvg{}}
	p := &protocol.PSIStats{CPU: calm, Memory: calm, IO: calm}
	for dst, src := range map[*protocol.Pressure]*protocol.Pressure{&p.CPU: cpu, &p.Memory: mem, &p.IO: io} {
		if src != nil {
			*dst = *src
		}
	}
	return &protocol.Metrics{NodeName: "k8server", Time: now.Add(-10 * time.Second), PSI: p}
}

func res(np protocol.NodePressure, name string) protocol.ResourcePressure {
	for _, r := range np.Resources {
		if r.Resource == name {
			return r
		}
	}
	return protocol.ResourcePressure{}
}

// The k8server case from the board: all conditions False, CPU some 27%.
func TestCPUPressureKubernetesCannotSee(t *testing.T) {
	np := Evaluate(allClear("k8server"), report(psi(27.26, 0), psi(0.1, 0), psi(0.2, 0.1)))
	cpu := res(np, "cpu")
	if cpu.Verdict != protocol.VerdictPressure || cpu.Condition != "" || !strings.Contains(cpu.Detail, "no cpu pressure condition") {
		t.Errorf("cpu = %+v", cpu)
	}
	if np.Verdict != protocol.VerdictPressure || !np.PSIAvailable || !strings.Contains(np.Summary, "cpu") {
		t.Errorf("node = %s %q", np.Verdict, np.Summary)
	}
	if m := res(np, "memory"); m.Verdict != protocol.VerdictOK || m.ConditionStatus != "False" || m.PSI == nil {
		t.Errorf("memory = %+v", m)
	}
}

func TestThresholds(t *testing.T) {
	for _, c := range []struct {
		name      string
		some, all float64
		want      string
	}{
		{"cpu", 9.9, 0, protocol.VerdictOK},
		{"cpu", 10, 0, protocol.VerdictElevated},
		{"cpu", 25, 0, protocol.VerdictPressure},
		{"cpu", 1, 50, protocol.VerdictOK}, // node-level cpu "full" is ignored
		{"memory", 1, 5, protocol.VerdictPressure},
		{"memory", 12, 4.9, protocol.VerdictElevated},
		{"io", 1, 5, protocol.VerdictPressure},
	} {
		var s *protocol.Metrics
		switch c.name {
		case "cpu":
			s = report(psi(c.some, c.all), nil, nil)
		case "memory":
			s = report(nil, psi(c.some, c.all), nil)
		case "io":
			s = report(nil, nil, psi(c.some, c.all))
		}
		if got := res(Evaluate(allClear("n"), s), c.name).Verdict; got != c.want {
			t.Errorf("%s some %.1f full %.1f: %s, want %s", c.name, c.some, c.all, got, c.want)
		}
	}
}

// A True condition wins over calm PSI: the kubelet is already evicting.
func TestConditionTrueWins(t *testing.T) {
	n := node("k8server", corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue, LastTransitionTime: since,
		Message: "ephemeral-storage"}, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, LastTransitionTime: since})
	np := Evaluate(n, report(psi(0, 0), psi(0, 0), psi(0, 0)))
	d := res(np, "disk")
	if d.Verdict != protocol.VerdictPressure || !strings.Contains(d.Detail, "2026-09-28T07:32:00Z") || !strings.Contains(d.Detail, "minimum-reclaim") {
		t.Errorf("disk = %+v", d)
	}
	if res(np, "memory").Verdict != protocol.VerdictPressure || np.Verdict != protocol.VerdictPressure {
		t.Errorf("memory = %+v", res(np, "memory"))
	}
}

// Without PSI, CPU and I/O are "not available", never "ok"; memory falls
// back to its condition.
func TestNoPSI(t *testing.T) {
	noPSI := report(nil, nil, nil)
	noPSI.PSI = nil // kernel without PSI
	for _, s := range []*protocol.Metrics{nil, noPSI} {
		np := Evaluate(allClear("old"), s)
		if np.PSIAvailable {
			t.Error("PSI available")
		}
		for _, name := range []string{"cpu", "io"} {
			if r := res(np, name); r.Verdict != protocol.VerdictUnknown || r.PSI != nil || !strings.Contains(r.Detail, "not available") {
				t.Errorf("%s = %+v", name, r)
			}
		}
		if m := res(np, "memory"); m.Verdict != protocol.VerdictOK || !strings.Contains(m.Detail, "PSI not available") {
			t.Errorf("memory = %+v", m)
		}
		if np.Verdict != protocol.VerdictUnknown || !strings.Contains(np.Summary, "PSI not available") {
			t.Errorf("node = %s %q", np.Verdict, np.Summary)
		}
	}
}

func TestNodeNotReady(t *testing.T) {
	n := node("gone", corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionUnknown})
	n.Status.Conditions[0].Status = corev1.ConditionUnknown
	np := Evaluate(n, nil)
	if np.Ready || res(np, "memory").Verdict != protocol.VerdictUnknown || !strings.HasPrefix(np.Summary, "node not Ready") {
		t.Errorf("np = %+v", np)
	}
	if d := res(np, "disk"); d.Verdict != protocol.VerdictUnknown || !strings.Contains(d.Detail, "no DiskPressure condition") {
		t.Errorf("disk = %+v", d)
	}
}

// Reports come from the portal; a missing or stale one gives a note.
func TestReport(t *testing.T) {
	b := report(psi(30, 0), psi(0, 0), psi(0, 0))
	b.NodeName = "b"
	stale := report(psi(90, 0), nil, nil)
	stale.NodeName, stale.Time = "c", now.Add(-5*time.Minute)
	rep := Report([]*corev1.Node{allClear("c"), allClear("b"), allClear("a")}, Reports([]protocol.Metrics{*b, *stale}), now)
	if len(rep.Nodes) != 3 || rep.Nodes[0].Node != "a" || rep.Time != now {
		t.Fatalf("report = %+v", rep)
	}
	a, bb, c := rep.Nodes[0], rep.Nodes[1], rep.Nodes[2]
	if a.PSIAvailable || !a.ReportTime.IsZero() || res(a, "cpu").Verdict != protocol.VerdictUnknown {
		t.Errorf("no report: %+v", a)
	}
	if !bb.PSIAvailable || bb.Verdict != protocol.VerdictPressure || !bb.ReportTime.Equal(now.Add(-10*time.Second)) {
		t.Errorf("fresh report: %+v", bb)
	}
	if c.PSIAvailable || c.Verdict == protocol.VerdictPressure {
		t.Errorf("a 5-minute-old report must not be used: %+v", c)
	}
	if len(rep.Notes) != 2 || !strings.Contains(rep.Notes[0], "node a: no node agent report") || !strings.Contains(rep.Notes[1], "node c: the node agent report is 5m0s old") {
		t.Errorf("notes = %v", rep.Notes)
	}
}
