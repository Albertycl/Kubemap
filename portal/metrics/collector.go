// Package metrics turns node agents' metrics messages into the node health
// data the portal shows (KM-30): the latest sample per node, kept in
// memory, and 1-minute averages stored as rollups for 24 hours.
package metrics

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

// Bucket is the rollup interval.
const Bucket = time.Minute

// Point is one node's health numbers. Percentages are 0-100; PSI values are
// the "some" avg10 share of time (0-100) that tasks waited.
type Point struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	Swap   float64 `json:"swap"`
	Disk   float64 `json:"disk"`
	Inodes float64 `json:"inodes"`
	PSICPU float64 `json:"psiCpu"`
	PSIMem float64 `json:"psiMemory"`
	PSIIO  float64 `json:"psiIo"`
}

// Sample is the latest metrics message from a node, with its derived Point.
type Sample struct {
	At      time.Time         `json:"at"` // when the portal received it
	Point   Point             `json:"point"`
	HasPSI  bool              `json:"hasPsi"`
	DiskFS  string            `json:"diskMountPoint,omitempty"` // the filesystem Disk and Inodes describe
	Metrics *protocol.Metrics `json:"metrics"`
}

// Collector implements tunnel.MetricsSink.
type Collector struct {
	store store.Store
	log   *slog.Logger
	now   func() time.Time

	mu     sync.Mutex
	latest map[key]*Sample
	acc    map[key]*accum
}

type key struct{ cluster, node string }

type accum struct {
	bucket time.Time
	sum    Point
	n      int
	nPSI   int // samples that carried PSI; the PSI sums count only those
}

// New returns a Collector. now may be nil (time.Now).
func New(st store.Store, log *slog.Logger, now func() time.Time) *Collector {
	if now == nil {
		now = time.Now
	}
	return &Collector{store: st, log: log, now: now, latest: map[key]*Sample{}, acc: map[key]*accum{}}
}

// Derive computes a node's Point from a metrics message. Disk and Inodes
// describe the filesystem mounted at "/", or else the fullest one.
func Derive(m *protocol.Metrics) (Point, bool, string) {
	p := Point{CPU: clamp(m.CPU.UsagePercent)}
	if t := m.Memory.TotalBytes; t > 0 {
		p.Memory = pct(t-min(m.Memory.AvailableBytes, t), t)
	}
	if t := m.Swap.TotalBytes; t > 0 {
		p.Swap = pct(min(m.Swap.UsedBytes, t), t)
	}
	var fs *protocol.FilesystemStats
	for i := range m.Filesystems {
		f := &m.Filesystems[i]
		if f.TotalBytes == 0 {
			continue
		}
		if f.MountPoint == "/" {
			fs = f
			break
		}
		if fs == nil || usedPct(f) > usedPct(fs) {
			fs = f
		}
	}
	mount := ""
	if fs != nil {
		mount = fs.MountPoint
		p.Disk = usedPct(fs)
		if fs.InodesTotal > 0 {
			p.Inodes = pct(fs.InodesTotal-min(fs.InodesFree, fs.InodesTotal), fs.InodesTotal)
		}
	}
	hasPSI := m.PSI != nil
	if hasPSI {
		p.PSICPU = clamp(m.PSI.CPU.Some.Avg10)
		p.PSIMem = clamp(m.PSI.Memory.Some.Avg10)
		p.PSIIO = clamp(m.PSI.IO.Some.Avg10)
	}
	return p, hasPSI, mount
}

func usedPct(f *protocol.FilesystemStats) float64 {
	return pct(f.TotalBytes-min(f.AvailableBytes, f.TotalBytes), f.TotalBytes)
}

func pct(part, total uint64) float64 { return clamp(float64(part) / float64(total) * 100) }

func clamp(v float64) float64 {
	switch {
	case v != v || v < 0: // NaN or negative
		return 0
	case v > 100:
		return 100
	}
	return v
}

// Observe records one metrics message (tunnel.MetricsSink). When a new
// minute starts, the previous minute's average is written as a rollup.
func (c *Collector) Observe(clusterID string, m *protocol.Metrics) {
	now := c.now()
	p, hasPSI, mount := Derive(m)
	k := key{clusterID, m.NodeName}
	b := now.Truncate(Bucket)

	c.mu.Lock()
	c.latest[k] = &Sample{At: now, Point: p, HasPSI: hasPSI, DiskFS: mount, Metrics: m}
	var done *accum
	a := c.acc[k]
	if a != nil && !a.bucket.Equal(b) {
		done, a = a, nil
	}
	if a == nil {
		a = &accum{bucket: b}
		c.acc[k] = a
	}
	a.add(p, hasPSI)
	c.mu.Unlock()

	if done != nil {
		c.write(context.Background(), []store.Rollup{done.rollup(k)})
	}
}

func (a *accum) add(p Point, hasPSI bool) {
	a.sum.CPU += p.CPU
	a.sum.Memory += p.Memory
	a.sum.Swap += p.Swap
	a.sum.Disk += p.Disk
	a.sum.Inodes += p.Inodes
	a.n++
	if hasPSI {
		a.sum.PSICPU += p.PSICPU
		a.sum.PSIMem += p.PSIMem
		a.sum.PSIIO += p.PSIIO
		a.nPSI++
	}
}

// rollup is the minute's average. PSI is averaged over the samples that
// had it; a minute without any has no PSI (stored as NULL), never 0 %.
func (a *accum) rollup(k key) store.Rollup {
	n := float64(a.n)
	r := store.Rollup{
		ClusterID: k.cluster, Node: k.node, Bucket: a.bucket,
		CPUPct: a.sum.CPU / n, MemPct: a.sum.Memory / n, SwapPct: a.sum.Swap / n,
		DiskPct: a.sum.Disk / n, InodesPct: a.sum.Inodes / n,
	}
	if a.nPSI > 0 {
		np := float64(a.nPSI)
		r.PSICPU, r.PSIMemory, r.PSIIO, r.HasPSI = a.sum.PSICPU/np, a.sum.PSIMem/np, a.sum.PSIIO/np, true
	}
	return r
}

func (c *Collector) write(ctx context.Context, rs []store.Rollup) {
	if err := c.store.WriteRollups(ctx, rs); err != nil {
		c.log.Error("write metric rollups", "count", len(rs), "err", err)
	}
}

// Flush writes every finished minute, including those of nodes that
// stopped reporting. Run calls it; tests call it directly.
func (c *Collector) Flush(ctx context.Context) {
	b := c.now().Truncate(Bucket)
	var rs []store.Rollup
	c.mu.Lock()
	for k, a := range c.acc {
		if a.bucket.Before(b) {
			rs = append(rs, a.rollup(k))
			delete(c.acc, k)
		}
	}
	c.mu.Unlock()
	if len(rs) > 0 {
		c.write(ctx, rs)
	}
}

// Run flushes finished minutes every 20 s and prunes rollups older than
// 24 h every hour, until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	flush := time.NewTicker(20 * time.Second)
	defer flush.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	c.prune(ctx)
	for {
		select {
		case <-ctx.Done():
			c.Flush(context.Background())
			return
		case <-flush.C:
			c.Flush(ctx)
		case <-prune.C:
			c.prune(ctx)
		}
	}
}

func (c *Collector) prune(ctx context.Context) {
	if n, err := c.store.PruneRollups(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("prune metric rollups", "err", err)
	} else if n > 0 {
		c.log.Info("pruned metric rollups", "deleted", n)
	}
}

// Reports returns the latest metrics message of every node agent of a
// cluster, sorted by node, for node.pressure and health.check: the cluster
// agent makes no kubelet calls and judges PSI from these (KM-60). Top
// processes and network counters are left out, as the protocol asks; the
// agent decides by each report's own time whether it is fresh enough.
func (c *Collector) Reports(clusterID string) []protocol.Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []protocol.Metrics
	for k, s := range c.latest {
		if k.cluster != clusterID || s.Metrics == nil {
			continue
		}
		m := *s.Metrics
		m.TopProcesses, m.Network = nil, nil
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeName < out[j].NodeName })
	if len(out) > protocol.MaxNodeReports {
		out = out[:protocol.MaxNodeReports]
	}
	return out
}

// Latest returns a node's latest sample.
func (c *Collector) Latest(clusterID, node string) (Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.latest[key{clusterID, node}]
	if s == nil {
		return Sample{}, false
	}
	return *s, true
}
