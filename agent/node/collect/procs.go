package collect

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"kubemap/pkg/protocol"
)

// topN is how many processes are picked by CPU and, separately, by memory.
// The union is at most 2*topN = protocol.MaxTopProcesses.
const topN = protocol.MaxTopProcesses / 2

var pageSize = uint64(os.Getpagesize())

// procKey tells a process apart from a later one that reuses its PID.
type procKey struct {
	pid   int
	start uint64 // start time in ticks since boot
}

type procSample struct {
	key   procKey
	ppid  int
	comm  string
	ticks uint64 // utime + stime
	rss   uint64 // bytes
	cpu   float64
}

// procTable keeps the CPU ticks of every process from the previous sample.
// It is rebuilt each sample, so it never holds more entries than the host
// has processes, and exited processes drop out.
type procTable struct {
	prev map[procKey]uint64
}

// topProcesses scans <proc>/<pid>/stat, computes each process's share of the
// node's CPU from totalDelta (the change of the aggregate /proc/stat "cpu"
// line since the previous sample), and returns the top processes by CPU and
// by memory with their origin. The first call has no previous ticks, so every
// cpuPercent is 0.
func (t *procTable) topProcesses(procRoot string, totalDelta uint64) []protocol.ProcessStats {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var all []procSample
	next := make(map[procKey]uint64, len(t.prev))
	var buf []byte
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		s, ok := readProcStat(filepath.Join(procRoot, e.Name(), "stat"), pid, &buf)
		if !ok {
			continue // exited while scanning, or unreadable
		}
		if before, seen := t.prev[s.key]; seen && totalDelta > 0 && s.ticks >= before {
			s.cpu = float64(s.ticks-before) / float64(totalDelta) * 100
		}
		next[s.key] = s.ticks
		all = append(all, s)
	}
	t.prev = next

	picked := pickTop(all)
	out := make([]protocol.ProcessStats, 0, len(picked))
	for _, s := range picked {
		p := protocol.ProcessStats{PID: s.key.pid, Command: s.comm, CPUPercent: s.cpu, RSSBytes: s.rss}
		cg, _ := readSmall(filepath.Join(procRoot, strconv.Itoa(s.key.pid), "cgroup"), &buf)
		classify(&p, s.ppid, string(cg))
		out = append(out, p)
	}
	return out
}

// pickTop returns the union of the top processes by CPU and by RSS, busiest
// first. Idle processes with no memory are never picked.
func pickTop(all []procSample) []procSample {
	chosen := map[procKey]bool{}
	var out []procSample
	add := func(less func(a, b *procSample) bool, keep func(s *procSample) bool) {
		sort.Slice(all, func(i, j int) bool { return less(&all[i], &all[j]) })
		n := 0
		for i := range all {
			if n == topN {
				break
			}
			if !keep(&all[i]) {
				break // sorted, so the rest are zero too
			}
			n++
			if !chosen[all[i].key] {
				chosen[all[i].key] = true
				out = append(out, all[i])
			}
		}
	}
	add(func(a, b *procSample) bool {
		if a.cpu != b.cpu {
			return a.cpu > b.cpu
		}
		return a.key.pid < b.key.pid
	}, func(s *procSample) bool { return s.cpu > 0 })
	add(func(a, b *procSample) bool {
		if a.rss != b.rss {
			return a.rss > b.rss
		}
		return a.key.pid < b.key.pid
	}, func(s *procSample) bool { return s.rss > 0 })
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].cpu != out[j].cpu {
			return out[i].cpu > out[j].cpu
		}
		return out[i].rss > out[j].rss
	})
	return out
}

// readProcStat parses /proc/<pid>/stat. The command name is in parentheses
// and may itself contain spaces or ")", so fields are counted from the last ")".
func readProcStat(path string, pid int, buf *[]byte) (procSample, bool) {
	b, err := readSmall(path, buf)
	if err != nil {
		return procSample{}, false
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return procSample{}, false
	}
	f := strings.Fields(string(b[end+1:]))
	// f[0] is field 3 (state); field N is f[N-3].
	if len(f) < 22 {
		return procSample{}, false
	}
	num := func(n int) uint64 { v, _ := strconv.ParseUint(f[n-3], 10, 64); return v }
	ppid, _ := strconv.Atoi(f[4-3])
	return procSample{
		key:   procKey{pid: pid, start: num(22)},
		ppid:  ppid,
		comm:  string(b[open+1 : end]),
		ticks: num(14) + num(15),
		rss:   num(24) * pageSize,
	}, true
}

var (
	// Pod cgroup segment: "pod<uid>" (cgroupfs driver) or
	// "kubepods-<qos>-pod<uid_with_underscores>.slice" (systemd driver).
	podRe = regexp.MustCompile(`pod([0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12})(\.slice)?$`)
	// Container segment: "cri-containerd-<id>.scope", "crio-<id>.scope",
	// "docker-<id>.scope", "libpod-<id>.scope", or a bare 64-hex id.
	containerRe = regexp.MustCompile(`^(?:[a-z-]+-)?([0-9a-f]{64})(?:\.scope)?$`)
)

// classify sets Origin, and PodUID, ContainerID or Unit, from the content
// of /proc/<pid>/cgroup. It only looks at path segments, so it also works
// when the node agent's own cgroup namespace makes paths start with "/..".
func classify(p *protocol.ProcessStats, ppid int, cgroup string) {
	if p.PID == 2 || ppid == 2 {
		p.Origin = protocol.OriginKernel
		return
	}
	path := cgroupPath(cgroup)
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if m := podRe.FindStringSubmatch(s); m != nil {
			p.PodUID = strings.ReplaceAll(m[1], "_", "-")
		}
		if m := containerRe.FindStringSubmatch(s); m != nil {
			p.ContainerID = m[1]
		}
	}
	switch {
	case p.PodUID != "":
		p.Origin = protocol.OriginPod
	case p.ContainerID != "":
		p.Origin = protocol.OriginContainer
	case strings.HasSuffix(segs[len(segs)-1], ".service"):
		p.Origin, p.Unit = protocol.OriginService, segs[len(segs)-1]
	case strings.Contains(path, "user.slice") || strings.Contains(path, "/session-"):
		p.Origin = protocol.OriginUser
	default:
		p.Origin = protocol.OriginHost
	}
}

// cgroupPath picks the path to classify from /proc/<pid>/cgroup: the first
// line that mentions a pod or container, else the cgroup v2 line ("0::"),
// else the first line.
func cgroupPath(content string) string {
	var v2, first string
	for line := range strings.SplitSeq(strings.TrimSpace(content), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if first == "" {
			first = path
		}
		if parts[0] == "0" && parts[1] == "" {
			v2 = path
		}
		if strings.Contains(path, "kubepods") || strings.Contains(path, "docker") ||
			strings.Contains(path, "containerd") || strings.Contains(path, "libpod") {
			return path
		}
	}
	if v2 != "" {
		return v2
	}
	return first
}
