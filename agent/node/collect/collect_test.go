package collect

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

// Fixtures in testdata/vm were copied from the dev VM (k8server-like, 4 CPUs,
// 12 GiB) and trimmed. Mount paths of pods and PVCs are replaced by examples.

const vmProc = "testdata/vm/proc"

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// fakeStatfs answers statfs for known host paths and fails for the rest.
func fakeStatfs(m map[string]fsUsage) func(string) (fsUsage, error) {
	return func(p string) (fsUsage, error) {
		if u, ok := m[p]; ok {
			return u, nil
		}
		return fsUsage{}, errors.New("no such mount")
	}
}

func newTestCollector(t *testing.T, proc string) *Collector {
	t.Helper()
	c, err := New(Config{ProcRoot: proc, SysRoot: "testdata/vm/sys", HostRoot: "/host"})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }
	c.statfs = fakeStatfs(map[string]fsUsage{
		"/host":      {total: 100 << 30, free: 19 << 30, avail: 14 << 30, inodesTotal: 1000, inodesFree: 750},
		"/host/boot": {total: 2 << 30, free: 1 << 30, avail: 1 << 30, inodesTotal: 100, inodesFree: 99},
	})
	return c
}

func TestCPUPercentFromTwoSamples(t *testing.T) {
	a, err := readCPUTimes(filepath.Join(vmProc, "stat.1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := readCPUTimes(filepath.Join(vmProc, "stat.2"))
	if err != nil {
		t.Fatal(err)
	}
	if a.cores != 4 {
		t.Errorf("cores = %d, want 4", a.cores)
	}
	// Expected value computed by hand from the two fixture lines:
	// busy = Δ(user+nice+system+irq+softirq+steal), total = busy + Δ(idle+iowait).
	got := cpuPercent(a, b)
	want := float64((b.total-b.idle)-(a.total-a.idle)) / float64(b.total-a.total) * 100
	if !near(got, want) || got <= 0 || got >= 100 {
		t.Errorf("cpuPercent = %.2f, want %.2f (between 0 and 100)", got, want)
	}
	if cpuPercent(b, a) != 0 || cpuPercent(a, a) != 0 {
		t.Error("no elapsed time or reset counters must give 0")
	}
}

func TestCPUPercentKnownValues(t *testing.T) {
	a := cpuTimes{total: 1000, idle: 800}
	b := cpuTimes{total: 1400, idle: 900} // 400 ticks, 100 idle -> 75% busy
	if got := cpuPercent(a, b); !near(got, 75) {
		t.Errorf("got %.2f, want 75", got)
	}
}

func TestCollectorCPUUsesPreviousSample(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(vmProc, "stat.1"), filepath.Join(dir, "stat"))
	for _, f := range []string{"meminfo", "loadavg"} {
		copyFile(t, filepath.Join(vmProc, f), filepath.Join(dir, f))
	}
	c := newTestCollector(t, dir)
	copyFile(t, filepath.Join(vmProc, "stat.2"), filepath.Join(dir, "stat"))
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := readCPUTimes(filepath.Join(vmProc, "stat.1"))
	b, _ := readCPUTimes(filepath.Join(vmProc, "stat.2"))
	if !near(m.CPU.UsagePercent, cpuPercent(a, b)) {
		t.Errorf("usage = %.2f, want %.2f", m.CPU.UsagePercent, cpuPercent(a, b))
	}
	// No PSI, mountinfo or net/dev in this folder: those parts are empty,
	// the sample still works.
	if m.PSI != nil || m.Filesystems != nil || m.Network != nil {
		t.Errorf("expected best-effort parts to be empty: %+v", m)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAndSwap(t *testing.T) {
	mem, swap, err := readMeminfo(filepath.Join(vmProc, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if mem.TotalBytes == 0 || mem.AvailableBytes == 0 || mem.AvailableBytes > mem.TotalBytes {
		t.Errorf("memory = %+v", mem)
	}
	// Fixture: SwapTotal 4194300 kB, SwapFree small -> swap almost full.
	if swap.TotalBytes != 4194300*1024 {
		t.Errorf("swap total = %d", swap.TotalBytes)
	}
	if p := Percent(swap.UsedBytes, swap.TotalBytes); p < 99 || p > 100 {
		t.Errorf("swap used = %.2f%%, want about 100%%", p)
	}
}

func TestSwapUsedPercent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	os.WriteFile(path, []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 2000 kB\nSwapFree: 500 kB\n"), 0o644)
	mem, swap, err := readMeminfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := Percent(swap.UsedBytes, swap.TotalBytes); !near(p, 75) {
		t.Errorf("swap used %% = %.2f, want 75", p)
	}
	if p := Percent(mem.TotalBytes-mem.AvailableBytes, mem.TotalBytes); !near(p, 75) {
		t.Errorf("mem used %% = %.2f, want 75", p)
	}
	// No swap configured: 0%, not a division by zero.
	os.WriteFile(path, []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), 0o644)
	_, swap, _ = readMeminfo(path)
	if Percent(swap.UsedBytes, swap.TotalBytes) != 0 {
		t.Error("no swap must be 0%")
	}
	os.WriteFile(path, []byte("MemTotal: 1000 kB\n"), 0o644)
	if _, _, err := readMeminfo(path); err == nil {
		t.Error("missing fields must fail")
	}
}

func TestPSIParsing(t *testing.T) {
	psi := readPSI(filepath.Join(vmProc, "pressure"))
	if psi == nil {
		t.Fatal("PSI is nil")
	}
	p, err := readPressure(filepath.Join(vmProc, "pressure", "cpu"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Some != psi.CPU.Some || psi.Memory.Full == nil || psi.IO.Full == nil {
		t.Errorf("psi = %+v, cpu file = %+v", psi, p)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "io")
	os.WriteFile(path, []byte("some avg10=30.68 avg60=27.32 avg300=23.02 total=228258513523\nfull avg10=1.50 avg60=0.25 avg300=0.00 total=5\n"), 0o644)
	p, err = readPressure(path)
	if err != nil {
		t.Fatal(err)
	}
	if !near(p.Some.Avg10, 30.68) || !near(p.Some.Avg60, 27.32) || !near(p.Some.Avg300, 23.02) {
		t.Errorf("some = %+v", p.Some)
	}
	if p.Full == nil || !near(p.Full.Avg10, 1.5) || !near(p.Full.Avg60, 0.25) {
		t.Errorf("full = %+v", p.Full)
	}
	// Older kernels have no "full" line for cpu.
	os.WriteFile(path, []byte("some avg10=1.00 avg60=2.00 avg300=3.00 total=1\n"), 0o644)
	if p, err = readPressure(path); err != nil || p.Full != nil {
		t.Errorf("some-only: %+v, %v", p, err)
	}
	os.WriteFile(path, []byte("some avg10=x avg60=2.00 avg300=3.00 total=1\n"), 0o644)
	if _, err = readPressure(path); err == nil {
		t.Error("bad number must fail")
	}
}

func TestPSIMissing(t *testing.T) {
	if readPSI("testdata/nopsi/proc/pressure") != nil {
		t.Error("missing /proc/pressure must give nil (not available), not zeros")
	}
	c := newTestCollector(t, "testdata/nopsi/proc")
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if m.PSI != nil {
		t.Error("PSI must be nil")
	}
	data, err := protocol.Encode("", m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"psi"`) {
		t.Errorf("psi must be absent from JSON: %s", data)
	}
	if !strings.Contains(Brief(m), "psi not available") {
		t.Errorf("brief: %s", Brief(m))
	}
}

func TestFilesystemFiltering(t *testing.T) {
	mounts, err := parseMountinfo(filepath.Join(vmProc, "1/mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) < 20 {
		t.Fatalf("parsed %d mounts", len(mounts))
	}
	var got []string
	for _, m := range realMounts(mounts) {
		got = append(got, m.mountPoint+" "+m.fsType)
	}
	// tmpfs, overlay, squashfs, proc, sysfs, nfs4 and the other pseudo
	// filesystems are gone; the pod bind mounts of the root disk collapse into "/".
	want := []string{"/ ext4", "/boot ext4", "/mnt/data disk xfs"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("real mounts = %q, want %q", got, want)
	}
}

func TestFilesystemsAndInodePercent(t *testing.T) {
	c := newTestCollector(t, vmProc)
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// /mnt/data disk fails statfs in the fake and is skipped.
	if len(m.Filesystems) != 2 {
		t.Fatalf("filesystems = %+v", m.Filesystems)
	}
	root := m.Filesystems[0]
	if root.MountPoint != "/" || root.Device != "/dev/mapper/ubuntu--vg-ubuntu--lv" {
		t.Errorf("root = %+v", root)
	}
	if p := Percent(root.UsedBytes, root.TotalBytes); !near(p, 81) {
		t.Errorf("disk used = %.2f%%, want 81", p)
	}
	if root.AvailableBytes != 14<<30 {
		t.Errorf("available = %d", root.AvailableBytes)
	}
	if p := Percent(root.InodesTotal-root.InodesFree, root.InodesTotal); !near(p, 25) {
		t.Errorf("inodes used = %.2f%%, want 25", p)
	}
	boot := m.Filesystems[1]
	if p := Percent(boot.InodesTotal-boot.InodesFree, boot.InodesTotal); !near(p, 1) {
		t.Errorf("boot inodes = %.2f%%, want 1", p)
	}
}

func TestStatfsReal(t *testing.T) {
	u, err := statfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.total == 0 || u.free > u.total || u.avail > u.free {
		t.Errorf("statfs = %+v", u)
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{`/a\040b`: "/a b", `/plain`: "/plain", `/x\`: `/x\`, `/t\011x`: "/t\tx", `/bad\09x`: `/bad\09x`} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNetwork(t *testing.T) {
	n := readNetDev(filepath.Join(vmProc, "missing"), filepath.Join(vmProc, "1/net/dev"))
	var names []string
	for _, s := range n {
		names = append(names, s.Interface)
		if s.RxBytes == 0 && s.TxBytes == 0 && s.Interface == "ens33" {
			t.Errorf("ens33 has no traffic: %+v", s)
		}
	}
	// lo, veth* and cali* are skipped.
	if strings.Join(names, ",") != "ens33,docker0,vxlan.calico" {
		t.Errorf("interfaces = %v", names)
	}
	if readNetDev("testdata/none") != nil {
		t.Error("unreadable net/dev must give nil")
	}
}

func TestLoadAndCPUCount(t *testing.T) {
	l, err := readLoad(filepath.Join(vmProc, "loadavg"))
	if err != nil || l.Load1 <= 0 {
		t.Errorf("load = %+v, %v", l, err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "online")
	for in, want := range map[string]int{"0-3\n": 4, "0-3,6,8-9": 7, "0": 1, "": 9, "x": 9, "3-1": 9} {
		os.WriteFile(p, []byte(in), 0o644)
		if got := cpuCount(p, 9); got != want {
			t.Errorf("cpuCount(%q) = %d, want %d", in, got, want)
		}
	}
	if cpuCount(filepath.Join(dir, "none"), 9) != 9 {
		t.Error("missing file must use fallback")
	}
	os.WriteFile(p, []byte("1.0 2.0\n"), 0o644)
	if _, err := readLoad(p); err == nil {
		t.Error("short loadavg must fail")
	}
}

func TestCollectRequiredFilesMissing(t *testing.T) {
	if _, err := New(Config{ProcRoot: t.TempDir()}); err == nil {
		t.Error("New without /proc/stat must fail")
	}
	dir := t.TempDir()
	copyFile(t, filepath.Join(vmProc, "stat"), filepath.Join(dir, "stat"))
	c := newTestCollector(t, dir)
	if _, err := c.Collect(); err == nil {
		t.Error("Collect without loadavg must fail")
	}
	copyFile(t, filepath.Join(vmProc, "loadavg"), filepath.Join(dir, "loadavg"))
	if _, err := c.Collect(); err == nil {
		t.Error("Collect without meminfo must fail")
	}
	os.Remove(filepath.Join(dir, "stat"))
	if _, err := c.Collect(); err == nil {
		t.Error("Collect without stat must fail")
	}
}

func TestLongLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stat")
	long := "intr " + strings.Repeat("1 ", maxLine)
	os.WriteFile(p, []byte("cpu  1 2 3 4 5 6 7 8 0 0\n"+long+"\ncpu0 1 2 3 4 5 6 7 8 0 0\n"), 0o644)
	ct, err := readCPUTimes(p)
	if err != nil || ct.total != 36 || ct.idle != 9 || ct.cores != 1 {
		t.Errorf("cpu = %+v, %v", ct, err)
	}
}

func TestSampleIsValidMetrics(t *testing.T) {
	c := newTestCollector(t, vmProc)
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if m.NodeName != "k8test" || m.CPU.Cores != 4 || m.PSI == nil {
		t.Errorf("sample = %+v", m)
	}
	if _, err := protocol.Encode("", m); err != nil {
		t.Fatalf("sample does not encode: %v", err)
	}
	b := Brief(m)
	for _, s := range []string{"k8test", "swap used", "disk / 81.0%", "inodes 25.0%", "psi some"} {
		if !strings.Contains(b, s) {
			t.Errorf("brief %q lacks %q", b, s)
		}
	}
}

// TestSampleSpeed runs against the real /proc of the machine running the
// tests; acceptance: one sample under 50 ms.
func TestSampleSpeed(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	c, err := New(Config{ProcRoot: "/proc", SysRoot: "/sys", HostRoot: "/"})
	if err != nil {
		t.Fatal(err)
	}
	// Median of 10 samples, so one slow moment on a busy VM does not fail it.
	var took []time.Duration
	for range 10 {
		start := time.Now()
		if _, err := c.Collect(); err != nil {
			t.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	slices.Sort(took)
	if why := busy(); why != "" {
		t.Logf("median %v; timing not checked: %s", took[len(took)/2], why)
		return
	}
	if med := took[len(took)/2]; med > 50*time.Millisecond {
		t.Errorf("median sample took %v, limit 50ms (all: %v)", med, took)
	}
}

// busy says why timing assertions on the real machine should be skipped:
// -short, or a 1-minute load average above the CPU count, when a busy VM
// (parallel builds) makes wall-clock limits and CPU sampling unreliable.
// Functional checks still run. It returns "" when timings are meaningful.
func busy() string {
	if testing.Short() {
		return "-short"
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if load, err := strconv.ParseFloat(f[0], 64); err == nil && load > float64(runtime.NumCPU()) {
				return fmt.Sprintf("load average %.1f on %d CPUs", load, runtime.NumCPU())
			}
		}
	}
	return ""
}

func BenchmarkCollect(b *testing.B) {
	c, err := New(Config{ProcRoot: "/proc", SysRoot: "/sys", HostRoot: "/"})
	if err != nil {
		b.Skip(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Collect(); err != nil {
			b.Fatal(err)
		}
	}
}

// review-km-44 L5: with no mount propagation, a disk mounted after the pod
// started shows up under HostRoot as a plain directory of its parent. Its
// device differs from the mount's, and it must be left out, not reported
// with the parent's numbers. Once the pod sees it (after a restart), it is
// measured again.
func TestMountNotVisibleInPodIsSkipped(t *testing.T) {
	c := newTestCollector(t, vmProc)
	devs := map[string]string{"/host": "252:0", "/host/boot": "252:0"} // /boot not propagated: the root's device
	c.devOf = func(p string) (string, error) {
		if d, ok := devs[p]; ok {
			return d, nil
		}
		return "", errors.New("no such file")
	}
	m, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Filesystems) != 1 || m.Filesystems[0].MountPoint != "/" || !c.unseen["/boot"] {
		t.Fatalf("an unseen mount must be skipped: %+v (unseen %v)", m.Filesystems, c.unseen)
	}
	devs["/host/boot"] = "8:2" // the pod restarted and sees the disk
	if m, _ = c.Collect(); len(m.Filesystems) != 2 || c.unseen["/boot"] {
		t.Fatalf("a visible mount must be measured: %+v", m.Filesystems)
	}
}

func TestDevOf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	d, err := devOf(t.TempDir())
	if err != nil || !strings.Contains(d, ":") {
		t.Fatalf("devOf = %q, %v", d, err)
	}
	// The same filesystem as its parent directory.
	if p, _ := devOf(filepath.Dir(t.TempDir())); p != d {
		t.Errorf("devOf differs for the same filesystem: %s vs %s", d, p)
	}
}
