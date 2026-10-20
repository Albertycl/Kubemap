package journal

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"kubemap/portal/store"
)

// ImportRequest is a Markdown file to import.
type ImportRequest struct {
	Markdown string `json:"markdown"`
	// Source is the file name ("2026-09-28-disk-pressure.md"). There is
	// one entry per source: a second import fails unless Replace is set.
	Source   string `json:"source"`
	Cluster  string `json:"cluster"`  // overrides what the text says
	Severity string `json:"severity"` // overrides what the text says
	Replace  bool   `json:"replace"`
}

// InvalidError is an import that cannot be read or stored as it is.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// ExistsError is a second import of a source without Replace.
type ExistsError struct{ ID, Source string }

func (e *ExistsError) Error() string {
	return e.Source + " was imported already; open it, or import again with replace"
}

var sourceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.md$`)

// Import stores a Markdown record, or replaces the entry imported from the
// same source (its evidence stays), and writes the audit entry as user by.
// exact tells whether an export gives the file back unchanged.
func Import(ctx context.Context, st store.Store, req ImportRequest, by string) (saved store.JournalEntry, exact bool, err error) {
	if req.Source != "" && !sourceRE.MatchString(req.Source) {
		return saved, false, &InvalidError{"the source must be a file name ending in .md, such as 2026-09-28-disk-pressure.md"}
	}
	rec, err := Parse(req.Markdown, req.Source)
	if err != nil {
		return saved, false, &InvalidError{err.Error()}
	}
	if req.Cluster != "" {
		rec.Cluster = req.Cluster
	}
	if req.Severity != "" {
		rec.Severity = req.Severity
	}
	if err := rec.Validate(); err != nil {
		return saved, false, &InvalidError{err.Error()}
	}
	exact = Render(rec) == strings.ReplaceAll(req.Markdown, "\r\n", "\n")
	se, err := fromRecord(rec)
	if err != nil {
		return saved, false, err
	}
	old, err := st.FindJournalEntryBySource(ctx, req.Source)
	switch {
	case err == nil && !req.Replace:
		return saved, false, &ExistsError{ID: old.ID, Source: req.Source}
	case err == nil:
		se.ID, se.Version, se.UpdatedBy = old.ID, old.Version, by
		saved, err = st.UpdateJournalEntry(ctx, se)
	case errors.Is(err, store.ErrNotFound):
		se.Source, se.CreatedBy = req.Source, by
		saved, err = st.CreateJournalEntry(ctx, se)
		if errors.Is(err, store.ErrExists) { // imported at the same moment
			return saved, false, &ExistsError{Source: req.Source}
		}
	}
	if err != nil {
		return saved, false, err
	}
	if err := st.WriteAudit(ctx, store.AuditEntry{User: by, Action: "journal.import", Target: target(saved.ID, saved.Title), Result: "ok"}); err != nil {
		return saved, exact, err
	}
	return saved, exact, nil
}
