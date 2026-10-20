package observe

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) (*API, *[]string) {
	var queries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		if strings.Contains(q, "spanmetrics") {
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			return
		}
		start := r.URL.Query().Get("start")
		w.Write([]byte(`{"status":"success","data":{"result":[{"values":[[` + start + `,"1.5"]]}]}}`))
	}))
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "ok1"
		if strings.Contains(r.URL.Query().Get("q"), "status=error") {
			id = "bad1"
		}
		w.Write([]byte(`{"traces":[{"traceID":"` + id + `","rootTraceName":"/x","rootServiceName":"s","startTimeUnixNano":"1000000000","durationMs":5}]}`))
	}))
	t.Cleanup(prom.Close)
	t.Cleanup(tempo.Close)
	a := NewAPI(Config{PromURL: prom.URL, TempoURL: tempo.URL})
	a.Now = func() time.Time { return time.Unix(7200, 0) }
	return a, &queries
}

func get(a *API, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.Routes(func(h http.Handler) http.Handler { return h })(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestWorkload(t *testing.T) {
	a, _ := newTest(t)
	w := get(a, "/api/observe/workload?namespace=otel-demo&name=demo-backend")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var m WorkloadMetrics
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m.Times) != 61 || len(m.CPU) != 1 || *m.CPU[0].Values[0] != 1.5 {
		t.Fatalf("bad metrics: %+v", m)
	}
	if len(m.Requests) != 0 || len(m.Notes) != 2 {
		t.Fatalf("empty span metrics should be a note: %v", m.Notes)
	}
}

func TestRejectsInjection(t *testing.T) {
	a, queries := newTest(t)
	for _, p := range []string{
		`/api/observe/workload?namespace=x&name=a%22%7D%20or%20vector(1)`,
		`/api/observe/workload?namespace=&name=a`,
		`/api/observe/traces?service=a%22%20%7D`,
	} {
		if w := get(a, p); w.Code != 400 {
			t.Errorf("%s: want 400, got %d", p, w.Code)
		}
	}
	if len(*queries) != 0 {
		t.Errorf("a rejected request reached Prometheus: %v", *queries)
	}
}

func TestTracesFailedFirst(t *testing.T) {
	a, _ := newTest(t)
	w := get(a, "/api/observe/traces?service=demo-backend")
	var r struct{ Traces []Trace }
	json.Unmarshal(w.Body.Bytes(), &r)
	if len(r.Traces) != 2 || r.Traces[0].ID != "bad1" || !r.Traces[0].Error || r.Traces[1].StartMs != 1000 {
		t.Fatalf("got %+v", r.Traces)
	}
}

func TestOff(t *testing.T) {
	a := NewAPI(Config{})
	if w := get(a, "/api/observe/workload?namespace=a&name=b"); w.Code != 501 {
		t.Fatal(w.Code)
	}
}

func TestPodPattern(t *testing.T) {
	re := regexp.MustCompile(`^(?:` + strings.ReplaceAll(podPattern("my.app"), `\\`, `\`) + `)$`)
	for _, ok := range []string{"my.app-6845869d98-42cx8", "my.app-0", "my.app-x7k2p"} {
		if !re.MatchString(ok) {
			t.Errorf("%s should match", ok)
		}
	}
	for _, bad := range []string{"my.app-api-6845869d98-42cx8", "myxapp-0", "my.app-api-x7k2p"} {
		if re.MatchString(bad) {
			t.Errorf("%s should not match", bad)
		}
	}
	if strings.Contains(podPattern("a.b"), `\\.`) == false {
		t.Error("backslash must be doubled for PromQL")
	}
}

func TestServiceResolvedFromWorkloadAttributes(t *testing.T) {
	var traceQ, promQ string
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "k8s.deployment.name") {
			if !strings.Contains(q, `k8s.namespace.name="tcping"`) || !strings.Contains(q, `k8s.deployment.name="backend"`) {
				t.Errorf("lookup must use namespace and deployment: %s", q)
			}
			w.Write([]byte(`{"traces":[{"spanSet":{"spans":[{"attributes":[{"key":"service.name","value":{"stringValue":"tcping-backend"}}]}]}}]}`))
			return
		}
		traceQ = q
		w.Write([]byte(`{"traces":[]}`))
	}))
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); strings.Contains(q, "spanmetrics") {
			promQ = q
		}
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(tempo.Close)
	t.Cleanup(prom.Close)
	a := NewAPI(Config{PromURL: prom.URL, TempoURL: tempo.URL})
	a.Now = func() time.Time { return time.Unix(7200, 0) }

	var m WorkloadMetrics
	json.Unmarshal(get(a, "/api/observe/workload?namespace=tcping&name=backend").Body.Bytes(), &m)
	if m.Service != "tcping-backend" || !strings.Contains(promQ, `service="tcping-backend"`) {
		t.Fatalf("service %q, promql %q", m.Service, promQ)
	}
	get(a, "/api/observe/traces?namespace=tcping&name=backend")
	if !strings.Contains(traceQ, `"tcping-backend"`) {
		t.Fatalf("traces query %q", traceQ)
	}
}

func TestTraceSpans(t *testing.T) {
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traces/11e59a9018b03b14fb5ae470f1c2471c" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"tcping-backend"}}]},"scopeSpans":[
		 {"scope":{"name":"opentelemetry.instrumentation.flask"},"spans":[{"spanId":"a","name":"GET /api/whitelist","startTimeUnixNano":"2000000000","endTimeUnixNano":"2015000000","attributes":[{"key":"http.target","value":{"stringValue":"/api/whitelist"}}],"status":{}}]},
		 {"scope":{"name":"opentelemetry.instrumentation.pymysql"},"spans":[{"spanId":"b","parentSpanId":"a","name":"SELECT","startTimeUnixNano":"2002000000","endTimeUnixNano":"2012000000","attributes":[{"key":"db.statement","value":{"stringValue":"SELECT 1"}}],"status":{"code":"STATUS_CODE_ERROR"}}]}]}]}`))
	}))
	t.Cleanup(tempo.Close)
	a := NewAPI(Config{TempoURL: tempo.URL})
	if w := get(a, "/api/observe/trace?id=..%2Fx"); w.Code != 400 {
		t.Fatalf("bad id: %d", w.Code)
	}
	w := get(a, "/api/observe/trace?id=11e59a9018b03b14fb5ae470f1c2471c")
	var r struct{ Spans []Span }
	json.Unmarshal(w.Body.Bytes(), &r)
	if len(r.Spans) != 2 {
		t.Fatalf("got %s", w.Body.String())
	}
	f, d := r.Spans[0], r.Spans[1]
	if f.Service != "tcping-backend" || f.Scope != "flask" || f.StartUs != 0 || f.DurationUs != 15000 || f.Detail != "/api/whitelist" {
		t.Errorf("flask span %+v", f)
	}
	if d.ParentID != "a" || d.StartUs != 2000 || !d.Error || d.Detail != "SELECT 1" {
		t.Errorf("db span %+v", d)
	}
}

func TestTimeRange(t *testing.T) {
	a, _ := newTest(t)
	// 5 minutes: one-minute steps, 6 points.
	w := get(a, "/api/observe/workload?namespace=otel-demo&name=demo-backend&from=3600000&to=3900000")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var m WorkloadMetrics
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m.Times) != 6 || m.From != 3600000 || m.To != 3900000 {
		t.Fatalf("bad window: %d points from %d to %d", len(m.Times), m.From, m.To)
	}
	// 2 days: step grows so there are at most about 120 points.
	w = get(a, "/api/observe/workload?namespace=otel-demo&name=demo-backend&from=1000&to="+strconv.Itoa(1000+2*24*3600*1000))
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m.Times) > 130 || m.StepMs < 120000 {
		t.Fatalf("2 days should use a coarse step: %d points, step %d ms", len(m.Times), m.StepMs)
	}
	for _, bad := range []string{"from=5&to=5", "from=abc&to=9", "from=1000&to=" + strconv.Itoa(1000+8*24*3600*1000), "from=1000"} {
		if w := get(a, "/api/observe/traces?namespace=otel-demo&name=demo-backend&"+bad); w.Code != 400 {
			t.Fatalf("%s should be rejected, got %d", bad, w.Code)
		}
	}
	if w := get(a, "/api/observe/traces?namespace=otel-demo&name=demo-backend&from=3600000&to=3900000"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSLOAndDatabaseLayer(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		inst := strings.HasSuffix(r.URL.Path, "/query")
		val := func(v string) {
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[1,"` + v + `"]}]}}`))
		}
		switch {
		case inst && strings.Contains(q, "by (server)"):
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"server":"tcping"},"value":[1,"1"]}]}}`))
		case inst && strings.Contains(q, "increase") && strings.Contains(q, "/ sum"):
			switch {
			case strings.Contains(q, "[1h]"):
				val("0.01")
			case strings.Contains(q, "[6h]"):
				val("0.002")
			default:
				val("0.0005")
			}
		case inst && strings.Contains(q, "STATUS_CODE_ERROR"):
			val("5")
		case inst:
			val("1000")
		case strings.Contains(q, "traces_service_graph"):
			w.Write([]byte(`{"status":"success","data":{"result":[{"values":[[` + r.URL.Query().Get("start") + `,"2"]]}]}}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)
	a := NewAPI(Config{PromURL: prom.URL})
	a.Now = func() time.Time { return time.Unix(7200, 0) }
	w := get(a, "/api/observe/workload?namespace=tcping&name=backend&slo=99.9")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var m WorkloadMetrics
	json.Unmarshal(w.Body.Bytes(), &m)
	s := m.SLO
	if s == nil || s.Burn1h == nil || s.Burn6h == nil || s.Remaining == nil || s.ExhaustHrs == nil {
		t.Fatalf("missing slo: %+v", s)
	}
	near := func(got *float64, want float64) bool { return math.Abs(*got-want) < 1e-6 }
	if !near(s.Burn1h, 10) || !near(s.Burn6h, 2) || !near(s.Remaining, 0.5) || !near(s.ExhaustHrs, 8.4) {
		t.Fatalf("burn %v %v remaining %v exhaust %v", *s.Burn1h, *s.Burn6h, *s.Remaining, *s.ExhaustHrs)
	}
	if len(m.DBRequests) != 1 || len(m.DBNames) != 1 || m.DBNames[0] != "tcping" {
		t.Fatalf("database layer missing: %+v %v", m.DBRequests, m.DBNames)
	}
	for _, bad := range []string{"50", "abc", "100"} {
		if w := get(a, "/api/observe/workload?namespace=tcping&name=backend&slo="+bad); w.Code != 400 {
			t.Fatalf("slo=%s should be rejected, got %d", bad, w.Code)
		}
	}
}

func TestWorkloadDropsNaNValues(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			w.Write([]byte(`{"status":"success","data":{"result":[{"values":[[` + r.URL.Query().Get("start") + `,"NaN"]]}]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer prom.Close()
	a := NewAPI(Config{PromURL: prom.URL})
	a.Now = func() time.Time { return time.Unix(7200, 0) }
	rec := get(a, "/api/observe/workload?namespace=tcping&name=backend")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"times"`) {
		t.Fatalf("want a JSON body, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestWorkloadRejectsNaNSlo(t *testing.T) {
	a, _ := newTest(t)
	if rec := get(a, "/api/observe/workload?namespace=tcping&name=backend&slo=NaN"); rec.Code != 400 {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
