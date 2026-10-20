// Package collect reads one server's health from the host /proc and /sys and
// fills a protocol.Metrics message. It runs in the node agent, where the host
// root is mounted read-only at /host, so every root is configurable and tests
// run against fixture folders.
package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubemap/pkg/protocol"
)

// Config says where the host's filesystems are mounted.
type Config struct {
	ProcRoot string // host /proc; default /host/proc
	SysRoot  string // host /sys; default /host/sys
	HostRoot string // host /; default /host
	// NodeName goes into every sample. Default: the host's hostname.
	NodeName string
}

// Collector takes samples. CPU usage is a delta between two /proc/stat reads,
// so a Collector keeps the previous read. It is not safe for concurrent use.
type Collector struct {
	cfg    Config
	statfs func(path string) (fsUsage, error)
	devOf  func(path string) (string, error)
	now    func() time.Time
	// unseen are mount points already logged as not visible in the pod.
	unseen map[string]bool
	prev   *cpuTimes
	procs  procTable
}

// New returns a Collector and takes the first CPU reading, so the first
// Collect reports usage since New.
func New(cfg Config) (*Collector, error) {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/host/proc"
	}
	if cfg.SysRoot == "" {
		cfg.SysRoot = "/host/sys"
	}
	if cfg.HostRoot == "" {
		cfg.HostRoot = "/host"
	}
	c := &Collector{cfg: cfg, statfs: statfs, devOf: devOf, now: time.Now, unseen: map[string]bool{}}
	if c.cfg.NodeName == "" {
		c.cfg.NodeName = hostname(cfg.ProcRoot)
	}
	t, err := readCPUTimes(c.proc("stat"))
	if err != nil {
		return nil, err
	}
	c.prev = &t
	c.procs.topProcesses(c.cfg.ProcRoot, 0) // first process ticks, for CPU deltas
	return c, nil
}

func (c *Collector) proc(parts ...string) string {
	return filepath.Join(append([]string{c.cfg.ProcRoot}, parts...)...)
}

// Collect takes one sample. CPU, memory and load are required; PSI,
// filesystems, network and top processes are best effort, because a node without them is
// still worth reporting.
func (c *Collector) Collect() (*protocol.Metrics, error) {
	m := &protocol.Metrics{Time: c.now().UTC(), NodeName: c.cfg.NodeName}

	cur, err := readCPUTimes(c.proc("stat"))
	if err != nil {
		return nil, err
	}
	m.CPU.UsagePercent = cpuPercent(*c.prev, cur)
	var delta uint64
	if cur.total > c.prev.total {
		delta = cur.total - c.prev.total
	}
	c.prev = &cur
	m.CPU.Cores = cpuCount(filepath.Join(c.cfg.SysRoot, "devices/system/cpu/online"), cur.cores)

	if m.Load, err = readLoad(c.proc("loadavg")); err != nil {
		return nil, err
	}
	if m.Memory, m.Swap, err = readMeminfo(c.proc("meminfo")); err != nil {
		return nil, err
	}
	m.PSI = readPSI(c.proc("pressure"))
	m.Filesystems = c.filesystems()
	m.Network = readNetDev(c.proc("1/net/dev"), c.proc("net/dev"))
	m.TopProcesses = c.procs.topProcesses(c.cfg.ProcRoot, delta)
	return m, nil
}

// hostname prefers the host's hostname from /proc over the pod's own.
func hostname(procRoot string) string {
	if b, err := os.ReadFile(filepath.Join(procRoot, "sys/kernel/hostname")); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

// Percent returns part/total*100, or 0 when total is 0.
func Percent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// Brief is a one-line human summary of a sample, for logs and --once.
func Brief(m *protocol.Metrics) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: cpu %.1f%% of %d, load %.2f, mem used %.1f%%, swap used %.1f%%",
		m.NodeName, m.CPU.UsagePercent, m.CPU.Cores, m.Load.Load1,
		Percent(m.Memory.TotalBytes-m.Memory.AvailableBytes, m.Memory.TotalBytes),
		Percent(m.Swap.UsedBytes, m.Swap.TotalBytes))
	if m.PSI == nil {
		b.WriteString(", psi not available")
	} else {
		fmt.Fprintf(&b, ", psi some avg10 cpu %.1f%% mem %.1f%% io %.1f%%",
			m.PSI.CPU.Some.Avg10, m.PSI.Memory.Some.Avg10, m.PSI.IO.Some.Avg10)
	}
	for _, fs := range m.Filesystems {
		fmt.Fprintf(&b, ", disk %s %.1f%% (inodes %.1f%%)", fs.MountPoint,
			Percent(fs.UsedBytes, fs.TotalBytes), Percent(fs.InodesTotal-fs.InodesFree, fs.InodesTotal))
	}
	return b.String()
}
