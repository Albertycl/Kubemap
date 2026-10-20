package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/metrics"
	"kubemap/portal/resources"
	"kubemap/portal/store"
)

// Evidence kinds.
const (
	EvFindings = "findings" // the health rules' findings (problem finder)
	EvEvents   = "events"   // warning events
	EvMetrics  = "metrics"  // node metrics: the latest report and the last hour
	EvAudit    = "audit"    // audit log entries
)

const (
	// maxEvidenceBytes bounds one snapshot as stored.
	maxEvidenceBytes = 256 << 10
	captureTimeout   = 20 * time.Second
	maxEvents        = 200
	maxAuditEntries  = 100
	metricsWindow    = time.Hour
	maxNote          = 2000
)

type evidenceRequest struct {
	Kind      string `json:"kind"`
	ClusterID string `json:"clusterId"`
	Namespace string `json:"namespace"` // findings, events: one namespace
	Node      string `json:"node"`      // metrics: one node
	Note      string `json:"note"`
}

// snapshot is what a capture returns.
type snapshot struct {
	summary, link string
	data          any
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

func (a *API) addEvidence(w http.ResponseWriter, r *http.Request) {
	e, ok := a.load(w, r)
	if !ok {
		return
	}
	var req evidenceRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	bad := func(msg string) { httpjson.Error(w, http.StatusBadRequest, "bad_request", msg) }
	switch {
	case req.Namespace != "" && !nameRE.MatchString(req.Namespace):
		bad("the namespace is not a valid name")
		return
	case req.Node != "" && !nameRE.MatchString(req.Node):
		bad("the node is not a valid name")
		return
	case len(req.Note) > maxNote:
		bad(fmt.Sprintf("the note is longer than %d characters", maxNote))
		return
	}
	var c store.Cluster
	if req.ClusterID != "" || req.Kind != EvAudit {
		var err error
		c, err = a.store.GetCluster(r.Context(), req.ClusterID)
		if errors.Is(err, store.ErrNotFound) {
			httpjson.Error(w, http.StatusNotFound, "not_found", "no such cluster")
			return
		}
		if err != nil {
			internal(w)
			return
		}
	}
	if n, err := a.store.CountJournalEvidence(r.Context(), e.ID); err != nil {
		internal(w)
		return
	} else if n >= store.MaxEvidencePerEntry {
		httpjson.Error(w, http.StatusConflict, "too_many", fmt.Sprintf("an entry keeps at most %d snapshots; remove one first", store.MaxEvidencePerEntry))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
	defer cancel()
	var snap snapshot
	var err error
	switch req.Kind {
	case EvFindings, EvEvents:
		if req.Kind == EvFindings {
			snap, err = a.captureFindings(ctx, c, req.Namespace)
		} else {
			snap, err = a.captureEvents(ctx, c, req.Namespace)
		}
	case EvMetrics:
		snap, err = a.captureMetrics(ctx, c, req.Node)
	case EvAudit:
		if !a.adminOnly(w, r, "copy audit log entries") {
			return
		}
		snap, err = a.captureAudit(ctx, c)
	default:
		bad("kind must be findings, events, metrics or audit")
		return
	}
	if err != nil {
		var f *httpjson.Failure
		if errors.As(err, &f) {
			httpjson.Error(w, f.Status, f.Code, f.Message)
		} else {
			agentcall.Error(w, err, "choose one namespace")
		}
		return
	}
	data, err := json.Marshal(snap.data)
	if err != nil {
		internal(w)
		return
	}
	if len(data) > maxEvidenceBytes {
		httpjson.Error(w, http.StatusUnprocessableEntity, "too_large",
			fmt.Sprintf("the snapshot is larger than %d KiB; choose one namespace or node", maxEvidenceBytes>>10))
		return
	}
	ev, err := a.store.AddJournalEvidence(r.Context(), store.JournalEvidence{EntryID: e.ID, Kind: req.Kind, ClusterID: c.ID,
		Summary: snap.summary, Link: snap.link, Note: strings.TrimSpace(req.Note), Data: data, CapturedBy: user(r)})
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such journal entry")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if err := a.writeAudit(r, c.ID, "journal.evidence.add", target(e.ID, req.Kind+": "+snap.summary)); err != nil {
		internal(w)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"evidence": toEvidence(ev)})
}

func (a *API) deleteEvidence(w http.ResponseWriter, r *http.Request) {
	e, ok := a.load(w, r)
	if !ok {
		return
	}
	eid := r.PathValue("eid")
	err := a.store.DeleteJournalEvidence(r.Context(), e.ID, eid)
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such evidence")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if err := a.writeAudit(r, "", "journal.evidence.delete", target(e.ID, eid)); err != nil {
		internal(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func clusterLink(id string, parts ...string) string {
	l := "#/clusters/" + url.PathEscape(id)
	for _, p := range parts {
		l += "/" + url.PathEscape(p)
	}
	return l
}

type findingView struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Object   string `json:"object"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	Fix      string `json:"fix,omitempty"`
}

func (a *API) captureFindings(ctx context.Context, c store.Cluster, ns string) (snapshot, error) {
	if a.Findings == nil {
		return snapshot{}, &httpjson.Failure{Status: http.StatusNotImplemented, Code: "unsupported", Message: "findings cannot be captured here"}
	}
	rep, err := a.Findings(ctx, c.ID, ns)
	if err != nil {
		return snapshot{}, err
	}
	n := map[string]int{}
	fs := make([]findingView, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		n[f.Severity]++
		fs = append(fs, findingView{Rule: f.Rule, Severity: f.Severity, Object: f.Object, Title: f.Title, Evidence: f.Evidence, Fix: f.Fix})
	}
	summary := fmt.Sprintf("%d critical, %d warning, %d info finding(s) from %d rules on %s",
		n[protocol.SeverityCritical], n[protocol.SeverityWarning], n[protocol.SeverityInfo], len(rep.Rules), c.Name)
	link := clusterLink(c.ID, "problems")
	if ns != "" {
		summary += ", namespace " + ns
	}
	return snapshot{summary: summary, link: link, data: map[string]any{
		"cluster": c.Name, "namespace": ns, "time": rep.Time, "rules": len(rep.Rules), "findings": fs, "notes": rep.Notes,
	}}, nil
}

type eventView struct {
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Namespace string    `json:"namespace"`
	Object    string    `json:"object"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
}

// kubeEvent is the part of a core/v1 Event the snapshot keeps.
type kubeEvent struct {
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int       `json:"count"`
	FirstTimestamp time.Time `json:"firstTimestamp"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
}

func (a *API) captureEvents(ctx context.Context, c store.Cluster, ns string) (snapshot, error) {
	if a.Agent == nil {
		return snapshot{}, &httpjson.Failure{Status: http.StatusNotImplemented, Code: "unsupported", Message: "events cannot be captured here"}
	}
	items, truncated, err := resources.ListAll(ctx, a.Agent, c.ID, protocol.Request{Resource: "events", Namespace: ns})
	if err != nil {
		return snapshot{}, err
	}
	evs := make([]eventView, 0, len(items))
	for _, raw := range items {
		var k kubeEvent
		if json.Unmarshal(raw, &k) != nil {
			continue
		}
		t := k.LastTimestamp
		if t.IsZero() {
			t = k.EventTime
		}
		if t.IsZero() {
			t = k.FirstTimestamp
		}
		evs = append(evs, eventView{Time: t, Type: k.Type, Namespace: k.Metadata.Namespace,
			Object: k.InvolvedObject.Kind + "/" + k.InvolvedObject.Name, Reason: k.Reason, Message: k.Message, Count: max(k.Count, 1)})
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.After(evs[j].Time) })
	total := len(evs)
	if len(evs) > maxEvents {
		evs = evs[:maxEvents]
	}
	summary := fmt.Sprintf("%d warning event(s) on %s", total, c.Name)
	link := clusterLink(c.ID, "pods")
	if ns != "" {
		summary += ", namespace " + ns
		link = clusterLink(c.ID, "pods", ns)
	}
	if total > maxEvents || truncated {
		summary += fmt.Sprintf(" (the newest %d kept)", len(evs))
	}
	return snapshot{summary: summary, link: link, data: map[string]any{"cluster": c.Name, "namespace": ns, "events": evs}}, nil
}

type rollupView struct {
	Time      time.Time `json:"time"`
	CPUPct    float64   `json:"cpuPct"`
	MemPct    float64   `json:"memPct"`
	SwapPct   float64   `json:"swapPct"`
	DiskPct   float64   `json:"diskPct"`
	InodesPct float64   `json:"inodesPct"`
	PSICPU    *float64  `json:"psiCpu,omitempty"`
	PSIMemory *float64  `json:"psiMemory,omitempty"`
	PSIIO     *float64  `json:"psiIo,omitempty"`
}

type nodeMetrics struct {
	Node   string `json:"node"`
	Status string `json:"status"`
	// Now is the node agent's latest report, if the portal has one.
	Now     *rollupView  `json:"now,omitempty"`
	Rollups []rollupView `json:"rollups"`
}

func (a *API) captureMetrics(ctx context.Context, c store.Cluster, node string) (snapshot, error) {
	nodes, err := a.store.ListNodes(ctx, c.ID)
	if err != nil {
		return snapshot{}, err
	}
	since := a.now().Add(-metricsWindow)
	now := map[string]*rollupView{}
	if a.Reports != nil {
		for _, m := range a.Reports(c.ID) {
			p, hasPSI, _ := metrics.Derive(&m)
			v := &rollupView{Time: m.Time, CPUPct: p.CPU, MemPct: p.Memory, SwapPct: p.Swap, DiskPct: p.Disk, InodesPct: p.Inodes}
			if hasPSI {
				v.PSICPU, v.PSIMemory, v.PSIIO = &p.PSICPU, &p.PSIMem, &p.PSIIO
			}
			now[m.NodeName] = v
		}
	}
	var out []nodeMetrics
	var latest []string
	for _, n := range nodes {
		if node != "" && n.Name != node {
			continue
		}
		rs, err := a.store.ListRollups(ctx, c.ID, n.Name, since)
		if err != nil {
			return snapshot{}, err
		}
		nm := nodeMetrics{Node: n.Name, Status: n.Status, Now: now[n.Name], Rollups: make([]rollupView, 0, len(rs))}
		for _, r := range rs {
			v := rollupView{Time: r.Bucket, CPUPct: r.CPUPct, MemPct: r.MemPct, SwapPct: r.SwapPct, DiskPct: r.DiskPct, InodesPct: r.InodesPct}
			if r.HasPSI {
				v.PSICPU, v.PSIMemory, v.PSIIO = &r.PSICPU, &r.PSIMemory, &r.PSIIO
			}
			nm.Rollups = append(nm.Rollups, v)
		}
		l := nm.Now
		if k := len(nm.Rollups); l == nil && k > 0 {
			l = &nm.Rollups[k-1]
		}
		if l != nil {
			latest = append(latest, fmt.Sprintf("%s CPU %.0f%%, memory %.0f%%, disk %.0f%%", n.Name, l.CPUPct, l.MemPct, l.DiskPct))
		}
		out = append(out, nm)
	}
	if node != "" && len(out) == 0 {
		return snapshot{}, &httpjson.Failure{Status: http.StatusNotFound, Code: "not_found", Message: "no such node in this cluster"}
	}
	summary := fmt.Sprintf("metrics of %d node(s) on %s", len(out), c.Name)
	if len(latest) > 0 {
		summary += "; latest: " + strings.Join(latest, "; ")
	} else {
		summary += "; no report in the last hour"
	}
	link := "#/servers"
	if node != "" {
		link = clusterLink(c.ID, "nodes", node)
	}
	if out == nil {
		out = []nodeMetrics{}
	}
	return snapshot{summary: summary, link: link, data: map[string]any{"cluster": c.Name, "since": since.UTC(), "nodes": out}}, nil
}

type auditView struct {
	Time      time.Time `json:"time"`
	User      string    `json:"user"`
	ClusterID string    `json:"clusterId"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Result    string    `json:"result"`
}

func toAuditView(e store.AuditEntry) auditView {
	return auditView{Time: e.Time, User: e.User, ClusterID: e.ClusterID, Action: e.Action, Target: e.Target, Result: e.Result}
}

// captureAudit keeps the newest audit entries, of one cluster if c is set.
func (a *API) captureAudit(ctx context.Context, c store.Cluster) (snapshot, error) {
	es, err := a.store.ListAudit(ctx, 1000)
	if err != nil {
		return snapshot{}, err
	}
	out := []auditView{}
	for _, e := range es {
		if c.ID != "" && e.ClusterID != c.ID {
			continue
		}
		if out = append(out, toAuditView(e)); len(out) == maxAuditEntries {
			break
		}
	}
	summary := fmt.Sprintf("the newest %d audit log entries", len(out))
	link := "#/audit"
	if c.ID != "" {
		summary += " for " + c.Name
		link += "?cluster=" + url.QueryEscape(c.ID)
	}
	return snapshot{summary: summary, link: link, data: map[string]any{"entries": out}}, nil
}
