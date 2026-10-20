// Package incidents drafts incidents from what Kubemap sees (KM-90). When the
// problem finder (the health.check rules, KM-61) raises a critical finding
// (DiskPressure, CrashLoopBackOff, a container killed with exit 137, a node
// not Ready …) or reports evicted pods, the portal opens a draft incident for
// that cluster, and keeps its timeline and evidence up to date: when each
// finding was first seen, how it changed, and when it cleared. A person adds
// the actions, decisions and lessons in the incident journal (KM-89).
//
// The drafter only reads: it asks the cluster agents for health.check, like
// the problem finder page does. Drafts are stored through a Sink; the
// journal implements it with its database, MemorySink keeps them in memory.
package incidents

import (
	"context"
	"sort"
	"sync"
	"time"

	"kubemap/pkg/protocol"
)

// Draft statuses. A draft is never closed by Kubemap: "resolved" only says
// that every triggering finding has cleared; the journal turns a draft into
// an incident when a person has written it up.
const (
	StatusOpen     = "open"     // at least one triggering finding is active
	StatusResolved = "resolved" // every triggering finding has cleared
)

// Draft is one draft incident: the part of a journal incident Kubemap can
// fill in by itself. The field names follow the journal's incident
// (KM-89): title, cluster, severity, detection, timeline, evidence.
type Draft struct {
	ID          string `json:"id"` // "draft-<cluster ID>-<start, unix seconds>"
	ClusterID   string `json:"clusterId"`
	ClusterName string `json:"clusterName"`
	Status      string `json:"status"`
	// Title is generated from the first triggering finding.
	Title string `json:"title"`
	// Severity is the worst severity seen during the episode.
	Severity string    `json:"severity"`
	Started  time.Time `json:"started"` // first finding seen
	Updated  time.Time `json:"updated"` // last change
	Resolved time.Time `json:"resolved,omitzero"`
	// Detection says how it was found, in one sentence.
	Detection string `json:"detection"`
	// PresentAtStart is true when the first findings were already there
	// when Kubemap started watching: the real start is earlier.
	PresentAtStart bool       `json:"presentAtStart,omitempty"`
	Timeline       []Entry    `json:"timeline"`
	Evidence       []Evidence `json:"evidence"`
	// Active are the triggering findings active now, by key (rule|object).
	Active map[string]Active `json:"active"`
}

// Entry is one line of the timeline.
type Entry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	Source string    `json:"source"`           // "kubemap" for drafted lines; the journal uses others for people
	Rule   string    `json:"rule,omitempty"`   // the health rule
	Object string    `json:"object,omitempty"` // "Node/k8server", "Pod/ns/name", "Cluster"
}

// Evidence is a snapshot of a finding at the time of an entry, with a link
// to where it came from.
type Evidence struct {
	Time    time.Time              `json:"time"`
	Kind    string                 `json:"kind"` // "finding"
	Link    string                 `json:"link"` // a portal route, "#/clusters/<id>/problems"
	Finding protocol.HealthFinding `json:"finding"`
}

// Active is what the drafter remembers about an active finding, to see
// changes.
type Active struct {
	Rule     string `json:"rule"`
	Object   string `json:"object"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Count    int    `json:"count,omitempty"`
}

// Sink stores drafts. The journal (KM-89) implements it with its database.
type Sink interface {
	// Latest returns the newest draft of a cluster, or nil.
	Latest(ctx context.Context, clusterID string) (*Draft, error)
	// Save creates or replaces a draft by ID.
	Save(ctx context.Context, d *Draft) error
	// List returns every draft, newest first.
	List(ctx context.Context) ([]Draft, error)
	// Get returns one draft, or nil.
	Get(ctx context.Context, id string) (*Draft, error)
}

// MemorySink keeps drafts in memory (lost when the portal restarts).
type MemorySink struct {
	mu     sync.Mutex
	drafts map[string]Draft
}

func (m *MemorySink) Latest(_ context.Context, clusterID string) (*Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Draft
	for _, d := range m.drafts {
		if d.ClusterID == clusterID && (best == nil || d.Started.After(best.Started)) {
			c := clone(d)
			best = &c
		}
	}
	return best, nil
}

func (m *MemorySink) Save(_ context.Context, d *Draft) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts == nil {
		m.drafts = map[string]Draft{}
	}
	m.drafts[d.ID] = clone(*d)
	return nil
}

func (m *MemorySink) List(_ context.Context) ([]Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Draft, 0, len(m.drafts))
	for _, d := range m.drafts {
		out = append(out, clone(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

func (m *MemorySink) Get(_ context.Context, id string) (*Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[id]
	if !ok {
		return nil, nil
	}
	c := clone(d)
	return &c, nil
}

// clone copies the slices and map, so a caller cannot change a stored draft.
func clone(d Draft) Draft {
	d.Timeline = append([]Entry(nil), d.Timeline...)
	d.Evidence = append([]Evidence(nil), d.Evidence...)
	a := make(map[string]Active, len(d.Active))
	for k, v := range d.Active {
		a[k] = v
	}
	d.Active = a
	return d
}
