package store

import (
	"context"
	"testing"
	"time"
)

// Review KM-44 L9: old audit entries, used or expired tokens, and old
// drafts are deleted; each cluster's newest draft stays.
func TestRetention(t *testing.T) {
	s, clk, _ := openTest(t)
	ctx := context.Background()
	c := mustCluster(t, s, "k3s")

	s.WriteAudit(ctx, AuditEntry{Time: t0.Add(-400 * 24 * time.Hour), User: "a", Action: "old", Result: "ok"})
	s.WriteAudit(ctx, AuditEntry{Time: t0.Add(-10 * 24 * time.Hour), User: "a", Action: "recent", Result: "ok"})
	if n, _ := s.PruneAudit(ctx, 0); n != 0 {
		t.Fatal("keep 0 must keep everything")
	}
	if n, err := s.PruneAudit(ctx, 365*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("prune audit: %d %v", n, err)
	}
	if es, _ := s.ListAudit(ctx, 10); len(es) != 1 || es[0].Action != "recent" {
		t.Fatalf("left %+v", es)
	}

	now := clk.now()
	for i, age := range []time.Duration{200 * 24 * time.Hour, 120 * 24 * time.Hour, time.Hour} {
		s.SaveJournalDraft(ctx, JournalDraft{ID: "d" + string(rune('0'+i)), ClusterID: c.ID, Started: now.Add(-age), Body: []byte(`{}`)})
	}
	c2 := mustCluster(t, s, "old-only")
	s.SaveJournalDraft(ctx, JournalDraft{ID: "only", ClusterID: c2.ID, Started: now.Add(-300 * 24 * time.Hour), Body: []byte(`{}`)})
	if n, err := s.PruneDrafts(ctx); err != nil || n != 2 {
		t.Fatalf("prune drafts: %d %v", n, err)
	}
	ds, _ := s.ListJournalDrafts(ctx)
	if len(ds) != 2 || ds[0].ID != "d2" || ds[1].ID != "only" {
		t.Fatalf("left %+v (the newest of each cluster)", ds)
	}
}
