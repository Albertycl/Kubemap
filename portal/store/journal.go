package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *SQLite) CreateJournalEntry(ctx context.Context, e JournalEntry) (JournalEntry, error) {
	id, err := newID("inc-")
	if err != nil {
		return JournalEntry{}, fmt.Errorf("store: create journal entry: %w", err)
	}
	now := s.now().UTC().Truncate(time.Second)
	e.ID, e.Version = id, 1
	e.CreatedAt, e.UpdatedAt, e.UpdatedBy = now, now, e.CreatedBy
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO journal_entries (id, kind, title, date, cluster, severity, body, source, version, created_at, created_by, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Kind, e.Title, e.Date, e.Cluster, e.Severity, string(e.Body), e.Source, e.Version,
		now.Unix(), e.CreatedBy, now.Unix(), e.UpdatedBy)
	if isUniqueViolation(err) {
		return JournalEntry{}, ErrExists
	}
	if err != nil {
		return JournalEntry{}, fmt.Errorf("store: create journal entry: %w", err)
	}
	return e, nil
}

const journalColumns = `id, kind, title, date, cluster, severity, body, source, version, created_at, created_by, updated_at, updated_by`

func scanJournal(row interface{ Scan(...any) error }) (JournalEntry, error) {
	var e JournalEntry
	var body string
	var created, updated int64
	err := row.Scan(&e.ID, &e.Kind, &e.Title, &e.Date, &e.Cluster, &e.Severity, &body, &e.Source, &e.Version,
		&created, &e.CreatedBy, &updated, &e.UpdatedBy)
	e.Body = []byte(body)
	e.CreatedAt, e.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return e, err
}

func (s *SQLite) GetJournalEntry(ctx context.Context, id string) (JournalEntry, error) {
	e, err := scanJournal(s.db.QueryRowContext(ctx, `SELECT `+journalColumns+` FROM journal_entries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEntry{}, ErrNotFound
	}
	if err != nil {
		return JournalEntry{}, fmt.Errorf("store: get journal entry: %w", err)
	}
	return e, nil
}

func (s *SQLite) FindJournalEntryBySource(ctx context.Context, source string) (JournalEntry, error) {
	if source == "" {
		return JournalEntry{}, ErrNotFound
	}
	e, err := scanJournal(s.db.QueryRowContext(ctx, `SELECT `+journalColumns+` FROM journal_entries WHERE source = ?`, source))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEntry{}, ErrNotFound
	}
	if err != nil {
		return JournalEntry{}, fmt.Errorf("store: find journal entry: %w", err)
	}
	return e, nil
}

func (s *SQLite) ListJournalEntries(ctx context.Context) ([]JournalEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+journalColumns+` FROM journal_entries ORDER BY date DESC, created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list journal entries: %w", err)
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list journal entries: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list journal entries: %w", err)
	}
	return out, nil
}

func (s *SQLite) UpdateJournalEntry(ctx context.Context, e JournalEntry) (JournalEntry, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`UPDATE journal_entries SET kind = ?, title = ?, date = ?, cluster = ?, severity = ?, body = ?,
		        version = version + 1, updated_at = ?, updated_by = ?
		 WHERE id = ? AND version = ?`,
		e.Kind, e.Title, e.Date, e.Cluster, e.Severity, string(e.Body), now.Unix(), e.UpdatedBy, e.ID, e.Version)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("store: update journal entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetJournalEntry(ctx, e.ID); err != nil {
			return JournalEntry{}, err
		}
		return JournalEntry{}, ErrConflict
	}
	return s.GetJournalEntry(ctx, e.ID)
}

func (s *SQLite) DeleteJournalEntry(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM journal_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete journal entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) AddJournalEvidence(ctx context.Context, ev JournalEvidence) (JournalEvidence, error) {
	id, err := newID("ev-")
	if err != nil {
		return JournalEvidence{}, fmt.Errorf("store: add evidence: %w", err)
	}
	ev.ID = id
	if ev.CapturedAt.IsZero() {
		ev.CapturedAt = s.now()
	}
	ev.CapturedAt = ev.CapturedAt.UTC().Truncate(time.Second)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO journal_evidence (id, entry_id, kind, cluster_id, summary, link, note, data, captured_at, captured_by)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM journal_entries WHERE id = ?)`,
		ev.ID, ev.EntryID, ev.Kind, ev.ClusterID, ev.Summary, ev.Link, ev.Note, string(ev.Data),
		ev.CapturedAt.Unix(), ev.CapturedBy, ev.EntryID)
	if err != nil {
		return JournalEvidence{}, fmt.Errorf("store: add evidence: %w", err)
	}
	if _, err := s.GetJournalEntry(ctx, ev.EntryID); err != nil {
		return JournalEvidence{}, err
	}
	return ev, nil
}

func (s *SQLite) ListJournalEvidence(ctx context.Context, entryID string) ([]JournalEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, entry_id, kind, cluster_id, summary, link, note, data, captured_at, captured_by
		 FROM journal_evidence WHERE entry_id = ? ORDER BY captured_at, id`, entryID)
	if err != nil {
		return nil, fmt.Errorf("store: list evidence: %w", err)
	}
	defer rows.Close()
	var out []JournalEvidence
	for rows.Next() {
		var ev JournalEvidence
		var data string
		var t int64
		if err := rows.Scan(&ev.ID, &ev.EntryID, &ev.Kind, &ev.ClusterID, &ev.Summary, &ev.Link, &ev.Note, &data, &t, &ev.CapturedBy); err != nil {
			return nil, fmt.Errorf("store: list evidence: %w", err)
		}
		ev.Data, ev.CapturedAt = []byte(data), time.Unix(t, 0).UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list evidence: %w", err)
	}
	return out, nil
}

func (s *SQLite) DeleteJournalEvidence(ctx context.Context, entryID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM journal_evidence WHERE entry_id = ? AND id = ?`, entryID, id)
	if err != nil {
		return fmt.Errorf("store: delete evidence: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) SaveJournalDraft(ctx context.Context, d JournalDraft) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO journal_drafts (id, cluster_id, started, body) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET body = excluded.body, started = excluded.started`,
		d.ID, d.ClusterID, d.Started.Unix(), string(d.Body))
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: save draft: %w", err)
	}
	return nil
}

func scanDraft(row interface{ Scan(...any) error }) (JournalDraft, error) {
	var d JournalDraft
	var started int64
	var body string
	err := row.Scan(&d.ID, &d.ClusterID, &started, &body)
	d.Started, d.Body = time.Unix(started, 0).UTC(), []byte(body)
	return d, err
}

func (s *SQLite) GetJournalDraft(ctx context.Context, id string) (JournalDraft, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx, `SELECT id, cluster_id, started, body FROM journal_drafts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalDraft{}, ErrNotFound
	}
	if err != nil {
		return JournalDraft{}, fmt.Errorf("store: get draft: %w", err)
	}
	return d, nil
}

func (s *SQLite) LatestJournalDraft(ctx context.Context, clusterID string) (JournalDraft, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx,
		`SELECT id, cluster_id, started, body FROM journal_drafts WHERE cluster_id = ? ORDER BY started DESC, id DESC LIMIT 1`, clusterID))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalDraft{}, ErrNotFound
	}
	if err != nil {
		return JournalDraft{}, fmt.Errorf("store: latest draft: %w", err)
	}
	return d, nil
}

func (s *SQLite) ListJournalDrafts(ctx context.Context) ([]JournalDraft, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, cluster_id, started, body FROM journal_drafts ORDER BY started DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list drafts: %w", err)
	}
	defer rows.Close()
	var out []JournalDraft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list drafts: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
