package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // time zones in the minimal image, for the write-up's timeline

	"kubemap/portal/httpjson"
	"kubemap/portal/incidents"
	"kubemap/portal/store"
)

// DraftSink stores the KM-90 draft incidents in the portal database, so they
// survive a restart. It implements incidents.Sink.
type DraftSink struct{ Store store.Store }

var _ incidents.Sink = DraftSink{}

func decodeDraft(d store.JournalDraft) (*incidents.Draft, error) {
	var out incidents.Draft
	if err := json.Unmarshal(d.Body, &out); err != nil {
		return nil, fmt.Errorf("draft %s: %w", d.ID, err)
	}
	return &out, nil
}

func (s DraftSink) Latest(ctx context.Context, clusterID string) (*incidents.Draft, error) {
	d, err := s.Store.LatestJournalDraft(ctx, clusterID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeDraft(d)
}

func (s DraftSink) Save(ctx context.Context, d *incidents.Draft) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.Store.SaveJournalDraft(ctx, store.JournalDraft{ID: d.ID, ClusterID: d.ClusterID, Started: d.Started, Body: b})
}

func (s DraftSink) List(ctx context.Context) ([]incidents.Draft, error) {
	ds, err := s.Store.ListJournalDrafts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]incidents.Draft, 0, len(ds))
	for _, d := range ds {
		dr, err := decodeDraft(d)
		if err != nil {
			continue // one unreadable draft does not hide the others
		}
		out = append(out, *dr)
	}
	return out, nil
}

func (s DraftSink) Get(ctx context.Context, id string) (*incidents.Draft, error) {
	d, err := s.Store.GetJournalDraft(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeDraft(d)
}

// DraftSource is the source of an entry written up from a draft: one entry
// per draft.
func DraftSource(draftID string) string { return "draft:" + draftID }

// severityOf maps a finding severity to an incident severity.
func severityOf(s string) string {
	switch s {
	case "critical":
		return "critical"
	case "warning":
		return "major"
	}
	return "minor"
}

// FromDraft makes an incident record from a draft: the title, date,
// cluster, severity, detection and timeline Kubemap knows, with empty
// sections for what only a person can write (summary, root cause,
// decisions, follow-ups, STAR, lessons). Times are shown in loc.
func FromDraft(d incidents.Draft, loc *time.Location) Record {
	start := d.Started.In(loc)
	zone := loc.String()
	if zone == "Local" {
		zone = start.Format("MST")
	}
	intro := fmt.Sprintf("Drafted by Kubemap from its problem finder (draft %s). Times in %s.", d.ID, zone)
	if d.PresentAtStart {
		intro += " The first findings were already there when Kubemap started watching, so the real start is earlier."
	}
	status := "still open when written up"
	if d.Status == incidents.StatusResolved && !d.Resolved.IsZero() {
		status = "every finding cleared at " + d.Resolved.In(loc).Format("15:04")
	}
	day := start.Format(time.DateOnly)
	rows := make([]TimelineRow, 0, len(d.Timeline))
	for _, e := range d.Timeline {
		t := e.Time.In(loc)
		when := t.Format("15:04")
		if t.Format(time.DateOnly) != day {
			when = t.Format("01-02 15:04")
		}
		ev := strings.TrimSpace(strings.ReplaceAll(e.Event, "\n", " "))
		rows = append(rows, TimelineRow{Time: when, Event: ev})
	}
	if len(rows) > maxRows {
		cut := len(rows) - (maxRows - 1)
		rows = append(append(rows[:maxRows/2:maxRows/2], TimelineRow{Event: fmt.Sprintf("… %d more lines in draft %s", cut, d.ID)}),
			rows[len(rows)-(maxRows/2-1):]...)
	}
	sections := []Section{
		{Kind: SecIntro, Text: intro},
		{Kind: SecSummary, Heading: DefaultHeadings[SecSummary], Text: "Kubemap saw: " + d.Title + " (" + status + ")."},
		{Kind: SecTimeline, Heading: DefaultHeadings[SecTimeline], Timeline: rows},
		{Kind: SecRootCause, Heading: DefaultHeadings[SecRootCause]},
		{Kind: SecDetection, Heading: DefaultHeadings[SecDetection], Text: d.Detection},
		{Kind: SecFollowUps, Heading: DefaultHeadings[SecFollowUps], FollowUps: []FollowUp{}},
		{Kind: SecDecisions, Heading: DefaultHeadings[SecDecisions], Decisions: []Decision{}},
		{Kind: SecSTAR, Heading: DefaultHeadings[SecSTAR], STAR: &STAR{}},
		{Kind: SecLessons, Heading: DefaultHeadings[SecLessons]},
	}
	title := d.Title
	if r := []rune(title); len(r) > maxTitle {
		title = string(r[:maxTitle])
	}
	return Record{Kind: KindIncident, Title: title, Date: day, Cluster: d.ClusterName, Severity: severityOf(d.Severity), Sections: sections}
}

// draftFindings is the evidence the write-up keeps: every finding snapshot
// of the draft, newest last, within the evidence size limit.
func draftFindings(d incidents.Draft) (summary, link string, data []byte) {
	type snap struct {
		Time time.Time `json:"time"`
		findingView
	}
	evs := append([]incidents.Evidence(nil), d.Evidence...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
	fs := make([]snap, 0, len(evs))
	for _, e := range evs {
		f := e.Finding
		fs = append(fs, snap{Time: e.Time, findingView: findingView{Rule: f.Rule, Severity: f.Severity, Object: f.Object, Title: f.Title, Evidence: f.Evidence, Fix: f.Fix}})
		if link == "" {
			link = e.Link
		}
	}
	for {
		data, _ = json.Marshal(map[string]any{"cluster": d.ClusterName, "draft": d.ID, "findings": fs})
		if len(data) <= maxEvidenceBytes || len(fs) == 0 {
			break
		}
		fs = fs[len(fs)/4+1:] // keep the newest
	}
	return fmt.Sprintf("%d finding snapshot(s) from Kubemap's draft %s", len(fs), d.ID), link, data
}

type fromDraftRequest struct {
	// TimeZone is the browser's IANA zone ("Asia/Taipei") for the
	// timeline; empty means UTC.
	TimeZone string `json:"timeZone"`
}

// fromDraft writes up a draft as a new incident with its findings as
// evidence. A second write-up of the same draft gets 409 with the entry.
func (a *API) fromDraft(w http.ResponseWriter, r *http.Request) {
	var req fromDraftRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	loc := time.UTC
	if req.TimeZone != "" {
		l, err := time.LoadLocation(req.TimeZone)
		if err != nil || len(req.TimeZone) > 64 {
			httpjson.Error(w, http.StatusBadRequest, "bad_request", "unknown time zone")
			return
		}
		loc = l
	}
	sd, err := a.store.GetJournalDraft(r.Context(), r.PathValue("draft"))
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such draft")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	d, err := decodeDraft(sd)
	if err != nil {
		internal(w)
		return
	}
	if old, err := a.store.FindJournalEntryBySource(r.Context(), DraftSource(d.ID)); err == nil {
		httpjson.Write(w, http.StatusConflict, map[string]string{"error": "exists", "message": "this draft was written up already", "id": old.ID})
		return
	}
	rec := FromDraft(*d, loc)
	if err := rec.Validate(); err != nil {
		httpjson.Error(w, http.StatusUnprocessableEntity, "bad_draft", "the draft cannot become an incident: "+err.Error())
		return
	}
	se, err := fromRecord(rec)
	if err != nil {
		internal(w)
		return
	}
	se.Source, se.CreatedBy = DraftSource(d.ID), user(r)
	saved, err := a.store.CreateJournalEntry(r.Context(), se)
	if errors.Is(err, store.ErrExists) {
		httpjson.Error(w, http.StatusConflict, "exists", "this draft was written up already")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if len(d.Evidence) > 0 {
		summary, link, data := draftFindings(*d)
		if _, err := a.store.AddJournalEvidence(r.Context(), store.JournalEvidence{EntryID: saved.ID, Kind: EvFindings, ClusterID: d.ClusterID,
			Summary: summary, Link: link, Data: data, CapturedAt: d.Updated, CapturedBy: "kubemap"}); err != nil {
			internal(w)
			return
		}
	}
	if err := a.writeAudit(r, d.ClusterID, "journal.from-draft", target(saved.ID, d.ID)); err != nil {
		internal(w)
		return
	}
	entry, err := toEntry(saved)
	if err != nil {
		internal(w)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"entry": entry})
}
