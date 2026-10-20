package incidents

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

// Defaults.
const (
	// DefaultInterval is how often each active cluster is checked.
	DefaultInterval = 60 * time.Second
	// DefaultWindow: findings that come back within this time after a draft
	// resolved reopen it (one episode); later ones start a new draft.
	DefaultWindow = 30 * time.Minute
	// MaxTimeline and MaxEvidence bound one draft; the oldest entries stay.
	MaxTimeline = 500
	MaxEvidence = 200
)

// DefaultIgnoreNamespaces are left out: the practice labs in kubemap-demo
// fail on purpose, all the time, and would keep a cluster's draft open.
var DefaultIgnoreNamespaces = []string{"kubemap-demo"}

// Triggers says whether a finding opens or feeds a draft: every critical
// finding, and evicted pods (a warning in the rules, but an incident in
// practice: 28 Sep 2026).
func Triggers(f protocol.HealthFinding) bool {
	return f.Severity == protocol.SeverityCritical || f.Rule == "pod.evicted"
}

// Checker runs health.check on one cluster (health.API.Check).
type Checker func(ctx context.Context, clusterID string) (*protocol.HealthReport, error)

// Drafter turns problem finder results into draft incidents.
type Drafter struct {
	Sink     Sink
	Check    Checker
	Clusters func(ctx context.Context) ([]store.Cluster, error)
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration
	Window   time.Duration
	// IgnoreNamespaces: findings about objects there are left out (and
	// items there are dropped from grouped findings).
	IgnoreNamespaces []string

	watching map[string]bool // clusters already observed once since start
}

func (d *Drafter) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run checks every active cluster each Interval until ctx ends.
func (d *Drafter) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick checks every active cluster once. A cluster whose agent cannot be
// asked is skipped: an unanswered check is not a cleared finding.
func (d *Drafter) Tick(ctx context.Context) {
	clusters, err := d.Clusters(ctx)
	if err != nil {
		d.log().Warn("incident drafts: list clusters", "err", err)
		return
	}
	for _, c := range clusters {
		if c.Status != store.StatusActive {
			continue
		}
		rep, err := d.Check(ctx, c.ID)
		if err != nil {
			d.log().Debug("incident drafts: health.check", "cluster", c.ID, "err", err)
			continue
		}
		if err := d.Observe(ctx, c, rep); err != nil {
			d.log().Warn("incident drafts: save", "cluster", c.ID, "err", err)
		}
	}
}

func (d *Drafter) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// key identifies a finding across checks.
func key(f protocol.HealthFinding) string { return f.Rule + "|" + f.Object }

// relevant keeps the triggering findings, without the ignored namespaces.
func (d *Drafter) relevant(fs []protocol.HealthFinding) map[string]protocol.HealthFinding {
	ignored := func(ns string) bool { return slices.Contains(d.IgnoreNamespaces, ns) }
	out := map[string]protocol.HealthFinding{}
	for _, f := range fs {
		if !Triggers(f) {
			continue
		}
		if kind, rest, ok := strings.Cut(f.Object, "/"); ok && (kind == "Pod" || kind == "Namespace") {
			if ns, _, _ := strings.Cut(rest, "/"); ignored(ns) {
				continue
			}
		}
		if len(f.Items) > 0 {
			var keep []string
			for _, it := range f.Items {
				if ns, _, _ := strings.Cut(it, "/"); !ignored(ns) {
					keep = append(keep, it)
				}
			}
			if len(keep) == 0 {
				continue
			}
			if dropped := len(f.Items) - len(keep); dropped > 0 {
				// Grouped titles start with the count ("41 pods were evicted").
				if old := strconv.Itoa(f.Count); strings.HasPrefix(f.Title, old+" ") {
					f.Title = strconv.Itoa(f.Count-dropped) + strings.TrimPrefix(f.Title, old)
				}
				f.Count -= dropped
				f.Items = keep
			}
		}
		out[key(f)] = f
	}
	return out
}

var sevRank = map[string]int{protocol.SeverityCritical: 3, protocol.SeverityWarning: 2, protocol.SeverityInfo: 1}

// Observe applies one health report of a cluster to its drafts.
func (d *Drafter) Observe(ctx context.Context, c store.Cluster, rep *protocol.HealthReport) error {
	now := d.now().UTC()
	window := d.Window
	if window <= 0 {
		window = DefaultWindow
	}
	if d.watching == nil {
		d.watching = map[string]bool{}
	}
	firstLook := !d.watching[c.ID]
	d.watching[c.ID] = true

	active := d.relevant(rep.Findings)
	dr, err := d.Sink.Latest(ctx, c.ID)
	if err != nil {
		return err
	}
	current := dr != nil && (dr.Status == StatusOpen || now.Sub(dr.Resolved) < window)

	if len(active) == 0 {
		if dr == nil || dr.Status != StatusOpen {
			return nil
		}
		for _, k := range sortedKeys(dr.Active) {
			a := dr.Active[k]
			addEntry(dr, Entry{Time: now, Event: "Cleared: " + a.Title, Source: "kubemap", Rule: a.Rule, Object: a.Object})
		}
		dr.Active = map[string]Active{}
		dr.Status, dr.Resolved, dr.Updated = StatusResolved, now, now
		addEntry(dr, Entry{Time: now, Event: "Every triggering finding has cleared. Add what was done, the decisions and the lessons.", Source: "kubemap"})
		return d.Sink.Save(ctx, dr)
	}

	changed, refreshed := false, false
	if !current {
		dr = &Draft{ID: fmt.Sprintf("draft-%s-%d", c.ID, now.Unix()), ClusterID: c.ID, ClusterName: c.Name,
			Status: StatusOpen, Started: now, Active: map[string]Active{}, Timeline: []Entry{}, Evidence: []Evidence{},
			PresentAtStart: firstLook}
		changed = true
	} else if dr.Status == StatusResolved {
		dr.Status, dr.Resolved = StatusOpen, time.Time{}
		addEntry(dr, Entry{Time: now, Event: "Findings are back within " + window.String() + " of resolving: same episode.", Source: "kubemap"})
		changed = true
	}
	if dr.ClusterName == "" {
		dr.ClusterName = c.Name
	}
	link := "#/clusters/" + c.ID + "/problems"

	keys := make([]string, 0, len(active))
	for k := range active {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // worst first, then by key
		fi, fj := active[keys[i]], active[keys[j]]
		if sevRank[fi.Severity] != sevRank[fj.Severity] {
			return sevRank[fi.Severity] > sevRank[fj.Severity]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		f := active[k]
		a := Active{Rule: f.Rule, Object: f.Object, Severity: f.Severity, Title: f.Title, Count: f.Count}
		old, seen := dr.Active[k]
		switch {
		case !seen:
			ev := "First seen: " + f.Title
			if firstLook {
				ev += " (already present when Kubemap started watching)"
			}
			addEntry(dr, Entry{Time: now, Event: ev + ". " + f.Evidence, Source: "kubemap", Rule: f.Rule, Object: f.Object})
			addEvidence(dr, Evidence{Time: now, Kind: "finding", Link: link, Finding: f})
		case old.Severity != a.Severity || old.Count != a.Count:
			addEntry(dr, Entry{Time: now, Event: "Changed: " + describeChange(old, a) + ". " + f.Evidence, Source: "kubemap", Rule: f.Rule, Object: f.Object})
			addEvidence(dr, Evidence{Time: now, Kind: "finding", Link: link, Finding: f})
		default:
			// Titles carry live numbers ("Disk 14.0% free"): keep the
			// newest without a timeline line, or every check would add one.
			if old.Title != a.Title {
				dr.Active[k] = a
				refreshed = true
			}
			continue
		}
		dr.Active[k] = a
		if sevRank[f.Severity] > sevRank[dr.Severity] {
			dr.Severity = f.Severity
		}
		changed = true
	}
	for _, k := range sortedKeys(dr.Active) {
		if _, still := active[k]; !still {
			a := dr.Active[k]
			addEntry(dr, Entry{Time: now, Event: "Cleared: " + a.Title, Source: "kubemap", Rule: a.Rule, Object: a.Object})
			delete(dr.Active, k)
			changed = true
		}
	}
	if !changed {
		if refreshed {
			return d.Sink.Save(ctx, dr) // newest numbers; not a change for the timeline
		}
		return nil
	}
	dr.Title = title(dr, active, keys)
	dr.Detection = detection(dr)
	dr.Updated = now
	return d.Sink.Save(ctx, dr)
}

func describeChange(old, a Active) string {
	var parts []string
	if old.Severity != a.Severity {
		parts = append(parts, old.Severity+" → "+a.Severity)
	}
	if old.Count != a.Count {
		parts = append(parts, fmt.Sprintf("%d → %d", old.Count, a.Count))
	}
	parts = append(parts, a.Title)
	return strings.Join(parts, ", ")
}

// title keeps the first finding's title and says how many more there are.
func title(dr *Draft, active map[string]protocol.HealthFinding, keys []string) string {
	first := ""
	for _, e := range dr.Timeline {
		if e.Rule != "" {
			first = strings.TrimPrefix(e.Event, "First seen: ")
			first, _, _ = strings.Cut(first, ". ")
			first = strings.TrimSuffix(first, " (already present when Kubemap started watching)")
			if e.Object != "" && e.Object != "Cluster" {
				first += " (" + e.Object + ")"
			}
			break
		}
	}
	if first == "" && len(keys) > 0 {
		first = active[keys[0]].Title
	}
	rules := map[string]bool{}
	for _, e := range dr.Timeline {
		if e.Rule != "" {
			rules[e.Rule+"|"+e.Object] = true
		}
	}
	if n := len(rules) - 1; n > 0 {
		return fmt.Sprintf("%s, and %d more problem(s)", first, n)
	}
	return first
}

func detection(dr *Draft) string {
	for _, e := range dr.Timeline {
		if e.Rule != "" {
			s := fmt.Sprintf("Kubemap's problem finder: rule %s on %s, %s", e.Rule, e.Object, e.Time.Format("2006-01-02 15:04 MST"))
			if dr.PresentAtStart {
				s += " (the problem was already there when Kubemap started watching; it began earlier)"
			}
			return s + "."
		}
	}
	return "Kubemap's problem finder."
}

func addEntry(dr *Draft, e Entry) {
	if len(dr.Timeline) < MaxTimeline {
		dr.Timeline = append(dr.Timeline, e)
	}
}

func addEvidence(dr *Draft, e Evidence) {
	if len(dr.Evidence) < MaxEvidence {
		dr.Evidence = append(dr.Evidence, e)
	}
}

func sortedKeys(m map[string]Active) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
