package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *SQLite) UpdateClusterInfo(ctx context.Context, id string, info ClusterInfo) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clusters SET agent_version = ?, source_ip = ?, kubernetes_version = ?, node_count = ?, cluster_uid = ?
		 WHERE id = ?`,
		info.AgentVersion, info.SourceIP, info.KubernetesVersion, info.NodeCount, info.ClusterUID, id)
	if err != nil {
		return fmt.Errorf("store: update cluster info: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT time, user, cluster_id, action, target, result FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var t int64
		if err := rows.Scan(&t, &e.User, &e.ClusterID, &e.Action, &e.Target, &e.Result); err != nil {
			return nil, fmt.Errorf("store: list audit: %w", err)
		}
		e.Time = time.Unix(t, 0).UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	return out, nil
}

func (s *SQLite) ResetConnections(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: reset connections: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE clusters SET status = ? WHERE status = ?`, StatusDisconnected, StatusActive); err != nil {
		return fmt.Errorf("store: reset connections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET status = 'disconnected'`); err != nil {
		return fmt.Errorf("store: reset connections: %w", err)
	}
	return tx.Commit()
}

func (s *SQLite) SetNodeStatus(ctx context.Context, clusterID, name, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET status = ? WHERE cluster_id = ? AND name = ?`, status, clusterID, name)
	if err != nil {
		return fmt.Errorf("store: set node status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) ListRollups(ctx context.Context, clusterID, node string, since time.Time) ([]Rollup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT bucket, cpu_pct, mem_pct, swap_pct, disk_pct, inodes_pct, psi_cpu, psi_memory, psi_io
		 FROM metric_rollups WHERE cluster_id = ? AND node = ? AND bucket >= ? ORDER BY bucket`,
		clusterID, node, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: list rollups: %w", err)
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		r := Rollup{ClusterID: clusterID, Node: node}
		var b int64
		var psiCPU, psiMem, psiIO sql.NullFloat64
		if err := rows.Scan(&b, &r.CPUPct, &r.MemPct, &r.SwapPct, &r.DiskPct, &r.InodesPct, &psiCPU, &psiMem, &psiIO); err != nil {
			return nil, fmt.Errorf("store: list rollups: %w", err)
		}
		if psiCPU.Valid && psiMem.Valid && psiIO.Valid {
			r.PSICPU, r.PSIMemory, r.PSIIO, r.HasPSI = psiCPU.Float64, psiMem.Float64, psiIO.Float64, true
		}
		r.Bucket = time.Unix(b, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rollups: %w", err)
	}
	return out, nil
}

func (s *SQLite) ListNodes(ctx context.Context, clusterID string) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, status, last_seen, agent_version FROM nodes WHERE cluster_id = ? ORDER BY name`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n := Node{ClusterID: clusterID}
		var seen int64
		if err := rows.Scan(&n.Name, &n.Status, &seen, &n.AgentVersion); err != nil {
			return nil, fmt.Errorf("store: list nodes: %w", err)
		}
		n.LastSeen = time.Unix(seen, 0).UTC()
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list nodes: %w", err)
	}
	return out, nil
}
