// Package observe reads metrics from Prometheus and traces from Tempo, both
// read-only, so the app page can show one workload's CPU, memory, restarts,
// request and error rate, and its recent traces next to its pods and logs.
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kubemap/portal/httpjson"
)

// Config holds the base URLs. An empty PromURL or TempoURL turns that source off.
type Config struct {
	PromURL    string
	TempoURL   string
	GrafanaURL string // only for the "Open in Grafana" links
}

// API serves (behind the login):
//
//	GET /api/observe/config                         which sources are on
//	GET /api/observe/workload?namespace=&name=      metrics, last hour
//	GET /api/observe/traces?service= | namespace=&name=   recent traces from Tempo
//	GET /api/observe/trace?id=                      the spans of one trace (timeline)
type API struct {
	Config Config
	Client *http.Client
	Now    func() time.Time
}

func NewAPI(c Config) *API {
	return &API{Config: c, Client: &http.Client{Timeout: 8 * time.Second}, Now: time.Now}
}

func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/observe/config", require(http.HandlerFunc(a.config)))
		mux.Handle("GET /api/observe/workload", require(http.HandlerFunc(a.workload)))
		mux.Handle("GET /api/observe/traces", require(http.HandlerFunc(a.traces)))
		mux.Handle("GET /api/observe/trace", require(http.HandlerFunc(a.trace)))
	}
}

// Names go into PromQL and TraceQL strings, so only Kubernetes name
// characters are accepted.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// podPattern matches the pod names of one Deployment, StatefulSet or DaemonSet
// ("web-<hash>-<id>", "web-<n>", "web-<id>") but not those of "web-api".
// Backslashes are doubled because the pattern sits inside a PromQL string.
func podPattern(name string) string {
	n := strings.ReplaceAll(regexp.QuoteMeta(name), `\`, `\\`)
	return n + `-[a-z0-9]{6,10}-[a-z0-9]{5}|` + n + `-[0-9]+|` + n + `-[a-z0-9]{5}`
}

func (a *API) config(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{
		"metrics":    a.Config.PromURL != "",
		"traces":     a.Config.TempoURL != "",
		"grafanaUrl": a.Config.GrafanaURL,
	})
}

// Series is one chart line: Values[i] belongs to Times[i] (ms).
type Series struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
}

type WorkloadMetrics struct {
	From     int64    `json:"from"`
	To       int64    `json:"to"`
	StepMs   int64    `json:"stepMs"`
	Times    []int64  `json:"times"`
	CPU      []Series `json:"cpu"`      // cores
	Memory   []Series `json:"memory"`   // bytes
	Restarts []Series `json:"restarts"` // total
	Requests []Series `json:"requests"` // per second, from Tempo span metrics
	Errors   []Series `json:"errors"`   // per second
	// Latency is the p50, p95 and p99 of the server spans, in seconds.
	Latency []Series `json:"latency"`
	// The database layer, from the service graph: calls this service makes to
	// a database (connection_type "database").
	DBRequests []Series `json:"dbRequests"` // per second
	DBErrors   []Series `json:"dbErrors"`   // per second
	DBLatency  []Series `json:"dbLatency"`  // p95, seconds
	DBNames    []string `json:"dbNames"`
	SLO        *SLO     `json:"slo"`
	// Service is the OpenTelemetry service.name found for this workload
	// ("" when no spans carry its namespace and deployment name).
	Service string `json:"service"`
	// Notes lists queries that failed or returned nothing, in plain words.
	Notes []string `json:"notes"`
}

// SLO is the error budget picture at the end of the chosen range. Ratios and
// counts are nil when there was no traffic in the window.
type SLO struct {
	Target     float64  `json:"target"` // percent, for example 99.9
	WindowDays int      `json:"windowDays"`
	Requests   *float64 `json:"requests"`     // server requests in the window
	Errors     *float64 `json:"errors"`       // failed ones
	Burn1h     *float64 `json:"burn1h"`       // error ratio over 1 h / allowed ratio
	Burn6h     *float64 `json:"burn6h"`       // the same over 6 h
	Remaining  *float64 `json:"remaining"`    // share of the budget left, 1 = all, below 0 = overspent
	ExhaustHrs *float64 `json:"exhaustHours"` // at the 1 h burn rate; nil when not burning
}

const sloWindow = 7 * 24 * time.Hour // Prometheus keeps 7 days, so no longer window is possible

const maxRange = 7 * 24 * time.Hour // Prometheus keeps 7 days; Tempo keeps 24 hours

// timeWindow reads the optional from and to query values (Unix milliseconds).
// Without them the window is the last hour ending now.
func (a *API) timeWindow(r *http.Request) (start, end time.Time, err error) {
	q := r.URL.Query()
	if q.Get("from") == "" && q.Get("to") == "" {
		end = a.Now()
		return end.Add(-time.Hour), end, nil
	}
	f, e1 := strconv.ParseInt(q.Get("from"), 10, 64)
	t, e2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if e1 != nil || e2 != nil || f <= 0 || t <= f {
		return start, end, fmt.Errorf("from and to must be Unix milliseconds, and to must be after from")
	}
	start, end = time.UnixMilli(f), time.UnixMilli(t)
	if end.Sub(start) > maxRange {
		return start, end, fmt.Errorf("the time range can be at most 7 days")
	}
	return start, end, nil
}

func (a *API) workload(w http.ResponseWriter, r *http.Request) {
	if a.Config.PromURL == "" {
		httpjson.Error(w, http.StatusNotImplemented, "not_configured", "Prometheus is not configured")
		return
	}
	ns, name := r.URL.Query().Get("namespace"), r.URL.Query().Get("name")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "namespace and name are required")
		return
	}
	from, to, err := a.timeWindow(r)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// About 120 points at most; never finer than one minute.
	step := int(math.Ceil(to.Sub(from).Seconds()/120/60)) * 60
	if step < 60 {
		step = 60
	}
	stepMs := int64(step) * 1000
	startMs := from.UnixMilli() - from.UnixMilli()%stepMs // epoch-aligned grid
	start, end := time.UnixMilli(startMs), to
	rateWin := "5m"
	if step > 300 {
		rateWin = strconv.Itoa(step) + "s"
	}
	out := WorkloadMetrics{From: startMs, To: to.UnixMilli(), StepMs: stepMs, Notes: []string{}}
	for ms := startMs; ms <= to.UnixMilli(); ms += stepMs {
		out.Times = append(out.Times, ms)
	}
	pod := fmt.Sprintf(`namespace="%s",pod=~"%s"`, ns, podPattern(name))
	service := a.resolveService(r.Context(), ns, name)
	out.Service = service
	svc := fmt.Sprintf(`service="%s"`, service)
	srv := svc + `,span_kind="SPAN_KIND_SERVER"` // the requests this service answers, not its own DB or HTTP calls
	target := 99.9
	if v := r.URL.Query().Get("slo"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 90 && f <= 99.999) {
			httpjson.Error(w, http.StatusBadRequest, "bad_request", "slo must be a percentage from 90 to 99.999")
			return
		}
		target = f
	}
	db := fmt.Sprintf(`client="%s",connection_type="database"`, service)
	queries := []struct {
		dst   *[]Series
		label string
		q     string
	}{
		{&out.CPU, "CPU", fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{%s,container!=""}[%s]))`, pod, rateWin)},
		{&out.Memory, "Memory", fmt.Sprintf(`sum(container_memory_working_set_bytes{%s,container!=""})`, pod)},
		{&out.Restarts, "Restarts", fmt.Sprintf(`sum(kube_pod_container_status_restarts_total{%s})`, pod)},
		{&out.Requests, "Requests", fmt.Sprintf(`sum(rate(traces_spanmetrics_calls_total{%s}[%s]))`, srv, rateWin)},
		{&out.Errors, "Errors", fmt.Sprintf(`(sum(rate(traces_spanmetrics_calls_total{%s,status_code="STATUS_CODE_ERROR"}[%s])) or vector(0))`, srv, rateWin)},
	}
	quiet := []struct { // an empty answer here means "nothing to show", not a problem
		dst *[]Series
		q   string
	}{
		{&out.DBRequests, fmt.Sprintf(`sum(rate(traces_service_graph_request_total{%s}[%s]))`, db, rateWin)},
		{&out.DBErrors, fmt.Sprintf(`sum(rate(traces_service_graph_request_failed_total{%s}[%s]))`, db, rateWin)},
		{&out.DBLatency, fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(traces_service_graph_request_client_seconds_bucket{%s}[%s])))`, db, rateWin)},
	}
	for _, q := range quiet {
		if vals, err := a.promRange(r.Context(), q.q, start, end, step, len(out.Times), start.Unix()); err == nil && vals != nil {
			*q.dst = []Series{{Name: "", Values: vals}}
		}
	}
	for _, p := range []struct {
		name string
		q    string
	}{{"p50", "0.5"}, {"p95", "0.95"}, {"p99", "0.99"}} {
		q := fmt.Sprintf(`histogram_quantile(%s, sum by (le) (rate(traces_spanmetrics_latency_bucket{%s}[%s])))`, p.q, srv, rateWin)
		if vals, err := a.promRange(r.Context(), q, start, end, step, len(out.Times), start.Unix()); err == nil && vals != nil {
			out.Latency = append(out.Latency, Series{Name: p.name, Values: vals})
		}
	}
	out.DBNames = a.promLabel(r.Context(), fmt.Sprintf(`count by (server) (traces_service_graph_request_total{%s})`, db), "server", to)
	out.SLO = a.slo(r.Context(), srv, target, to)
	for _, q := range queries {
		vals, err := a.promRange(r.Context(), q.q, start, end, step, len(out.Times), start.Unix())
		switch {
		case err != nil:
			out.Notes = append(out.Notes, q.label+": "+err.Error())
		case vals == nil:
			out.Notes = append(out.Notes, q.label+": no data")
		default:
			*q.dst = []Series{{Name: q.label, Values: vals}}
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

// promValue runs one instant query at time at and returns the first value,
// or nil when there is none.
func (a *API) promValue(ctx context.Context, q string, at time.Time) *float64 {
	r := a.promInstant(ctx, q, at)
	if len(r) == 0 {
		return nil
	}
	s, _ := r[0].Value[1].(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

type instantRow struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func (a *API) promInstant(ctx context.Context, q string, at time.Time) []instantRow {
	v := url.Values{"query": {q}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []instantRow `json:"result"`
		} `json:"data"`
	}
	if err := a.get(ctx, strings.TrimRight(a.Config.PromURL, "/")+"/api/v1/query?"+v.Encode(), &resp); err != nil || resp.Status != "success" {
		return nil
	}
	return resp.Data.Result
}

// promLabel returns the values of one label across the rows of an instant query.
func (a *API) promLabel(ctx context.Context, q, label string, at time.Time) []string {
	out := []string{}
	for _, r := range a.promInstant(ctx, q, at) {
		if v := r.Metric[label]; v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// slo reads the burn rates and the error budget that is left, counted back
// from at. srv is the PromQL selector of the service's server spans.
func (a *API) slo(ctx context.Context, srv string, target float64, at time.Time) *SLO {
	allowed := 1 - target/100
	ratio := func(win string) *float64 {
		q := fmt.Sprintf(`(sum(increase(traces_spanmetrics_calls_total{%[1]s,status_code="STATUS_CODE_ERROR"}[%[2]s])) or vector(0)) / sum(increase(traces_spanmetrics_calls_total{%[1]s}[%[2]s]))`, srv, win)
		return a.promValue(ctx, q, at)
	}
	days := int(sloWindow / (24 * time.Hour))
	out := &SLO{Target: target, WindowDays: days}
	out.Requests = a.promValue(ctx, fmt.Sprintf(`sum(increase(traces_spanmetrics_calls_total{%s}[7d]))`, srv), at)
	out.Errors = a.promValue(ctx, fmt.Sprintf(`sum(increase(traces_spanmetrics_calls_total{%s,status_code="STATUS_CODE_ERROR"}[7d])) or vector(0)`, srv), at)
	burn := func(r *float64) *float64 {
		if r == nil {
			return nil
		}
		b := *r / allowed
		return &b
	}
	out.Burn1h, out.Burn6h = burn(ratio("1h")), burn(ratio("6h"))
	if w := ratio("7d"); w != nil {
		rem := 1 - *w/allowed
		out.Remaining = &rem
		if out.Burn1h != nil && *out.Burn1h > 0 && rem > 0 {
			h := rem * sloWindow.Hours() / *out.Burn1h
			out.ExhaustHrs = &h
		}
	}
	return out
}

// promRange runs one range query and lines the single result up with the
// shared time axis. It returns nil when Prometheus has no series.
func (a *API) promRange(ctx context.Context, q string, start, end time.Time, step, n int, startUnix int64) ([]*float64, error) {
	v := url.Values{"query": {q}, "start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}, "step": {strconv.Itoa(step)}}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := a.get(ctx, strings.TrimRight(a.Config.PromURL, "/")+"/api/v1/query_range?"+v.Encode(), &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" || len(resp.Data.Result) == 0 {
		return nil, nil
	}
	vals := make([]*float64, n)
	for _, p := range resp.Data.Result[0].Values {
		ts, _ := p[0].(float64)
		s, _ := p[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		i := int((int64(ts) - startUnix) / int64(step))
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || i < 0 || i >= n {
			continue
		}
		vals[i] = &f
	}
	return vals, nil
}

type Trace struct {
	ID         string `json:"id"`
	RootName   string `json:"rootName"`
	RootSvc    string `json:"rootService"`
	StartMs    int64  `json:"startMs"`
	DurationMs int64  `json:"durationMs"`
	Error      bool   `json:"error"`
}

func (a *API) traces(w http.ResponseWriter, r *http.Request) {
	if a.Config.TempoURL == "" {
		httpjson.Error(w, http.StatusNotImplemented, "not_configured", "Tempo is not configured")
		return
	}
	svc := r.URL.Query().Get("service")
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		name := r.URL.Query().Get("name")
		if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
			httpjson.Error(w, http.StatusBadRequest, "bad_request", "namespace and name are required")
			return
		}
		svc = a.resolveService(r.Context(), ns, name)
	}
	if !nameRe.MatchString(svc) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "service is required")
		return
	}
	from, to, err := a.timeWindow(r)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	res := map[bool][]Trace{}
	for _, isErr := range []bool{false, true} {
		q := fmt.Sprintf(`{resource.service.name="%s"`, svc)
		if isErr {
			q += ` && status=error`
		}
		q += `}`
		v := url.Values{"q": {q}, "limit": {"50"}, "start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix()+1, 10)}}
		var resp struct {
			Traces []struct {
				TraceID           string `json:"traceID"`
				RootServiceName   string `json:"rootServiceName"`
				RootTraceName     string `json:"rootTraceName"`
				StartTimeUnixNano string `json:"startTimeUnixNano"`
				DurationMs        int64  `json:"durationMs"`
			} `json:"traces"`
		}
		if err := a.get(r.Context(), strings.TrimRight(a.Config.TempoURL, "/")+"/api/search?"+v.Encode(), &resp); err != nil {
			httpjson.Error(w, http.StatusBadGateway, "tempo", "Tempo: "+err.Error())
			return
		}
		for _, t := range resp.Traces {
			ns, _ := strconv.ParseInt(t.StartTimeUnixNano, 10, 64)
			res[isErr] = append(res[isErr], Trace{ID: t.TraceID, RootName: t.RootTraceName, RootSvc: t.RootServiceName, StartMs: ns / 1e6, DurationMs: t.DurationMs, Error: isErr})
		}
	}
	// Failed traces first, then the rest without the failed ones again.
	failed := map[string]bool{}
	out := []Trace{}
	for _, t := range res[true] {
		failed[t.ID] = true
		out = append(out, t)
	}
	for _, t := range res[false] {
		if !failed[t.ID] {
			out = append(out, t)
		}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"traces": out, "service": svc})
}

var traceIDRe = regexp.MustCompile(`^[0-9a-f]{16,32}$`)

// Span is one bar of the timeline. StartUs is the offset from the trace start.
type Span struct {
	ID         string `json:"id"`
	ParentID   string `json:"parentId"`
	Service    string `json:"service"`
	Scope      string `json:"scope"`
	Name       string `json:"name"`
	StartUs    int64  `json:"startUs"`
	DurationUs int64  `json:"durationUs"`
	Error      bool   `json:"error"`
	// Detail is the one attribute that explains the span: the SQL statement,
	// the URL or the route.
	Detail string `json:"detail"`
}

const maxSpans = 500

func (a *API) trace(w http.ResponseWriter, r *http.Request) {
	if a.Config.TempoURL == "" {
		httpjson.Error(w, http.StatusNotImplemented, "not_configured", "Tempo is not configured")
		return
	}
	id := r.URL.Query().Get("id")
	if !traceIDRe.MatchString(id) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "id must be a hex trace id")
		return
	}
	var resp struct {
		Batches []struct {
			Resource struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope struct {
					Name string `json:"name"`
				} `json:"scope"`
				Spans []struct {
					SpanID     string     `json:"spanId"`
					ParentID   string     `json:"parentSpanId"`
					Name       string     `json:"name"`
					Start      string     `json:"startTimeUnixNano"`
					End        string     `json:"endTimeUnixNano"`
					Attributes []otlpAttr `json:"attributes"`
					Status     struct {
						Code any `json:"code"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	if err := a.get(r.Context(), strings.TrimRight(a.Config.TempoURL, "/")+"/api/traces/"+id, &resp); err != nil {
		httpjson.Error(w, http.StatusBadGateway, "tempo", "Tempo: "+err.Error())
		return
	}
	spans := []Span{}
	var first int64
	for _, b := range resp.Batches {
		svc := attr(b.Resource.Attributes, "service.name")
		for _, ss := range b.ScopeSpans {
			scope := ss.Scope.Name[strings.LastIndex(ss.Scope.Name, ".")+1:]
			for _, sp := range ss.Spans {
				if len(spans) >= maxSpans {
					break
				}
				st, _ := strconv.ParseInt(sp.Start, 10, 64)
				en, _ := strconv.ParseInt(sp.End, 10, 64)
				if first == 0 || st < first {
					first = st
				}
				spans = append(spans, Span{
					ID: sp.SpanID, ParentID: sp.ParentID, Service: svc, Scope: scope, Name: sp.Name,
					StartUs: st / 1000, DurationUs: (en - st) / 1000,
					Error:  fmt.Sprint(sp.Status.Code) == "STATUS_CODE_ERROR" || fmt.Sprint(sp.Status.Code) == "2",
					Detail: spanDetail(sp.Attributes),
				})
			}
		}
	}
	for i := range spans {
		spans[i].StartUs -= first / 1000
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"spans": spans})
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
		IntValue    string `json:"intValue"`
	} `json:"value"`
}

func attr(as []otlpAttr, key string) string {
	for _, a := range as {
		if a.Key == key {
			if a.Value.StringValue != "" {
				return a.Value.StringValue
			}
			return a.Value.IntValue
		}
	}
	return ""
}

// spanDetail picks the attribute that says what the span did, first match wins.
func spanDetail(as []otlpAttr) string {
	for _, k := range []string{"db.statement", "db.query.text", "http.url", "url.full", "http.target", "url.path", "http.route"} {
		if v := attr(as, k); v != "" {
			if len(v) > 300 {
				v = v[:300] + "…"
			}
			return v
		}
	}
	return ""
}

// resolveService finds the service.name that the OpenTelemetry Operator (or
// any SDK setting k8s.namespace.name and k8s.deployment.name) gave this
// workload's spans, so the app's service name need not equal its Deployment
// name and two namespaces' "backend" are not mixed. It falls back to name.
func (a *API) resolveService(ctx context.Context, ns, name string) string {
	if a.Config.TempoURL == "" {
		return name
	}
	now := a.Now()
	q := fmt.Sprintf(`{resource.k8s.namespace.name="%s" && resource.k8s.deployment.name="%s"} | select(resource.service.name)`, ns, name)
	v := url.Values{"q": {q}, "limit": {"1"}, "start": {strconv.FormatInt(now.Add(-24*time.Hour).Unix(), 10)}, "end": {strconv.FormatInt(now.Unix(), 10)}}
	var resp struct {
		Traces []struct {
			SpanSet struct {
				Spans []struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"spanSet"`
		} `json:"traces"`
	}
	if err := a.get(ctx, strings.TrimRight(a.Config.TempoURL, "/")+"/api/search?"+v.Encode(), &resp); err != nil {
		return name
	}
	for _, t := range resp.Traces {
		for _, sp := range t.SpanSet.Spans {
			for _, at := range sp.Attributes {
				if at.Key == "service.name" && nameRe.MatchString(at.Value.StringValue) {
					return at.Value.StringValue
				}
			}
		}
	}
	return name
}

func (a *API) get(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("not reachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v); err != nil {
		return fmt.Errorf("unexpected answer")
	}
	return nil
}
