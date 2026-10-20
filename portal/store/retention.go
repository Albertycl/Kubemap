package store

import (
	"context"
	"fmt"
	"time"
)

// Retention of records that are not needed forever (review KM-44 L9).
const (
	// TokenRetention keeps used and expired import tokens this long, so a
	// late "already used" answer is still exact.
	TokenRetention = 7 * 24 * time.Hour
	// DraftRetention keeps draft incidents this long after they started;
	// each cluster's newest draft is always kept (the drafter continues it).
	// A draft that was written up lives on in its journal entry.
	DraftRetention = 90 * 24 * time.Hour
	// MaxEvidencePerEntry bounds the snapshots of one journal entry.
	MaxEvidencePerEntry = 50
)

func (s *SQLite) PruneAudit(ctx context.Context, keep time.Duration) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE time < ?`, s.now().Add(-keep).Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune audit: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLite) PruneDrafts(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM journal_drafts WHERE started < ? AND id NOT IN (
		   SELECT d.id FROM journal_drafts d
		   WHERE d.started = (SELECT MAX(started) FROM journal_drafts WHERE cluster_id = d.cluster_id))`,
		s.now().Add(-DraftRetention).Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune drafts: %w", err)
	}
	return res.RowsAffected()
}

func (s *SQLite) CountJournalEvidence(ctx context.Context, entryID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_evidence WHERE entry_id = ?`, entryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count evidence: %w", err)
	}
	return n, nil
}
