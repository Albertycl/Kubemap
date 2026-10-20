package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

// fakeLogs serves logs by "ns/pod/container[/previous]" and records the
// options each read asked for.
type fakeLogs struct {
	mu    sync.Mutex
	logs  map[string]string
	errs  map[string]error
	calls []*corev1.PodLogOptions
}

func (f *fakeLogs) open(_ context.Context, ns, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, o)
	key := ns + "/" + pod + "/" + o.Container
	if o.Previous {
		key += "/previous"
	}
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	// Like the kubelet: tailLines picks the newest lines, then limitBytes
	// cuts from the start of what is left.
	text := f.logs[key]
	if o.TailLines != nil {
		ls := strings.SplitAfter(text, "\n")
		if ls[len(ls)-1] == "" {
			ls = ls[:len(ls)-1]
		}
		if n := int(*o.TailLines); len(ls) > n {
			ls = ls[len(ls)-n:]
		}
		text = strings.Join(ls, "")
	}
	if o.LimitBytes != nil && int64(len(text)) > *o.LimitBytes {
		text = text[:*o.LimitBytes]
	}
	return io.NopCloser(strings.NewReader(text)), nil
}

// lines stamps each line at now-ago.
func lines(now time.Time, ls ...string) string {
	var b strings.Builder
	for _, l := range ls {
		ago, text, _ := strings.Cut(l, "|")
		d, _ := time.ParseDuration(ago)
		b.WriteString(now.Add(-d).UTC().Format(time.RFC3339Nano) + " " + text + "\n")
	}
	return b.String()
}

// searchApps is twoApps with containers, a restarted container and a
// second url-shortener pod.
func searchApps() map[schema.GroupVersionResource][]runtime.Object {
	src := twoApps()
	for _, o := range src[gPods] {
		p := o.(*corev1.Pod)
		p.Spec.Containers = []corev1.Container{{Name: "app"}}
		if p.Name == "us-1" {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "proxy"})
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3}}
		}
	}
	us2 := src[gPods][0].(*corev1.Pod).DeepCopy()
	us2.Name, us2.Spec.Containers, us2.Status.ContainerStatuses = "us-2", []corev1.Container{{Name: "app"}}, nil
	src[gPods] = append(src[gPods], us2)
	return src
}

func search(t *testing.T, h *Handler, q *protocol.LogSearchRequest) (*protocol.LogSearchResult, *protocol.Response) {
	t.Helper()
	r := do(h, &protocol.Request{Op: protocol.OpLogSearch, LogSearch: q})
	if r.Error != nil {
		return nil, r
	}
	dec := json.NewDecoder(bytes.NewReader(r.Object))
	dec.DisallowUnknownFields()
	var res protocol.LogSearchResult
	if err := dec.Decode(&res); err != nil {
		t.Fatal(err)
	}
	return &res, r
}

func TestLogSearch(t *testing.T) {
	now := time.Now()
	h, _, _ := newHandler(t, searchApps())
	f := &fakeLogs{logs: map[string]string{
		"sre-lab-app/us-1/app": lines(now,
			"2h|ERROR too old, outside the window",
			"50m|INFO started",
			"40m|ERROR pool exhausted",
			"39m|java.lang.IllegalStateException: pool exhausted",
			"39m|\tat com.example.Pool.get(Pool.java:42)",
			"10m|WARN slow query on pool"),
		"sre-lab-app/us-1/app/previous": lines(now, "55m|panic: pool nil", "55m|goroutine 1 [running]:", "55m|main.main()"),
		"sre-lab-app/us-1/proxy":        lines(now, "30m|[error] upstream pool timed out"),
		"sre-lab-app/us-2/app":          lines(now, "20m|INFO pool ok", "5m|ERROR pool exhausted again"),
		"sre-lab-app/rd-1/app":          lines(now, "20m|ERROR redis pool"), // another app: never read
	}}
	h.LogStream = f.open

	res, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener", Text: "pool", Levels: []string{protocol.LevelError}, Previous: true})
	if res == nil {
		t.Fatalf("search: %+v", r.Error)
	}
	var got []string
	for _, e := range res.Entries {
		got = append(got, fmt.Sprintf("%s/%s%s %s: %s (%d)", e.Pod, e.Container, map[bool]string{true: "/prev"}[e.Previous], e.Level, e.Text, len(e.Stack)))
	}
	want := []string{
		"us-1/app/prev error: panic: pool nil (2)",
		"us-1/app error: ERROR pool exhausted (0)",
		"us-1/app error: java.lang.IllegalStateException: pool exhausted (1)",
		"us-1/proxy error: [error] upstream pool timed out (0)",
		"us-2/app error: ERROR pool exhausted again (0)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Levels count every text match in the window, before the level filter.
	if res.Levels[protocol.LevelError] != 5 || res.Levels[protocol.LevelWarn] != 1 || res.Levels[protocol.LevelInfo] != 1 || res.Matched != 5 || res.Truncated {
		t.Errorf("levels %v matched %d truncated %v", res.Levels, res.Matched, res.Truncated)
	}
	if len(res.Sources) != 4 || res.Sources[0].Container != "app" || !res.Sources[0].Previous || res.Sources[1].Lines != 4 {
		t.Errorf("sources %+v", res.Sources)
	}
	if !res.Until.After(res.Since) || res.Until.Sub(res.Since) != protocol.DefaultLogSearchSince {
		t.Errorf("window %v – %v", res.Since, res.Until)
	}
	// Every read asked for timestamps from the start of the window, bounded.
	for _, o := range f.calls {
		if !o.Timestamps || o.SinceTime == nil || !o.SinceTime.Time.Equal(res.Since) || o.LimitBytes == nil || o.Follow ||
			o.TailLines == nil || *o.TailLines != searchTailLines {
			t.Errorf("log options %+v", o)
		}
	}

	// One component, no previous, a window and a line limit.
	f.calls = nil
	res, _ = search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener", Component: "Deployment/sre-lab-app/url-shortener",
		Since: now.Add(-45 * time.Minute), Until: now.Add(-15 * time.Minute), MaxLines: 2})
	if len(f.calls) != 3 || len(res.Entries) != 2 || !res.Truncated || res.Matched != 4 || res.Entries[1].Text != "INFO pool ok" ||
		!strings.Contains(strings.Join(res.Notes, " "), "4 entries matched; the newest 2") {
		t.Errorf("window search: %d reads, %+v", len(f.calls), res)
	}
}

func TestLogSearchErrors(t *testing.T) {
	h, _, _ := newHandler(t, searchApps())
	f := &fakeLogs{logs: map[string]string{}, errs: map[string]error{
		"sre-lab-app/us-1/proxy": apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "us-1", errors.New("RBAC")),
		"sre-lab-app/us-2/app":   apierrors.NewBadRequest(`container "app" in pod "us-2" is waiting to start: ContainerCreating`),
	}}
	h.LogStream = f.open
	res, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener"})
	if res == nil {
		t.Fatalf("one failing container must not fail the search: %+v", r.Error)
	}
	errs := map[string]string{}
	for _, s := range res.Sources {
		errs[s.Pod+"/"+s.Container] = s.Error
	}
	if !strings.HasPrefix(errs["us-1/proxy"], "forbidden") || !strings.Contains(errs["us-2/app"], "waiting to start") || errs["us-1/app"] != "" {
		t.Errorf("source errors %v", errs)
	}

	for name, q := range map[string]*protocol.LogSearchRequest{
		"unknown app":       {App: "sre-lab-app/none"},
		"unknown component": {App: "sre-lab-app/url-shortener", Component: "Deployment/sre-lab-app/redis"},
	} {
		if _, r := search(t, h, q); code(r) != protocol.CodeNotFound {
			t.Errorf("%s: %+v", name, r.Error)
		}
	}
	for name, q := range map[string]*protocol.LogSearchRequest{
		"no app":        {},
		"bad level":     {App: "a/b", Levels: []string{"loud"}},
		"two lines":     {App: "a/b", Text: "a\nb"},
		"long text":     {App: "a/b", Text: strings.Repeat("x", protocol.MaxLogSearchText+1)},
		"too many":      {App: "a/b", MaxLines: protocol.MaxLogSearchLines + 1},
		"reversed":      {App: "a/b", Since: time.Now(), Until: time.Now().Add(-time.Hour)},
		"window > week": {App: "a/b", Since: time.Now().Add(-8 * 24 * time.Hour), Until: time.Now()},
	} {
		if _, r := search(t, h, q); code(r) != protocol.CodeBadRequest {
			t.Errorf("%s: %+v", name, r.Error)
		}
	}
	if r := do(h, &protocol.Request{Op: protocol.OpLogSearch}); code(r) != protocol.CodeBadRequest {
		t.Errorf("no logSearch: %+v", r.Error)
	}
}

func TestLogSearchLimits(t *testing.T) {
	src := searchApps()
	// Many pods: only MaxLogSearchPods are read, the rest are named.
	base := src[gPods][0].(*corev1.Pod)
	for i := range protocol.MaxLogSearchPods + 3 {
		p := base.DeepCopy()
		p.Name, p.Spec.Containers, p.Status.ContainerStatuses = fmt.Sprintf("us-x%02d", i), []corev1.Container{{Name: "app"}}, nil
		src[gPods] = append(src[gPods], p)
	}
	h, _, _ := newHandler(t, src)
	now := time.Now()
	big := strings.Repeat("y", 3000)
	logs := map[string]string{}
	for _, o := range src[gPods] {
		p := o.(*corev1.Pod)
		var ls []string
		for i := range 400 {
			ls = append(ls, fmt.Sprintf("%ds|ERROR %s %s", 3000-i, p.Name, big))
		}
		logs["sre-lab-app/"+p.Name+"/app"] = lines(now, ls...)
	}
	h.LogStream = (&fakeLogs{logs: logs}).open
	res, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener", MaxLines: protocol.MaxLogSearchLines})
	if res == nil {
		t.Fatalf("search: %+v", r.Error)
	}
	pods := map[string]bool{}
	for _, s := range res.Sources {
		pods[s.Pod] = true
	}
	notes := strings.Join(res.Notes, " ")
	if len(pods) != protocol.MaxLogSearchPods || !strings.Contains(notes, "pods not read") || !strings.Contains(notes, "and ") {
		t.Errorf("%d pods read; notes %q", len(pods), notes)
	}
	// 30 pods × 400 lines × 3 KB is far over one message: the newest fit.
	if len(r.Object) > pageBudget || !res.Truncated || len(res.Entries) == 0 || len(res.Entries) >= protocol.MaxLogSearchLines ||
		!res.Entries[len(res.Entries)-1].Time.After(res.Entries[0].Time) {
		t.Errorf("size %d, %d entries, truncated %v", len(r.Object), len(res.Entries), res.Truncated)
	}
}

// review-km-57 N-1: a container that logged more than the search reads in
// the window must still have its newest lines searched, and say that older
// ones were not.
func TestLogSearchChattyContainer(t *testing.T) {
	now := time.Now()
	h, _, _ := newHandler(t, searchApps())
	var ls []string
	for i := range searchTailLines + 2000 {
		ls = append(ls, fmt.Sprintf("%dms|INFO request %d ok", 3_000_000-i*200, i))
	}
	ls = append(ls, "1s|ERROR the newest line: pool exhausted")
	h.LogStream = (&fakeLogs{logs: map[string]string{"sre-lab-app/us-2/app": lines(now, ls...)}}).open
	res, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener", Levels: []string{protocol.LevelError}})
	if res == nil {
		t.Fatalf("search: %+v", r.Error)
	}
	if len(res.Entries) != 1 || !strings.Contains(res.Entries[0].Text, "the newest line") {
		t.Fatalf("the newest line must be searched: %+v", res.Entries)
	}
	var us2 protocol.LogSource
	for _, s := range res.Sources {
		if s.Pod == "us-2" {
			us2 = s
		}
	}
	notes := strings.Join(res.Notes, " ")
	if !us2.Truncated || us2.Lines != searchTailLines || !strings.Contains(notes, "older lines of the window were not searched for us-2/app") {
		t.Errorf("source %+v, notes %q", us2, notes)
	}
}

// review-km-57 N-2: at most searchesAtOnce searches run at the same time.
func TestLogSearchBusy(t *testing.T) {
	h, _, _ := newHandler(t, searchApps())
	h.LogStream = (&fakeLogs{logs: map[string]string{}}).open
	for range searchesAtOnce {
		h.searches <- struct{}{}
	}
	if _, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener"}); code(r) != protocol.CodeUnavailable ||
		!strings.Contains(r.Error.Message, "already running") {
		t.Fatalf("busy: %+v", r.Error)
	}
	<-h.searches
	if res, r := search(t, h, &protocol.LogSearchRequest{App: "sre-lab-app/url-shortener"}); res == nil {
		t.Fatalf("a free slot must run the search: %+v", r.Error)
	}
	if len(h.searches) != searchesAtOnce-1 {
		t.Errorf("the slot must be given back: %d in use", len(h.searches))
	}
}
