package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// testClock is a settable clock for tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openTest(t *testing.T) (*SQLite, *testClock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	clk := &testClock{t: t0}
	s.now = clk.now
	return s, clk, path
}

func mustCluster(t *testing.T, s *SQLite, name string) Cluster {
	t.Helper()
	c, err := s.EnsureCluster(context.Background(), "c-"+name, name)
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	return c
}

func TestWALMode(t *testing.T) {
	s, _, _ := openTest(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// schemaSnapshot returns the full schema plus the migration records.
func schemaSnapshot(t *testing.T, s *SQLite) string {
	t.Helper()
	var b strings.Builder
	rows, err := s.db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name, sql string
		if err := rows.Scan(&typ, &name, &sql); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + " " + name + " " + sql + "\n")
	}
	mrows, err := s.db.Query(`SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var v, at int64
		if err := mrows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "migration %d applied_at %d\n", v, at)
	}
	return b.String()
}

func TestMigrateTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, clk, path := openTest(t)
	mustCluster(t, s, "k3s-home")
	before := schemaSnapshot(t, s)

	clk.add(time.Hour) // a re-applied migration would record a new applied_at
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if after := schemaSnapshot(t, s); after != before {
		t.Fatalf("schema changed after second Migrate:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Reopening the same file runs migrations again and keeps the data.
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if after := schemaSnapshot(t, s2); after != before {
		t.Fatalf("schema changed after reopen:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	cs, err := s2.ListClusters(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatalf("ListClusters after reopen = %v, %v; want 1 cluster", cs, err)
	}
}

func TestPruneRollups(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	rs := []Rollup{
		{ClusterID: c.ID, Node: "k8server", Bucket: t0.Add(-25 * time.Hour), CPUPct: 1},
		{ClusterID: c.ID, Node: "k8server", Bucket: t0.Add(-24*time.Hour - time.Second), CPUPct: 2},
		{ClusterID: c.ID, Node: "k8server", Bucket: t0.Add(-24 * time.Hour), CPUPct: 3},
		{ClusterID: c.ID, Node: "k8server", Bucket: t0.Add(-time.Minute), CPUPct: 4, MemPct: 86, PSIMemory: 0.5},
	}
	if err := s.WriteRollups(ctx, rs); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneRollups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}
	var left []float64
	rows, err := s.db.Query(`SELECT cpu_pct FROM metric_rollups ORDER BY bucket`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v float64
		rows.Scan(&v)
		left = append(left, v)
	}
	if len(left) != 2 || left[0] != 3 || left[1] != 4 {
		t.Fatalf("remaining cpu_pct = %v, want [3 4]", left)
	}
}

func TestUpsertNodeAndAudit(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	if err := s.UpsertNode(ctx, Node{ClusterID: c.ID, Name: "k8server", Status: "Ready"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNode(ctx, Node{ClusterID: c.ID, Name: "k8server", Status: "NotReady", LastSeen: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var count int
	var status string
	var seen int64
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(status), MAX(last_seen) FROM nodes`).Scan(&count, &status, &seen); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != "NotReady" || seen != t0.Add(time.Minute).Unix() {
		t.Fatalf("nodes: count=%d status=%q last_seen=%d", count, status, seen)
	}

	if err := s.WriteAudit(ctx, AuditEntry{User: "admin", ClusterID: c.ID, Action: "cluster.accept", Target: c.Name, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	var at int64
	var action string
	if err := s.db.QueryRow(`SELECT time, action FROM audit_log`).Scan(&at, &action); err != nil {
		t.Fatal(err)
	}
	if at != t0.Unix() || action != "cluster.accept" {
		t.Fatalf("audit row: time=%d action=%q", at, action)
	}

	// Nodes of an unknown cluster are rejected by the foreign key.
	if err := s.UpsertNode(ctx, Node{ClusterID: "c-missing", Name: "x", Status: "Ready"}); err == nil {
		t.Fatal("UpsertNode for unknown cluster succeeded; foreign keys off?")
	}
}

func TestContextCancelled(t *testing.T) {
	s, _, _ := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.EnsureCluster(ctx, "c-x", "x"); err == nil {
		t.Fatal("EnsureCluster with cancelled context succeeded")
	}
}

func TestBuildDSNEscapesPath(t *testing.T) {
	ctx := context.Background()
	for _, dir := range []string{"with space", "q?dir", "hash#dir", "pct%20dir"} {
		t.Run(dir, func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, dir, "portal.db")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()
			var mode string
			if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not at %s: %v", path, err)
			}
			entries, _ := os.ReadDir(base)
			if len(entries) != 1 {
				t.Fatalf("files outside the target directory: %v", entries)
			}
		})
	}
}

func TestMigrationVersionChecks(t *testing.T) {
	body := &fstest.MapFile{Data: []byte("CREATE TABLE t (x INTEGER);")}
	cases := map[string]fstest.MapFS{
		"duplicate": {"0001_a.sql": body, "0002_b.sql": body, "0002_c.sql": body},
		"gap":       {"0001_a.sql": body, "0003_c.sql": body},
		"bad name":  {"init.sql": body},
	}
	for name, fsys := range cases {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: loadMigrations accepted %v", name, fsys)
		}
	}
	if ms, err := loadMigrations(fstest.MapFS{"0001_a.sql": body, "0002_b.sql": body}); err != nil || len(ms) != 2 {
		t.Fatalf("valid set: %v, %v", ms, err)
	}
}

func TestMigrationChangedAfterApplyFails(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	// Start from an empty migration history with a test migration set.
	if _, err := s.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	v1 := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE extra (x INTEGER);")}}
	if err := s.migrate(ctx, v1); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := s.migrate(ctx, v1); err != nil {
		t.Fatalf("same migration again: %v", err)
	}
	v1changed := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE extra (x INTEGER, y TEXT);")}}
	if err := s.migrate(ctx, v1changed); err == nil {
		t.Fatal("migrate accepted an applied migration whose file changed")
	}
}

// Review KM-30 B1: a rollup without PSI is stored as NULL and read back
// without PSI, never as 0 %.
func TestRollupWithoutPSI(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	if err := s.WriteRollups(ctx, []Rollup{
		{ClusterID: c.ID, Node: "rocky", Bucket: t0.Add(-time.Minute), CPUPct: 7, PSICPU: 99}, // HasPSI false: PSI ignored
		{ClusterID: c.ID, Node: "k8server", Bucket: t0.Add(-time.Minute), CPUPct: 8, PSICPU: 0, PSIMemory: 0, PSIIO: 0, HasPSI: true},
	}); err != nil {
		t.Fatal(err)
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM metric_rollups WHERE psi_cpu IS NULL AND psi_memory IS NULL AND psi_io IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("NULL PSI rows = %d, %v", nulls, err)
	}
	got, _ := s.ListRollups(ctx, c.ID, "rocky", t0.Add(-time.Hour))
	if len(got) != 1 || got[0].HasPSI || got[0].PSICPU != 0 || got[0].CPUPct != 7 {
		t.Fatalf("rocky: %+v", got)
	}
	// A real 0 % stays a real 0 %.
	got, _ = s.ListRollups(ctx, c.ID, "k8server", t0.Add(-time.Hour))
	if len(got) != 1 || !got[0].HasPSI || got[0].PSICPU != 0 {
		t.Fatalf("k8server: %+v", got)
	}
}

// Migration 0004 rebuilds metric_rollups; rows written under 0001 keep
// their values and read back with PSI.
func TestMigration0004KeepsRollups(t *testing.T) {
	ctx := context.Background()
	dsn, err := buildDSN(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	s := &SQLite{db: db, now: func() time.Time { return t0 }}
	t.Cleanup(func() { s.Close() })

	old := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_sessions.sql", "0003_cluster_registration.sql"} {
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		old[name] = &fstest.MapFile{Data: b}
	}
	if err := s.migrate(ctx, old); err != nil {
		t.Fatal(err)
	}
	c := mustCluster(t, s, "k3s-home")
	if _, err := s.db.Exec(`INSERT INTO metric_rollups VALUES (?, 'k8server', ?, 1, 2, 3, 4, 5, 6.5, 7.5, 8.5)`, c.ID, t0.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListRollups(ctx, c.ID, "k8server", t0.Add(-time.Hour))
	if err != nil || len(got) != 1 || !got[0].HasPSI || got[0].PSICPU != 6.5 || got[0].PSIIO != 8.5 || got[0].InodesPct != 5 {
		t.Fatalf("after 0004: %+v, %v", got, err)
	}
	// The index is back, and the cascade still deletes a cluster's rollups.
	var idx int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'metric_rollups_bucket'`).Scan(&idx)
	if idx != 1 {
		t.Fatal("index metric_rollups_bucket missing after 0004")
	}
	if _, err := s.db.Exec(`DELETE FROM clusters WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM metric_rollups`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rollups left after the cluster was deleted", left)
	}
}
