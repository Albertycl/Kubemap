package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJournalEntries(t *testing.T) {
	s, clk, _ := openTest(t)
	ctx := context.Background()

	e, err := s.CreateJournalEntry(ctx, JournalEntry{Kind: "incident", Title: "Disk pressure", Date: "2026-09-28",
		Cluster: "k8server", Severity: "major", Body: []byte(`{"a":1}`), Source: "2026-09-28-disk-pressure.md", CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == "" || e.Version != 1 || e.UpdatedBy != "admin" || !e.CreatedAt.Equal(t0.Truncate(time.Second)) {
		t.Fatalf("created %+v", e)
	}
	if _, err := s.CreateJournalEntry(ctx, JournalEntry{Kind: "incident", Title: "again", Body: []byte(`{}`), Source: e.Source, CreatedBy: "admin"}); !errors.Is(err, ErrExists) {
		t.Fatalf("same source: %v, want ErrExists", err)
	}
	// Entries without a source do not clash.
	for i := 0; i < 2; i++ {
		if _, err := s.CreateJournalEntry(ctx, JournalEntry{Kind: "incident", Title: "manual", Date: "2026-01-0" + string(rune('1'+i)), Body: []byte(`{}`), CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.FindJournalEntryBySource(ctx, e.Source); err != nil || got.ID != e.ID {
		t.Fatalf("find by source: %+v %v", got, err)
	}
	if _, err := s.FindJournalEntryBySource(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty source: %v", err)
	}
	list, err := s.ListJournalEntries(ctx)
	if err != nil || len(list) != 3 || list[0].ID != e.ID {
		t.Fatalf("list: %d %v (newest date first)", len(list), err)
	}

	clk.add(time.Minute)
	e.Title, e.Body, e.UpdatedBy = "Disk pressure evicted every app", []byte(`{"a":2}`), "bob"
	up, err := s.UpdateJournalEntry(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if up.Version != 2 || up.Title != e.Title || string(up.Body) != `{"a":2}` || up.UpdatedBy != "bob" || up.CreatedBy != "admin" || !up.UpdatedAt.After(up.CreatedAt) {
		t.Fatalf("updated %+v", up)
	}
	// e still names version 1: someone else's edit is not overwritten.
	if _, err := s.UpdateJournalEntry(ctx, e); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v, want ErrConflict", err)
	}
	if _, err := s.UpdateJournalEntry(ctx, JournalEntry{ID: "inc-none", Version: 1, Body: []byte(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown update: %v", err)
	}

	ev, err := s.AddJournalEvidence(ctx, JournalEvidence{EntryID: e.ID, Kind: "findings", ClusterID: "c-1", Summary: "2 critical",
		Link: "#/clusters/c-1/problems", Data: []byte(`{"findings":[]}`), CapturedBy: "admin"})
	if err != nil || ev.ID == "" {
		t.Fatalf("add evidence: %+v %v", ev, err)
	}
	if _, err := s.AddJournalEvidence(ctx, JournalEvidence{EntryID: "inc-none", Kind: "audit", Data: []byte(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evidence for unknown entry: %v", err)
	}
	evs, err := s.ListJournalEvidence(ctx, e.ID)
	if err != nil || len(evs) != 1 || evs[0].Link != ev.Link || string(evs[0].Data) != `{"findings":[]}` {
		t.Fatalf("list evidence: %+v %v", evs, err)
	}
	if err := s.DeleteJournalEvidence(ctx, "inc-other", ev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete evidence of another entry: %v", err)
	}

	if _, err := s.AddJournalEvidence(ctx, JournalEvidence{EntryID: e.ID, Kind: "audit", Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJournalEntry(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.ListJournalEvidence(ctx, e.ID); len(evs) != 0 {
		t.Fatalf("evidence left after the entry was deleted: %d", len(evs))
	}
	if _, err := s.GetJournalEntry(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if err := s.DeleteJournalEntry(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestJournalDrafts(t *testing.T) {
	s, _, _ := openTest(t)
	ctx := context.Background()
	c, _ := s.EnsureCluster(ctx, "c-k3s", "k3s")
	t1 := t0.Add(-time.Hour).Truncate(time.Second)
	for _, d := range []JournalDraft{
		{ID: "draft-a", ClusterID: c.ID, Started: t1, Body: []byte(`{"v":1}`)},
		{ID: "draft-b", ClusterID: c.ID, Started: t1.Add(time.Minute), Body: []byte(`{"v":2}`)},
		{ID: "draft-a", ClusterID: c.ID, Started: t1, Body: []byte(`{"v":3}`)}, // replaced
	} {
		if err := s.SaveJournalDraft(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := s.GetJournalDraft(ctx, "draft-a"); err != nil || string(d.Body) != `{"v":3}` || !d.Started.Equal(t1) {
		t.Fatalf("get: %+v %v", d, err)
	}
	if d, err := s.LatestJournalDraft(ctx, c.ID); err != nil || d.ID != "draft-b" {
		t.Fatalf("latest: %+v %v", d, err)
	}
	if _, err := s.LatestJournalDraft(ctx, "c-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("latest of unknown: %v", err)
	}
	if ds, err := s.ListJournalDrafts(ctx); err != nil || len(ds) != 2 || ds[0].ID != "draft-b" {
		t.Fatalf("list: %d %v", len(ds), err)
	}
	if err := s.SaveJournalDraft(ctx, JournalDraft{ID: "x", ClusterID: "c-none", Started: t1, Body: []byte(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	if _, err := s.GetJournalDraft(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}
