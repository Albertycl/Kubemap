package collect

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kubemap/pkg/protocol"
)

// maxLine bounds every line read from /proc. /proc/stat's "intr" line can be
// many KiB on big machines; lines longer than this are skipped, not buffered.
const maxLine = 64 << 10

// scanLines calls fn for each line of path that fits in maxLine.
func scanLines(path string, fn func(line string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4096)
	var buf []byte
	skip := false
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !skip {
			buf = append(buf, chunk...)
			if len(buf) > maxLine {
				buf, skip = buf[:0], true
			}
		}
		if isPrefix {
			continue
		}
		if !skip {
			if err := fn(string(buf)); err != nil {
				return err
			}
		}
		buf, skip = buf[:0], false
	}
}

type cpuTimes struct {
	total, idle uint64
	cores       int // "cpuN" lines
}

// readCPUTimes reads the aggregate "cpu" line of /proc/stat.
func readCPUTimes(path string) (cpuTimes, error) {
	var t cpuTimes
	found := false
	err := scanLines(path, func(line string) error {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			return nil
		}
		if f[0] != "cpu" {
			t.cores++
			return nil
		}
		// user nice system idle iowait irq softirq steal guest guest_nice.
		// guest and guest_nice are already counted in user and nice.
		if len(f) < 5 {
			return fmt.Errorf("%s: short cpu line", path)
		}
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			t.total += v
			if i == 4 || i == 5 { // idle, iowait
				t.idle += v
			}
		}
		found = true
		return nil
	})
	if err == nil && !found {
		err = fmt.Errorf("%s: no cpu line", path)
	}
	return t, err
}

// cpuPercent is the busy share of CPU time between two readings.
func cpuPercent(a, b cpuTimes) float64 {
	if b.total <= a.total || b.idle < a.idle {
		return 0 // no time passed, or counters reset
	}
	dt, di := b.total-a.total, b.idle-a.idle
	if di > dt {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

// cpuCount reads /sys/devices/system/cpu/online ("0-3,6"), falling back to
// the number of cpuN lines in /proc/stat.
func cpuCount(onlinePath string, fallback int) int {
	b, err := os.ReadFile(onlinePath)
	if err != nil {
		return fallback
	}
	n := 0
	for part := range strings.SplitSeq(strings.TrimSpace(string(b)), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		l, err1 := strconv.Atoi(lo)
		h := l
		var err2 error
		if isRange {
			h, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || h < l {
			return fallback
		}
		n += h - l + 1
	}
	if n == 0 {
		return fallback
	}
	return n
}

func readLoad(path string) (protocol.LoadStats, error) {
	var l protocol.LoadStats
	b, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return l, fmt.Errorf("%s: short file", path)
	}
	vals := []*float64{&l.Load1, &l.Load5, &l.Load15}
	for i, p := range vals {
		if *p, err = strconv.ParseFloat(f[i], 64); err != nil {
			return l, fmt.Errorf("%s: %w", path, err)
		}
	}
	return l, nil
}

func readMeminfo(path string) (protocol.MemoryStats, protocol.SwapStats, error) {
	var mem protocol.MemoryStats
	var swap protocol.SwapStats
	var swapFree uint64
	want := map[string]*uint64{
		"MemTotal": &mem.TotalBytes, "MemAvailable": &mem.AvailableBytes,
		"SwapTotal": &swap.TotalBytes, "SwapFree": &swapFree,
	}
	seen := 0
	err := scanLines(path, func(line string) error {
		key, rest, ok := strings.Cut(line, ":")
		p := want[key]
		if !ok || p == nil {
			return nil
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return fmt.Errorf("%s: empty %s", path, key)
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		*p = v
		seen++
		return nil
	})
	if err != nil {
		return mem, swap, err
	}
	if seen < len(want) {
		return mem, swap, fmt.Errorf("%s: missing MemTotal, MemAvailable, SwapTotal or SwapFree", path)
	}
	if swapFree <= swap.TotalBytes {
		swap.UsedBytes = swap.TotalBytes - swapFree
	}
	return mem, swap, nil
}

// readPSI reads /proc/pressure/{cpu,memory,io}. It returns nil when PSI is not
// available (kernel without CONFIG_PSI, or booted with psi=0), so the portal
// shows "not available" instead of a false zero.
func readPSI(dir string) *protocol.PSIStats {
	var s protocol.PSIStats
	for name, p := range map[string]*protocol.Pressure{"cpu": &s.CPU, "memory": &s.Memory, "io": &s.IO} {
		v, err := readPressure(filepath.Join(dir, name))
		if err != nil {
			return nil
		}
		*p = v
	}
	return &s
}

// readPressure parses lines like
// "some avg10=30.68 avg60=27.32 avg300=23.02 total=228258513523".
func readPressure(path string) (protocol.Pressure, error) {
	var p protocol.Pressure
	gotSome := false
	err := scanLines(path, func(line string) error {
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil
		}
		var avg protocol.PressureAvg
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			dst := map[string]*float64{"avg10": &avg.Avg10, "avg60": &avg.Avg60, "avg300": &avg.Avg300}[k]
			if dst == nil {
				continue
			}
			x, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			*dst = x
		}
		switch f[0] {
		case "some":
			p.Some, gotSome = avg, true
		case "full":
			p.Full = &avg
		}
		return nil
	})
	if err == nil && !gotSome {
		err = errors.New(path + ": no some line")
	}
	return p, err
}

// skipIface marks pod-side and loopback interfaces. A node with 100 pods has
// 100 veth or cali interfaces; their traffic is per-pod, not per-node.
func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "cali", "lxc", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// readNetDev reads the first readable path. The node agent prefers
// /proc/1/net/dev, the host's network namespace; /proc/net/dev would show
// the pod's own namespace unless the pod uses hostNetwork.
func readNetDev(paths ...string) []protocol.NetworkStats {
	for _, path := range paths {
		var out []protocol.NetworkStats
		err := scanLines(path, func(line string) error {
			name, rest, ok := strings.Cut(line, ":")
			name = strings.TrimSpace(name)
			if !ok || skipIface(name) || strings.Contains(name, "|") {
				return nil
			}
			f := strings.Fields(rest)
			if len(f) < 16 {
				return nil
			}
			n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
			out = append(out, protocol.NetworkStats{
				Interface: name, RxBytes: n(0), RxErrors: n(2), TxBytes: n(8), TxErrors: n(10),
			})
			return nil
		})
		if err == nil {
			return out
		}
	}
	return nil
}
