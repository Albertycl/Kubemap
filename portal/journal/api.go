package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/auth"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

// MaxBodyBytes bounds a record or an import sent to the API.
const MaxBodyBytes = 512 << 10

// API serves the journal (behind the login). Every change is written to
// the audit log.
//
//	GET    /api/journal                         the list
//	POST   /api/journal                         create (a Record)
//	POST   /api/journal/import                  import Markdown
//	GET    /api/journal/{id}                    a record with its evidence
//	PUT    /api/journal/{id}                    edit (a Record and the version read)
//	DELETE /api/journal/{id}
//	GET    /api/journal/{id}/markdown[?evidence=1]   export
//	POST   /api/journal/{id}/evidence           capture a snapshot
//	DELETE /api/journal/{id}/evidence/{eid}
//	GET    /api/audit[?limit=n]                 the audit log (evidence links to it)
//	POST   /api/journal-drafts/{draft}/write-up turn a KM-90 draft into an incident
type API struct {
	store store.Store
	// Findings runs the health rules on a cluster (health.API.Report).
	// Nil: findings cannot be captured.
	Findings func(ctx context.Context, clusterID, namespace string) (*protocol.HealthReport, error)
	// Reports gives a cluster's latest node agent reports
	// (metrics.Collector.Reports), for the metrics snapshot. Nil: the
	// snapshot has the stored rollups only.
	Reports func(clusterID string) []protocol.Metrics
	// Agent asks cluster agents (tunnel.Hub), for the events snapshot.
	// Nil: events cannot be captured.
	Agent agentcall.Requester
	// Admin is the only user who may delete an entry or read the audit log
	// (review KM-89 N-3); auth.AdminUser.
	Admin string
	now   func() time.Time
}

// NewAPI returns the journal API.
func NewAPI(st store.Store) *API { return &API{store: st, now: time.Now, Admin: auth.AdminUser} }

// adminOnly answers 403 unless the user is the admin.
func (a *API) adminOnly(w http.ResponseWriter, r *http.Request, what string) bool {
	if user(r) != a.Admin {
		httpjson.Error(w, http.StatusForbidden, "admin_only", "only the admin can "+what)
		return false
	}
	return true
}

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		h := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, require(f)) }
		h("GET /api/journal", a.list)
		h("POST /api/journal", a.create)
		h("POST /api/journal/import", a.importMarkdown)
		h("GET /api/journal/{id}", a.get)
		h("PUT /api/journal/{id}", a.update)
		h("DELETE /api/journal/{id}", a.delete)
		h("GET /api/journal/{id}/markdown", a.export)
		h("POST /api/journal/{id}/evidence", a.addEvidence)
		h("DELETE /api/journal/{id}/evidence/{eid}", a.deleteEvidence)
		h("GET /api/audit", a.auditLog)
		h("POST /api/journal-drafts/{draft}/write-up", a.fromDraft)
	}
}

// Entry is a stored record.
type Entry struct {
	ID string `json:"id"`
	Record
	Source    string    `json:"source"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

// Summary is an entry in the list, without its sections.
type Summary struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Date      string    `json:"date"`
	Cluster   string    `json:"cluster"`
	Severity  string    `json:"severity"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

// Evidence is a snapshot attached to an entry.
type Evidence struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	ClusterID  string          `json:"clusterId"`
	Summary    string          `json:"summary"`
	Link       string          `json:"link"`
	Note       string          `json:"note"`
	Data       json.RawMessage `json:"data"`
	CapturedAt time.Time       `json:"capturedAt"`
	CapturedBy string          `json:"capturedBy"`
}

// body is what the store keeps as JSON; the rest of a Record is columns.
type body struct {
	Sections []Section `json:"sections"`
}

func toEntry(e store.JournalEntry) (Entry, error) {
	var b body
	if err := json.Unmarshal(e.Body, &b); err != nil {
		return Entry{}, fmt.Errorf("journal %s: %w", e.ID, err)
	}
	if b.Sections == nil {
		b.Sections = []Section{}
	}
	return Entry{
		ID:     e.ID,
		Record: Record{Kind: e.Kind, Title: e.Title, Date: e.Date, Cluster: e.Cluster, Severity: e.Severity, Sections: b.Sections},
		Source: e.Source, Version: e.Version,
		CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy, UpdatedAt: e.UpdatedAt, UpdatedBy: e.UpdatedBy,
	}, nil
}

func fromRecord(r Record) (store.JournalEntry, error) {
	b, err := json.Marshal(body{Sections: r.Sections})
	if err != nil {
		return store.JournalEntry{}, err
	}
	return store.JournalEntry{Kind: r.Kind, Title: r.Title, Date: r.Date, Cluster: r.Cluster, Severity: r.Severity, Body: b}, nil
}

func toEvidence(ev store.JournalEvidence) Evidence {
	return Evidence{ID: ev.ID, Kind: ev.Kind, ClusterID: ev.ClusterID, Summary: ev.Summary, Link: ev.Link, Note: ev.Note,
		Data: json.RawMessage(ev.Data), CapturedAt: ev.CapturedAt, CapturedBy: ev.CapturedBy}
}

func user(r *http.Request) string {
	u, _ := auth.UserFromContext(r.Context())
	return u
}

func internal(w http.ResponseWriter) {
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}

// auditLog serves the audit log, newest first, for the audit page.
func (a *API) auditLog(w http.ResponseWriter, r *http.Request) {
	if !a.adminOnly(w, r, "read the audit log") {
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			httpjson.Error(w, http.StatusBadRequest, "bad_request", "limit must be 1 to 1000")
			return
		}
		limit = n
	}
	es, err := a.store.ListAudit(r.Context(), limit)
	if err != nil {
		internal(w)
		return
	}
	out := make([]auditView, 0, len(es))
	for _, e := range es {
		out = append(out, toAuditView(e))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"entries": out})
}

// writeAudit records a change. The change is already stored; if the audit
// log cannot be written the caller answers 500, so the user sees it.
func (a *API) writeAudit(r *http.Request, clusterID, action, target string) error {
	return a.store.WriteAudit(r.Context(), store.AuditEntry{User: user(r), ClusterID: clusterID, Action: action, Target: target, Result: "ok"})
}

func target(id, title string) string {
	if r := []rune(title); len(r) > 80 {
		title = string(r[:80]) + "…"
	}
	return id + " " + title
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	es, err := a.store.ListJournalEntries(r.Context())
	if err != nil {
		internal(w)
		return
	}
	out := make([]Summary, 0, len(es))
	for _, e := range es {
		out = append(out, Summary{ID: e.ID, Kind: e.Kind, Title: e.Title, Date: e.Date, Cluster: e.Cluster, Severity: e.Severity,
			Source: e.Source, UpdatedAt: e.UpdatedAt, UpdatedBy: e.UpdatedBy})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"entries": out})
}

// load returns the entry named by {id}, or answers 404.
func (a *API) load(w http.ResponseWriter, r *http.Request) (store.JournalEntry, bool) {
	e, err := a.store.GetJournalEntry(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such journal entry")
		return e, false
	}
	if err != nil {
		internal(w)
		return e, false
	}
	return e, true
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	e, ok := a.load(w, r)
	if !ok {
		return
	}
	entry, err := toEntry(e)
	if err != nil {
		internal(w)
		return
	}
	evs, err := a.store.ListJournalEvidence(r.Context(), e.ID)
	if err != nil {
		internal(w)
		return
	}
	out := make([]Evidence, 0, len(evs))
	for _, ev := range evs {
		out = append(out, toEvidence(ev))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"entry": entry, "evidence": out})
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var rec Record
	if !httpjson.ReadLimit(w, r, &rec, MaxBodyBytes) {
		return
	}
	if err := rec.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.insert(w, r, rec)
}

// insert stores a new record and answers with it.
func (a *API) insert(w http.ResponseWriter, r *http.Request, rec Record) {
	se, err := fromRecord(rec)
	if err != nil {
		internal(w)
		return
	}
	se.CreatedBy = user(r)
	saved, err := a.store.CreateJournalEntry(r.Context(), se)
	if err != nil {
		internal(w)
		return
	}
	a.answer(w, r, saved, http.StatusCreated, "journal.create")
}

// answer audits a change and sends the stored entry.
func (a *API) answer(w http.ResponseWriter, r *http.Request, saved store.JournalEntry, status int, action string) {
	if err := a.writeAudit(r, "", action, target(saved.ID, saved.Title)); err != nil {
		internal(w)
		return
	}
	entry, err := toEntry(saved)
	if err != nil {
		internal(w)
		return
	}
	httpjson.Write(w, status, map[string]any{"entry": entry})
}

type updateRequest struct {
	Record
	Version int `json:"version"`
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !httpjson.ReadLimit(w, r, &req, MaxBodyBytes) {
		return
	}
	if err := req.Record.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.save(w, r, r.PathValue("id"), req.Version, req.Record)
}

// save replaces a stored record if it is still at version.
func (a *API) save(w http.ResponseWriter, r *http.Request, id string, version int, rec Record) {
	se, err := fromRecord(rec)
	if err != nil {
		internal(w)
		return
	}
	se.ID, se.Version, se.UpdatedBy = id, version, user(r)
	saved, err := a.store.UpdateJournalEntry(r.Context(), se)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such journal entry")
		return
	case errors.Is(err, store.ErrConflict):
		httpjson.Error(w, http.StatusConflict, "conflict", "someone changed this entry after you opened it; reload it and edit again")
		return
	case err != nil:
		internal(w)
		return
	}
	a.answer(w, r, saved, http.StatusOK, "journal.update")
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	if !a.adminOnly(w, r, "delete a journal entry") {
		return
	}
	e, ok := a.load(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteJournalEntry(r.Context(), e.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		internal(w)
		return
	}
	if err := a.writeAudit(r, "", "journal.delete", target(e.ID, e.Title)); err != nil {
		internal(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) importMarkdown(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if !httpjson.ReadLimit(w, r, &req, MaxBodyBytes) {
		return
	}
	saved, exact, err := Import(r.Context(), a.store, req, user(r))
	var invalid *InvalidError
	var exists *ExistsError
	switch {
	case errors.As(err, &invalid):
		httpjson.Error(w, http.StatusBadRequest, "bad_request", invalid.Msg)
		return
	case errors.As(err, &exists):
		httpjson.Write(w, http.StatusConflict, map[string]string{"error": "exists", "message": exists.Error(), "id": exists.ID})
		return
	case errors.Is(err, store.ErrConflict):
		httpjson.Error(w, http.StatusConflict, "conflict", "the entry changed during the import; try again")
		return
	case err != nil:
		internal(w)
		return
	}
	entry, err := toEntry(saved)
	if err != nil {
		internal(w)
		return
	}
	status := http.StatusCreated
	if saved.Version > 1 {
		status = http.StatusOK
	}
	httpjson.Write(w, status, map[string]any{"entry": entry, "exact": exact})
}

func (a *API) export(w http.ResponseWriter, r *http.Request) {
	e, ok := a.load(w, r)
	if !ok {
		return
	}
	entry, err := toEntry(e)
	if err != nil {
		internal(w)
		return
	}
	md := Render(entry.Record)
	if r.URL.Query().Get("evidence") == "1" {
		evs, err := a.store.ListJournalEvidence(r.Context(), e.ID)
		if err != nil {
			internal(w)
			return
		}
		md += evidenceMarkdown(evs)
	}
	name := e.Source
	if !strings.HasSuffix(name, ".md") { // not imported: made here or from a draft
		name = FileName(entry.Record)
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(md))
}

// FileName is the export's file name: the date and the title as a slug.
func FileName(r Record) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(r.Title), "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	if slug == "" {
		slug = r.Kind
	}
	if r.Date != "" {
		return r.Date + "-" + slug + ".md"
	}
	return slug + ".md"
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// evidenceMarkdown lists the evidence after the record. The list is not
// read back by an import (it would become a text section).
func evidenceMarkdown(evs []store.JournalEvidence) string {
	if len(evs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Evidence (from Kubemap)\n\n")
	for _, ev := range evs {
		fmt.Fprintf(&b, "- %s %s: %s", ev.CapturedAt.UTC().Format("2006-01-02 15:04 UTC"), ev.Kind, ev.Summary)
		if ev.Link != "" {
			fmt.Fprintf(&b, " (Kubemap page `%s`)", ev.Link)
		}
		if ev.Note != "" {
			b.WriteString(". " + strings.ReplaceAll(ev.Note, "\n", " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}
