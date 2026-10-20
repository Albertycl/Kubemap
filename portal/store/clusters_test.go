package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdateClusterInfo(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	info := ClusterInfo{AgentVersion: "v0.1.0", SourceIP: "192.168.32.36", KubernetesVersion: "v1.34.8+k3s1", NodeCount: 1, ClusterUID: "uid-1"}
	if err := s.UpdateClusterInfo(ctx, c.ID, info); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCluster(ctx, c.ID)
	if got.AgentVersion != info.AgentVersion || got.SourceIP != info.SourceIP || got.KubernetesVersion != info.KubernetesVersion ||
		got.NodeCount != 1 || got.ClusterUID != "uid-1" {
		t.Fatalf("GetCluster = %+v", got)
	}
	if err := s.UpdateClusterInfo(ctx, "c-missing", info); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cluster: err = %v", err)
	}
}

func TestListAudit(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	for _, a := range []string{"one", "two", "three"} {
		if err := s.WriteAudit(ctx, AuditEntry{User: "admin", Action: a, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAudit(ctx, 2)
	if err != nil || len(got) != 2 || got[0].Action != "three" || got[1].Action != "two" || !got[0].Time.Equal(t0) {
		t.Fatalf("ListAudit = %+v, %v", got, err)
	}
}

func TestResetConnections(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	a := mustCluster(t, s, "active-one")
	s.UpsertNode(ctx, Node{ClusterID: a.ID, Name: "n1", Status: "connected"})
	if err := s.ResetConnections(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetCluster(ctx, a.ID); c.Status != StatusDisconnected {
		t.Fatalf("active cluster after reset: %s", c.Status)
	}
	var st string
	s.db.QueryRow(`SELECT status FROM nodes`).Scan(&st)
	if st != "disconnected" {
		t.Fatalf("node status after reset: %s", st)
	}
}

func TestSetNodeStatusAndSeenKeepsVersion(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	s.UpsertNode(ctx, Node{ClusterID: c.ID, Name: "n1", Status: "connected", LastSeen: t0})
	if err := s.SetNodeStatus(ctx, c.ID, "n1", "disconnected"); err != nil {
		t.Fatal(err)
	}
	var st string
	var seen int64
	s.db.QueryRow(`SELECT status, last_seen FROM nodes`).Scan(&st, &seen)
	if st != "disconnected" || seen != t0.Unix() {
		t.Fatalf("node %s %d", st, seen)
	}
	if err := s.SetNodeStatus(ctx, c.ID, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	s.MarkClusterSeen(ctx, c.ID, "v1")
	s.MarkClusterSeen(ctx, c.ID, "")
	if got, _ := s.GetCluster(ctx, c.ID); got.AgentVersion != "v1" {
		t.Fatalf("agent version %q, want v1 kept", got.AgentVersion)
	}
}

func TestListRollupsAndNodes(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c := mustCluster(t, s, "k3s-home")
	o := mustCluster(t, s, "other")
	s.WriteRollups(ctx, []Rollup{
		{ClusterID: c.ID, Node: "n1", Bucket: t0.Add(-2 * time.Hour), CPUPct: 1},
		{ClusterID: c.ID, Node: "n1", Bucket: t0.Add(-time.Minute), CPUPct: 2, PSIMemory: 0.5, HasPSI: true},
		{ClusterID: c.ID, Node: "n1", Bucket: t0.Add(-2 * time.Minute), CPUPct: 3},
		{ClusterID: c.ID, Node: "n2", Bucket: t0.Add(-time.Minute), CPUPct: 4},
		{ClusterID: o.ID, Node: "n1", Bucket: t0.Add(-time.Minute), CPUPct: 5},
	})
	got, err := s.ListRollups(ctx, c.ID, "n1", t0.Add(-time.Hour))
	if err != nil || len(got) != 2 || got[0].CPUPct != 3 || got[1].CPUPct != 2 || got[1].PSIMemory != 0.5 || !got[1].HasPSI || got[0].HasPSI || !got[1].Bucket.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("ListRollups = %+v, %v", got, err)
	}
	s.UpsertNode(ctx, Node{ClusterID: c.ID, Name: "n2", Status: "connected", LastSeen: t0})
	s.UpsertNode(ctx, Node{ClusterID: c.ID, Name: "n1", Status: "disconnected", LastSeen: t0.Add(-time.Hour)})
	s.UpsertNode(ctx, Node{ClusterID: o.ID, Name: "x", Status: "connected", LastSeen: t0})
	nodes, err := s.ListNodes(ctx, c.ID)
	if err != nil || len(nodes) != 2 || nodes[0].Name != "n1" || nodes[0].Status != "disconnected" || !nodes[1].LastSeen.Equal(t0) {
		t.Fatalf("ListNodes = %+v, %v", nodes, err)
	}
}

func TestEnsureClusterIsActiveAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	c, err := s.EnsureCluster(ctx, "local", "this-cluster")
	if err != nil || c.Status != StatusActive || c.ID != "local" || c.AcceptedAt.IsZero() {
		t.Fatalf("EnsureCluster = %+v, %v", c, err)
	}
	if err := s.MarkClusterDisconnected(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	again, err := s.EnsureCluster(ctx, "local", "renamed")
	if err != nil || again.Status != StatusDisconnected || again.Name != "renamed" {
		t.Fatalf("second EnsureCluster = %+v, %v (status must be kept, name updated)", again, err)
	}
	if cs, _ := s.ListClusters(ctx); len(cs) != 1 {
		t.Fatalf("clusters = %d, want 1", len(cs))
	}
}
