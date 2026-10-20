package metrics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

const gib = 1 << 30

func sample(cpu float64) *protocol.Metrics {
	return &protocol.Metrics{
		Time: time.Now(), NodeName: "k8server",
		CPU:    protocol.CPUStats{Cores: 4, UsagePercent: cpu},
		Memory: protocol.MemoryStats{TotalBytes: 12 * gib, AvailableBytes: 3 * gib},
		Swap:   protocol.SwapStats{TotalBytes: 4 * gib, UsedBytes: 4 * gib},
		Load:   protocol.LoadStats{Load1: 1.5},
		PSI: &protocol.PSIStats{
			CPU:    protocol.Pressure{Some: protocol.PressureAvg{Avg10: 14}},
			Memory: protocol.Pressure{Some: protocol.PressureAvg{Avg10: 2.5}},
			IO:     protocol.Pressure{Some: protocol.PressureAvg{Avg10: 0.5}},
		},
		Filesystems: []protocol.FilesystemStats{
			{MountPoint: "/boot", TotalBytes: 100, AvailableBytes: 5, InodesTotal: 10, InodesFree: 1},
			{MountPoint: "/", TotalBytes: 100 * gib, AvailableBytes: 40 * gib, InodesTotal: 1000, InodesFree: 900},
		},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDerive(t *testing.T) {
	p, psi, mount := Derive(sample(37))
	if !psi || mount != "/" {
		t.Fatalf("psi=%v mount=%q", psi, mount)
	}
	want := Point{CPU: 37, Memory: 75, Swap: 100, Disk: 60, Inodes: 10, PSICPU: 14, PSIMem: 2.5, PSIIO: 0.5}
	if p != want {
		t.Fatalf("Derive = %+v, want %+v", p, want)
	}

	// No "/" mount: the fullest filesystem. No PSI, no swap, zero totals.
	m := sample(0)
	m.PSI = nil
	m.Swap = protocol.SwapStats{}
	m.Memory = protocol.MemoryStats{}
	m.Filesystems = []protocol.FilesystemStats{
		{MountPoint: "/data", TotalBytes: 100, AvailableBytes: 50},
		{MountPoint: "/var", TotalBytes: 100, AvailableBytes: 10},
		{MountPoint: "/empty"},
	}
	p, psi, mount = Derive(m)
	if psi || mount != "/var" || p.Disk != 90 || p.Swap != 0 || p.Memory != 0 || p.PSIMem != 0 {
		t.Fatalf("fallbacks: %+v psi=%v mount=%q", p, psi, mount)
	}

	// Nonsense input is clamped, never NaN or over 100.
	m = sample(250)
	m.Memory.AvailableBytes = m.Memory.TotalBytes * 2
	m.CPU.UsagePercent = math.NaN()
	p, _, _ = Derive(m)
	if p.CPU != 0 || p.Memory != 0 {
		t.Fatalf("clamping: %+v", p)
	}
	if p, _, _ := Derive(sample(250)); p.CPU != 100 {
		t.Fatalf("cpu 250 -> %v", p.CPU)
	}
}

type env struct {
	st  *store.SQLite
	c   *Collector
	now time.Time
	cl  store.Cluster
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cl, _ := st.EnsureCluster(context.Background(), "c-k3s-home", "k3s-home")
	e := &env{st: st, now: time.Date(2026, 9, 27, 12, 0, 5, 0, time.UTC), cl: cl}
	e.c = New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return e.now })
	return e
}

func (e *env) rollups(t *testing.T) []store.Rollup {
	t.Helper()
	rs, err := e.st.ListRollups(context.Background(), e.cl.ID, "k8server", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestObserveWritesMinuteAverages(t *testing.T) {
	e := newEnv(t)
	for _, cpu := range []float64{10, 20, 30} { // 12:00:05, :15, :25
		e.c.Observe(e.cl.ID, sample(cpu))
		e.now = e.now.Add(10 * time.Second)
	}
	if rs := e.rollups(t); len(rs) != 0 {
		t.Fatalf("rollup written before the minute ended: %+v", rs)
	}
	if s, ok := e.c.Latest(e.cl.ID, "k8server"); !ok || s.Point.CPU != 30 || s.Metrics.Load.Load1 != 1.5 {
		t.Fatalf("latest %+v %v", s, ok)
	}
	e.now = time.Date(2026, 9, 27, 12, 1, 3, 0, time.UTC)
	e.c.Observe(e.cl.ID, sample(90)) // next minute: the first is written
	rs := e.rollups(t)
	if len(rs) != 1 || !near(rs[0].CPUPct, 20) || !rs[0].Bucket.Equal(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)) ||
		rs[0].MemPct != 75 || rs[0].PSICPU != 14 {
		t.Fatalf("rollups %+v", rs)
	}

	// The node goes quiet: Flush writes its last minute once that minute is over.
	e.c.Flush(context.Background())
	if len(e.rollups(t)) != 1 {
		t.Fatal("Flush wrote the minute that is still running")
	}
	e.now = e.now.Add(time.Minute)
	e.c.Flush(context.Background())
	e.c.Flush(context.Background())
	if rs := e.rollups(t); len(rs) != 2 || rs[1].CPUPct != 90 {
		t.Fatalf("after flush: %+v", rs)
	}
}

func TestDownsample(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var rs []store.Rollup
	for i, cpu := range []float64{10, 20, 30, 40, 50} { // 12:00 .. 12:04
		rs = append(rs, store.Rollup{Bucket: base.Add(time.Duration(i) * time.Minute), CPUPct: cpu, MemPct: 50})
	}
	rs = append(rs, store.Rollup{Bucket: base.Add(20 * time.Minute), CPUPct: 99}) // after a gap
	got := Downsample(rs, 2*time.Minute)
	want := []float64{15, 35, 50, 99}
	if len(got) != len(want) {
		t.Fatalf("got %d points: %+v", len(got), got)
	}
	for i, w := range want {
		if !near(got[i].CPU, w) {
			t.Errorf("point %d cpu %v, want %v", i, got[i].CPU, w)
		}
	}
	if !got[3].T.Equal(base.Add(20*time.Minute)) || got[0].Memory != 50 {
		t.Fatalf("times/values: %+v", got)
	}
	if len(Downsample(nil, time.Minute)) != 0 {
		t.Fatal("empty input")
	}
}

func TestAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.UpsertNode(ctx, store.Node{ClusterID: e.cl.ID, Name: "k8server", Status: "connected", LastSeen: e.now})
	e.st.UpsertNode(ctx, store.Node{ClusterID: e.cl.ID, Name: "quiet", Status: "disconnected", LastSeen: e.now.Add(-time.Hour)})
	e.c.Observe(e.cl.ID, sample(37))
	var rs []store.Rollup
	for i := 1; i <= 90; i++ {
		rs = append(rs, store.Rollup{ClusterID: e.cl.ID, Node: "k8server", Bucket: e.now.Truncate(time.Minute).Add(-time.Duration(i) * time.Minute), CPUPct: float64(i % 100)})
	}
	e.st.WriteRollups(ctx, rs)

	api := NewAPI(e.c, e.st, func(_, node string) bool { return node == "k8server" })
	mux := http.NewServeMux()
	var guarded int
	require := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; h.ServeHTTP(w, r) })
	}
	api.Routes(require)(mux)
	get := func(path string) (*httptest.ResponseRecorder, map[string]any) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var body map[string]any
		json.Unmarshal(w.Body.Bytes(), &body)
		return w, body
	}

	w, body := get("/api/clusters/" + e.cl.ID + "/nodes")
	nodes, _ := body["nodes"].([]any)
	if w.Code != 200 || len(nodes) != 2 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	first := nodes[0].(map[string]any)
	cur := first["current"].(map[string]any)
	if first["name"] != "k8server" || first["connected"] != true || cur["cores"] != 4.0 || cur["point"].(map[string]any)["memory"] != 75.0 {
		t.Fatalf("node view %+v", first)
	}
	if _, has := nodes[1].(map[string]any)["current"]; has {
		t.Fatal("a node with no metrics since start has no current values")
	}

	for rg, maxPoints := range map[string]int{"1h": 61, "6h": 46, "24h": 19} {
		w, body = get("/api/clusters/" + e.cl.ID + "/nodes/k8server?range=" + rg)
		series, _ := body["series"].([]any)
		if w.Code != 200 || len(series) == 0 || len(series) > maxPoints {
			t.Errorf("range %s: %d, %d points", rg, w.Code, len(series))
		}
	}
	if _, body = get("/api/clusters/" + e.cl.ID + "/nodes/k8server"); body["latest"] == nil || body["range"] != "1h" {
		t.Fatalf("default range / latest: %+v", body["range"])
	}
	if w, _ = get("/api/clusters/" + e.cl.ID + "/nodes/k8server?range=7d"); w.Code != 400 {
		t.Fatalf("bad range: %d", w.Code)
	}
	if w, _ = get("/api/clusters/" + e.cl.ID + "/nodes/nope"); w.Code != 404 {
		t.Fatalf("unknown node: %d", w.Code)
	}
	if w, _ = get("/api/clusters/c-0000000000000000/nodes"); w.Code != 404 {
		t.Fatalf("unknown cluster: %d", w.Code)
	}
	if guarded != 8 {
		t.Fatalf("require wrapped %d of 8 requests", guarded)
	}
}

// KM-65: every node view carries the crossed state, and nodes Kubernetes
// lists without a node agent appear too.
func TestNodeStates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.UpsertNode(ctx, store.Node{ClusterID: e.cl.ID, Name: "k8server", Status: "connected", LastSeen: e.now})
	e.st.UpsertNode(ctx, store.Node{ClusterID: e.cl.ID, Name: "quiet", Status: "disconnected", LastSeen: e.now.Add(-5 * time.Minute)})
	e.st.UpsertNode(ctx, store.Node{ClusterID: e.cl.ID, Name: "dead", Status: "disconnected", LastSeen: e.now.Add(-2 * time.Minute)})
	api := NewAPI(e.c, e.st, func(_, node string) bool { return node == "k8server" })
	if !api.Started.Equal(e.now) {
		t.Fatalf("Started = %v, want the collector's now", api.Started)
	}
	started := api.Started
	api.Started = e.now.Add(-time.Hour) // the portal has run for an hour
	sum := &protocol.Summary{Nodes: protocol.NodeCounts{Total: 4}, NodeList: []protocol.NodeStatus{
		{Name: "k8server", Ready: true}, {Name: "quiet", Ready: true}, {Name: "dead", Ready: false}, {Name: "fresh", Ready: true}}}
	connected := true
	api.Kube = func(string) (*protocol.Summary, bool) { return sum, connected }
	mux := http.NewServeMux()
	api.Routes(func(h http.Handler) http.Handler { return h })(mux)
	states := func() map[string]string {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/clusters/"+e.cl.ID+"/nodes", nil))
		var body struct {
			Nodes []struct {
				Name  string `json:"name"`
				State struct {
					Code string `json:"code"`
				} `json:"state"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		out := map[string]string{}
		for _, n := range body.Nodes {
			out[n.Name] = n.State.Code
		}
		return out
	}
	got := states()
	want := map[string]string{"k8server": "ok", "quiet": "agent-down", "dead": "node-down", "fresh": "no-agent"}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("%s: %s, want %s (all: %v)", n, got[n], c, got)
		}
	}
	// Cluster agent gone: the silent nodes are unreachable, not "agent down".
	connected = false
	if got := states(); got["quiet"] != "unreachable" || got["k8server"] != "ok" || len(got) != 3 {
		t.Errorf("cluster agent offline: %v", got)
	}
	// Review N-1: the portal has just started; the silent node agents have
	// not reconnected yet. They read reconnecting, not agent-down.
	connected = true
	api.Started = started
	if got := states(); got["quiet"] != "reconnecting" || got["dead"] != "reconnecting" || got["k8server"] != "ok" {
		t.Errorf("just after a portal start: %v", got)
	}
	e.now = e.now.Add(2 * time.Minute)
	if got := states(); got["quiet"] != "agent-down" || got["dead"] != "node-down" {
		t.Errorf("2 min after a portal start: %v", got)
	}
}

// Review B1: a kernel without PSI must never become 0 % pressure. A minute
// with no PSI sample has no PSI; a minute with some averages only those.
func TestNoPSIIsNotZeroPressure(t *testing.T) {
	e := newEnv(t)
	noPSI := func(cpu float64) *protocol.Metrics { m := sample(cpu); m.PSI = nil; return m }

	e.c.Observe(e.cl.ID, noPSI(10)) // 12:00:05, no PSI at all this minute
	e.now = e.now.Add(10 * time.Second)
	e.c.Observe(e.cl.ID, noPSI(20))
	e.now = time.Date(2026, 9, 27, 12, 1, 3, 0, time.UTC)
	e.c.Observe(e.cl.ID, noPSI(30)) // 12:01: one sample without PSI ...
	e.now = e.now.Add(10 * time.Second)
	e.c.Observe(e.cl.ID, sample(40)) // ... and one with (cpu some 14)
	e.now = time.Date(2026, 9, 27, 12, 2, 3, 0, time.UTC)
	e.c.Flush(context.Background())

	rs := e.rollups(t)
	if len(rs) != 2 {
		t.Fatalf("rollups %+v", rs)
	}
	if rs[0].HasPSI || rs[0].PSICPU != 0 || !near(rs[0].CPUPct, 15) {
		t.Fatalf("minute without PSI: %+v", rs[0])
	}
	if !rs[1].HasPSI || !near(rs[1].PSICPU, 14) || !near(rs[1].PSIMemory, 2.5) {
		t.Fatalf("mixed minute must average PSI over its PSI samples only: %+v", rs[1])
	}

	// The chart series: null PSI where there was none, the value where there was.
	got := Downsample(rs, time.Minute)
	if len(got) != 2 || got[0].PSICPU != nil || got[0].PSIMem != nil || got[0].PSIIO != nil {
		t.Fatalf("series without PSI: %+v", got)
	}
	if got[1].PSICPU == nil || !near(*got[1].PSICPU, 14) {
		t.Fatalf("series with PSI: %+v", got[1])
	}
	// One step over both minutes: PSI from the one minute that had it.
	one := Downsample(rs, 2*time.Minute)
	if len(one) != 1 || one[0].PSICPU == nil || !near(*one[0].PSICPU, 14) || !near(one[0].CPU, 25) {
		t.Fatalf("2-minute step: %+v", one)
	}

	// On the wire the missing values are null, not 0.
	b, _ := json.Marshal(got[0])
	if !strings.Contains(string(b), `"psiCpu":null`) || !strings.Contains(string(b), `"psiIo":null`) {
		t.Fatalf("JSON %s", b)
	}
}
