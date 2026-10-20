package resources

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
)

// searchTimeout bounds a log search: the agent reads every container of
// the app within its own 15 s request limit.
const searchTimeout = 20 * time.Second

// logSearch searches the logs of every pod of an app (logs.search, KM-57):
//
//	GET /api/clusters/{id}/log-search?app=ns/name[&component=Kind/ns/name]
//	    [&q=text][&levels=error,warn][&since=1h|RFC 3339][&until=RFC 3339]
//	    [&previous=1][&max=500]
//
// since is a duration back from until (or now), or a time.
func (a *API) logSearch(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	q, msg := searchQuery(r, time.Now())
	if msg == "" {
		req := &protocol.Request{Op: protocol.OpLogSearch, LogSearch: q}
		if err := req.Validate(); err != nil {
			msg = protocol.AsError(err).Message
		}
	}
	if msg != "" {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, &protocol.Request{Op: protocol.OpLogSearch, LogSearch: q})
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "search one component or a shorter window")
		return
	}
	var res protocol.LogSearchResult
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &res) != nil {
		httpjson.Error(w, http.StatusBadGateway, "agent_error", "the cluster agent sent an unreadable search result")
		return
	}
	if res.Entries == nil {
		res.Entries = []protocol.LogEntry{}
	}
	if res.Sources == nil {
		res.Sources = []protocol.LogSource{}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"search": res, "at": time.Now().UTC()})
}

// searchQuery reads the query string; msg says what is wrong with it.
func searchQuery(r *http.Request, now time.Time) (q *protocol.LogSearchRequest, msg string) {
	v := r.URL.Query()
	q = &protocol.LogSearchRequest{App: v.Get("app"), Component: v.Get("component"), Text: v.Get("q"),
		Previous: v.Get("previous") == "1" || v.Get("previous") == "true"}
	if q.App == "" {
		return nil, "app is required"
	}
	for l := range strings.SplitSeq(v.Get("levels"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			q.Levels = append(q.Levels, l)
		}
	}
	if s := v.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, "until must be an RFC 3339 time"
		}
		q.Until = t
	}
	if s := v.Get("since"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			if d <= 0 {
				return nil, "since must be a positive duration"
			}
			end := q.Until
			if end.IsZero() {
				end = now
			}
			q.Since = end.Add(-d)
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			q.Since = t
		} else {
			return nil, "since must be a duration (1h) or an RFC 3339 time"
		}
	}
	if s := v.Get("max"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, "max must be a number"
		}
		q.MaxLines = n
	}
	return q, ""
}
