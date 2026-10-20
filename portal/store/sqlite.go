package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// SQLite implements Store.
type SQLite struct {
	db *sql.DB
	// now is the clock; tests replace it.
	now func() time.Time
}

var _ Store = (*SQLite)(nil)

// Open opens (or creates) the database at path, turns on WAL mode and
// foreign keys, and applies any pending migrations.
func Open(ctx context.Context, path string) (*SQLite, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	s := &SQLite{db: db, now: time.Now}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// buildDSN turns a file path into a SQLite URI with the connection pragmas.
// The path is made absolute and percent-escaped, so '?', '#' and '%' in a
// directory name cannot cut off the pragmas or change the file location.
func buildDSN(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows: file:///C:/ProgramData/Kubemap/portal.db
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	return u.String(), nil
}

func (s *SQLite) Close() error { return s.db.Close() }

type migration struct {
	version  int
	name     string
	body     string
	checksum string
}

// loadMigrations reads NNNN_name.sql files from fsys. Versions must be
// unique and run 1, 2, 3, ... with no gaps, so two branches that both add
// "0002_*.sql" fail loudly instead of one being skipped.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []migration
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %s: bad version prefix", name)
		}
		if want := len(out) + 1; version != want {
			if version < want {
				return nil, fmt.Errorf("migration %s: duplicate version %d (also %s)", name, version, out[version-1].name)
			}
			return nil, fmt.Errorf("migration %s: version %d is missing", name, want)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		out = append(out, migration{version: version, name: name, body: string(body), checksum: hashSecret(string(body))})
	}
	return out, nil
}

// Migrate applies embedded migrations that are not yet recorded in
// schema_migrations. It is safe to call more than once. It fails if an
// applied migration's file has changed since it ran.
func (s *SQLite) Migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return s.migrate(ctx, sub)
}

func (s *SQLite) migrate(ctx context.Context, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		checksum   TEXT NOT NULL,
		applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	for _, m := range ms {
		if err := s.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
	}
	return nil
}

func (s *SQLite) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied string
	err = tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, m.version).Scan(&applied)
	switch {
	case err == nil && applied == m.checksum:
		return nil
	case err == nil:
		return errors.New("file changed after it was applied; add a new migration instead")
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
		m.version, m.checksum, s.now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// newSecret returns a random URL-safe string with 256 bits of entropy.
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// isUniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func fromNull(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// EnsureCluster returns the cluster with this ID, creating it active when it
// does not exist yet. The server calls it once at start for its own cluster.
func (s *SQLite) EnsureCluster(ctx context.Context, id, name string) (Cluster, error) {
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO clusters (id, name, status, created_at, accepted_at, registered_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET name = excluded.name`,
		id, name, StatusActive, now, now, now); err != nil {
		return Cluster{}, fmt.Errorf("store: ensure cluster: %w", err)
	}
	return s.GetCluster(ctx, id)
}

const clusterCols = `id, name, status, created_at, accepted_at, last_seen, agent_version,
	registered_at, source_ip, kubernetes_version, node_count, cluster_uid`

func scanCluster(row interface{ Scan(...any) error }) (Cluster, error) {
	var c Cluster
	var created int64
	var accepted, seen, registered sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.Status, &created, &accepted, &seen, &c.AgentVersion,
		&registered, &c.SourceIP, &c.KubernetesVersion, &c.NodeCount, &c.ClusterUID); err != nil {
		return Cluster{}, err
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	c.AcceptedAt = fromNull(accepted)
	c.LastSeen = fromNull(seen)
	c.RegisteredAt = fromNull(registered)
	return c, nil
}

func (s *SQLite) GetCluster(ctx context.Context, id string) (Cluster, error) {
	c, err := scanCluster(s.db.QueryRowContext(ctx, `SELECT `+clusterCols+` FROM clusters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, ErrNotFound
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("store: get cluster: %w", err)
	}
	return c, nil
}

func (s *SQLite) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+clusterCols+` FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list clusters: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	return out, nil
}

func (s *SQLite) MarkClusterSeen(ctx context.Context, id, agentVersion string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clusters SET last_seen = ?,
		   agent_version = CASE WHEN ? = '' THEN agent_version ELSE ? END,
		   status = CASE WHEN status = ? THEN ? ELSE status END
		 WHERE id = ?`,
		s.now().Unix(), agentVersion, agentVersion, StatusDisconnected, StatusActive, id)
	if err != nil {
		return fmt.Errorf("store: mark cluster seen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) MarkClusterDisconnected(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clusters SET status = ? WHERE id = ? AND status = ?`,
		StatusDisconnected, id, StatusActive)
	if err != nil {
		return fmt.Errorf("store: mark cluster disconnected: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetCluster(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: only an active cluster can be disconnected", ErrInvalidState)
	}
	return nil
}

func (s *SQLite) UpsertNode(ctx context.Context, n Node) error {
	seen := n.LastSeen
	if seen.IsZero() {
		seen = s.now()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (cluster_id, name, status, last_seen, agent_version) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (cluster_id, name) DO UPDATE SET status = excluded.status, last_seen = excluded.last_seen,
		   agent_version = CASE WHEN excluded.agent_version = '' THEN nodes.agent_version ELSE excluded.agent_version END`,
		n.ClusterID, n.Name, n.Status, seen.Unix(), n.AgentVersion); err != nil {
		return fmt.Errorf("store: upsert node: %w", err)
	}
	return nil
}

func (s *SQLite) WriteAudit(ctx context.Context, e AuditEntry) error {
	t := e.Time
	if t.IsZero() {
		t = s.now()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (time, user, cluster_id, action, target, result) VALUES (?, ?, ?, ?, ?, ?)`,
		t.Unix(), e.User, e.ClusterID, e.Action, e.Target, e.Result); err != nil {
		return fmt.Errorf("store: write audit: %w", err)
	}
	return nil
}

func (s *SQLite) WriteRollups(ctx context.Context, rs []Rollup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: write rollups: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO metric_rollups
		 (cluster_id, node, bucket, cpu_pct, mem_pct, swap_pct, disk_pct, inodes_pct, psi_cpu, psi_memory, psi_io)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: write rollups: %w", err)
	}
	defer stmt.Close()
	for _, r := range rs {
		var psiCPU, psiMem, psiIO any // NULL when the node's kernel has no PSI
		if r.HasPSI {
			psiCPU, psiMem, psiIO = r.PSICPU, r.PSIMemory, r.PSIIO
		}
		if _, err := stmt.ExecContext(ctx, r.ClusterID, r.Node, r.Bucket.Unix(),
			r.CPUPct, r.MemPct, r.SwapPct, r.DiskPct, r.InodesPct, psiCPU, psiMem, psiIO); err != nil {
			return fmt.Errorf("store: write rollups: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: write rollups: %w", err)
	}
	return nil
}

func (s *SQLite) PruneRollups(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-RollupRetention).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM metric_rollups WHERE bucket < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune rollups: %w", err)
	}
	return res.RowsAffected()
}
