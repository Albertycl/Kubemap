package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

// Cgroup lines as seen on the dev VM (k3s with the systemd cgroup driver,
// cgroup v2), with example IDs.
const (
	cgPod = "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod8126e47b_4f21_40f5_9fae_e2e0aee62dfb.slice/" +
		"cri-containerd-021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f.scope\n"
	cgDocker  = "0::/system.slice/docker-07e7aa8ff8d9a060f82887298122325b7c5e9c1873c6aade6366c3e29a602903.scope\n"
	cgK3s     = "0::/system.slice/k3s.service\n"
	cgSession = "0::/user.slice/user-1000.slice/session-6327.scope\n"
	cgRoot    = "0::/\n"
)

type fakeProc struct {
	pid, ppid    int
	comm         string
	utime, stime uint64
	start        uint64
	rssPages     uint64
	cgroup       string
}

// statLine builds a /proc/<pid>/stat line with the fields the collector reads.
func (p fakeProc) statLine() string {
	f := make([]string, 52) // fields 1..52
	for i := range f {
		f[i] = "0"
	}
	f[0] = fmt.Sprint(p.pid)
	f[1] = "(" + p.comm + ")"
	f[2] = "S"
	f[3] = fmt.Sprint(p.ppid)
	f[13] = fmt.Sprint(p.utime)
	f[14] = fmt.Sprint(p.stime)
	f[21] = fmt.Sprint(p.start)
	f[23] = fmt.Sprint(p.rssPages)
	return strings.Join(f, " ") + "\n"
}

func writeProcs(t *testing.T, root string, procs []fakeProc) {
	t.Helper()
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] >= '0' && e.Name()[0] <= '9' {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "stat"), []byte(p.statLine()), 0o644)
		os.WriteFile(filepath.Join(dir, "cgroup"), []byte(p.cgroup), 0o644)
	}
}

func byPID(ps []protocol.ProcessStats) map[int]protocol.ProcessStats {
	m := map[int]protocol.ProcessStats{}
	for _, p := range ps {
		m[p.PID] = p
	}
	return m
}

func TestTopProcessesCPUFromTwoSamples(t *testing.T) {
	root := t.TempDir()
	procs := []fakeProc{
		{pid: 100, ppid: 1, comm: "prometheus", utime: 1000, stime: 100, start: 5, rssPages: 100000, cgroup: cgPod},
		{pid: 200, ppid: 1, comm: "k3s-server", utime: 5000, stime: 500, start: 6, rssPages: 150000, cgroup: cgK3s},
		{pid: 300, ppid: 1, comm: "mariadbd", utime: 10, stime: 0, start: 7, rssPages: 20000, cgroup: cgDocker},
		{pid: 400, ppid: 2, comm: "kswapd0", utime: 0, stime: 50, start: 1, cgroup: cgRoot},
		{pid: 500, ppid: 1, comm: "sleep", start: 9, rssPages: 100, cgroup: cgSession},
	}
	writeProcs(t, root, procs)
	var tab procTable
	first := tab.topProcesses(root, 0)
	for _, p := range first {
		if p.CPUPercent != 0 {
			t.Errorf("first sample must have 0%% CPU: %+v", p)
		}
	}
	// 400 node ticks pass (for example 4 CPUs x 100 ticks).
	procs[0].utime += 80 // 20% of the node
	procs[0].stime += 20 // +20 -> 25% total
	procs[1].utime += 40 // 10%
	procs[3].stime += 4  // 1%
	writeProcs(t, root, procs)
	got := byPID(tab.topProcesses(root, 400))

	want := map[int]float64{100: 25, 200: 10, 300: 0, 400: 1, 500: 0}
	for pid, cpu := range want {
		if !near(got[pid].CPUPercent, cpu) {
			t.Errorf("pid %d cpu = %.2f, want %.2f", pid, got[pid].CPUPercent, cpu)
		}
	}
	if got[100].RSSBytes != 100000*pageSize {
		t.Errorf("rss = %d", got[100].RSSBytes)
	}
	// Busiest first.
	list := tab.topProcesses(root, 400) // no change since: all 0% now
	if len(list) == 0 {
		t.Fatal("empty list")
	}
	for _, p := range list {
		if p.CPUPercent != 0 {
			t.Errorf("no ticks since last sample, got %+v", p)
		}
	}
}

func TestTopProcessesPIDReuse(t *testing.T) {
	root := t.TempDir()
	p := fakeProc{pid: 100, ppid: 1, comm: "old", utime: 1000, start: 5, rssPages: 1, cgroup: cgK3s}
	writeProcs(t, root, []fakeProc{p})
	var tab procTable
	tab.topProcesses(root, 0)
	// Same PID, new process (different start time) with fewer ticks.
	writeProcs(t, root, []fakeProc{{pid: 100, ppid: 1, comm: "new", utime: 30, start: 99, rssPages: 1, cgroup: cgK3s}})
	got := tab.topProcesses(root, 400)
	if len(got) != 1 || got[0].CPUPercent != 0 || got[0].Command != "new" {
		t.Errorf("reused PID: %+v", got)
	}
	if len(tab.prev) != 1 {
		t.Errorf("table keeps %d entries, want 1 (exited processes dropped)", len(tab.prev))
	}
}

func TestTopProcessesLimit(t *testing.T) {
	root := t.TempDir()
	var procs []fakeProc
	for i := range 60 {
		procs = append(procs, fakeProc{pid: 1000 + i, ppid: 1, comm: "w", start: 1, utime: 0, rssPages: uint64(i + 1), cgroup: cgK3s})
	}
	writeProcs(t, root, procs)
	var tab procTable
	tab.topProcesses(root, 0)
	for i := range procs {
		procs[i].utime = uint64(60 - i) // CPU order is the reverse of memory order
	}
	writeProcs(t, root, procs)
	got := tab.topProcesses(root, 10000)
	if len(got) != protocol.MaxTopProcesses {
		t.Fatalf("got %d processes, want %d", len(got), protocol.MaxTopProcesses)
	}
	m := byPID(got)
	for i := range topN {
		if _, ok := m[1000+i]; !ok { // top CPU
			t.Errorf("top-CPU pid %d missing", 1000+i)
		}
		if _, ok := m[1059-i]; !ok { // top RSS
			t.Errorf("top-RSS pid %d missing", 1059-i)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].CPUPercent > got[i-1].CPUPercent {
			t.Errorf("not sorted by CPU at %d", i)
		}
	}
	// The whole message still fits and validates.
	if _, err := protocol.Encode("", &protocol.Metrics{Time: time.Now(), NodeName: "n", TopProcesses: got}); err != nil {
		t.Error(err)
	}
}

func TestIdleProcessesNotPicked(t *testing.T) {
	root := t.TempDir()
	writeProcs(t, root, []fakeProc{{pid: 7, ppid: 2, comm: "kworker/0:1", start: 1, cgroup: cgRoot}})
	var tab procTable
	if got := tab.topProcesses(root, 100); len(got) != 0 {
		t.Errorf("idle kernel thread with no memory picked: %+v", got)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		pid    int
		ppid   int
		cgroup string
		want   protocol.ProcessStats
	}{
		{"pod systemd driver", 10, 1, cgPod, protocol.ProcessStats{Origin: protocol.OriginPod,
			PodUID: "8126e47b-4f21-40f5-9fae-e2e0aee62dfb", ContainerID: "021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f"}},
		{"pod cgroupfs driver", 10, 1,
			"0::/kubepods/burstable/pod8126e47b-4f21-40f5-9fae-e2e0aee62dfb/021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f\n",
			protocol.ProcessStats{Origin: protocol.OriginPod, PodUID: "8126e47b-4f21-40f5-9fae-e2e0aee62dfb",
				ContainerID: "021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f"}},
		{"pod seen from the agent's own cgroup namespace", 10, 1,
			"0::/../../kubepods-besteffort-pod1e80f5cb_0860_4208_b2de_006858d432ed.slice/cri-containerd-155fdeb7ac811ec0485f88c1fb8d3da11fe01d5319bb392870017a5c26fd69bf.scope\n",
			protocol.ProcessStats{Origin: protocol.OriginPod, PodUID: "1e80f5cb-0860-4208-b2de-006858d432ed",
				ContainerID: "155fdeb7ac811ec0485f88c1fb8d3da11fe01d5319bb392870017a5c26fd69bf"}},
		{"pod pause, no container scope", 10, 1, "0::/kubepods.slice/kubepods-pod8126e47b_4f21_40f5_9fae_e2e0aee62dfb.slice\n",
			protocol.ProcessStats{Origin: protocol.OriginPod, PodUID: "8126e47b-4f21-40f5-9fae-e2e0aee62dfb"}},
		{"cgroup v1 lines", 10, 1,
			"12:cpu,cpuacct:/kubepods/besteffort/pod8126e47b-4f21-40f5-9fae-e2e0aee62dfb/021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f\n1:name=systemd:/kubepods/besteffort/pod8126e47b-4f21-40f5-9fae-e2e0aee62dfb/021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f\n",
			protocol.ProcessStats{Origin: protocol.OriginPod, PodUID: "8126e47b-4f21-40f5-9fae-e2e0aee62dfb",
				ContainerID: "021415e7716746bdecc87ef6c6b2de364df02a66b5e955ee8a3ca261061c633f"}},
		{"docker outside kubernetes", 10, 1, cgDocker, protocol.ProcessStats{Origin: protocol.OriginContainer,
			ContainerID: "07e7aa8ff8d9a060f82887298122325b7c5e9c1873c6aade6366c3e29a602903"}},
		{"docker cgroupfs", 10, 1, "0::/docker/07e7aa8ff8d9a060f82887298122325b7c5e9c1873c6aade6366c3e29a602903\n",
			protocol.ProcessStats{Origin: protocol.OriginContainer, ContainerID: "07e7aa8ff8d9a060f82887298122325b7c5e9c1873c6aade6366c3e29a602903"}},
		{"systemd service", 10, 1, cgK3s, protocol.ProcessStats{Origin: protocol.OriginService, Unit: "k3s.service"}},
		{"docker daemon", 10, 1, "0::/system.slice/docker.service\n", protocol.ProcessStats{Origin: protocol.OriginService, Unit: "docker.service"}},
		{"login session", 10, 1, cgSession, protocol.ProcessStats{Origin: protocol.OriginUser}},
		{"kernel thread", 50, 2, cgRoot, protocol.ProcessStats{Origin: protocol.OriginKernel}},
		{"kthreadd", 2, 0, cgRoot, protocol.ProcessStats{Origin: protocol.OriginKernel}},
		{"init", 1, 0, "0::/init.scope\n", protocol.ProcessStats{Origin: protocol.OriginHost}},
		{"unreadable cgroup", 10, 1, "", protocol.ProcessStats{Origin: protocol.OriginHost}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := protocol.ProcessStats{PID: tt.pid}
			classify(&got, tt.ppid, tt.cgroup)
			tt.want.PID = tt.pid
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestReadProcStatOddNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	p := fakeProc{pid: 42, ppid: 1, comm: "we ird) (name", utime: 7, stime: 3, start: 11, rssPages: 2}
	os.WriteFile(path, []byte(p.statLine()), 0o644)
	var buf []byte
	s, ok := readProcStat(path, 42, &buf)
	if !ok || s.comm != "we ird) (name" || s.ticks != 10 || s.key.start != 11 || s.rss != 2*pageSize || s.ppid != 1 {
		t.Errorf("parsed %+v, ok %v", s, ok)
	}
	os.WriteFile(path, []byte("42 (short) S 1 2 3\n"), 0o644)
	if _, ok := readProcStat(path, 42, &buf); ok {
		t.Error("short stat must fail")
	}
	os.WriteFile(path, []byte("garbage"), 0o644)
	if _, ok := readProcStat(path, 42, &buf); ok {
		t.Error("garbage must fail")
	}
	if _, ok := readProcStat(filepath.Join(dir, "none"), 42, &buf); ok {
		t.Error("missing file must fail")
	}
}

// A real stat line copied from the dev VM (PID 1).
func TestReadProcStatFixture(t *testing.T) {
	var buf []byte
	s, ok := readProcStat("testdata/procs/1/stat", 1, &buf)
	if !ok || s.comm != "systemd" || s.ticks != 7133+11381 || s.key.start != 102 || s.rss != 2258*pageSize {
		t.Errorf("parsed %+v, ok %v", s, ok)
	}
}

func TestTopProcessesRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	c, err := New(Config{ProcRoot: "/proc", SysRoot: "/sys", HostRoot: "/"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.TopProcesses) == 0 || len(m.TopProcesses) > protocol.MaxTopProcesses {
		t.Fatalf("got %d processes", len(m.TopProcesses))
	}
	var sum float64
	for _, p := range m.TopProcesses {
		if p.Origin == "" || p.Command == "" {
			t.Errorf("incomplete: %+v", p)
		}
		sum += p.CPUPercent
	}
	// Processes and the node total are read at slightly different moments;
	// on a loaded machine the shares can add up to a little over 100 %.
	if why := busy(); why != "" {
		t.Logf("sum %.1f%%; not checked: %s", sum, why)
		return
	}
	if sum > 100.5 {
		t.Errorf("top processes use %.1f%% of the node, more than 100%%", sum)
	}
}

func TestProcRootMissing(t *testing.T) {
	var tab procTable
	if got := tab.topProcesses(filepath.Join(t.TempDir(), "none"), 1); got != nil {
		t.Errorf("got %+v", got)
	}
}
