package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemap/agent/cluster/appmap"
	"kubemap/agent/cluster/logsearch"
	"kubemap/pkg/protocol"
)

const (
	// searchTailLines and searchLogBytes are how much of each container's
	// log a search reads: the newest lines of the window, asked of the
	// kubelet with tailLines next to sinceTime (review-km-57 N-1: limitBytes
	// alone counts from the window's start, so a chatty container would
	// lose its newest lines). A source that fills either is truncated.
	searchTailLines = 10000
	searchLogBytes  = 1 << 20
	// searchesAtOnce bounds the searches the agent runs at the same time,
	// across users and tabs, to spare the kubelets (review-km-57 N-2).
	searchesAtOnce = 2
	// searchParallel is how many container logs are read at once.
	searchParallel = 6
	// searchBudget is how long the reads may take, so the answer still
	// arrives within DefaultTimeout.
	searchBudget = 11 * time.Second
)

// target is one container log to read.
type target struct {
	src logsearch.Source
}

// logSearch searches the logs of every pod of an app (KM-57). The pods come
// from the app map built over the cache; each container's log is read with
// pods/log (timestamps on, from the start of the window), so nothing new
// is needed in the RBAC and no kubelet is called.
func (h *Handler) logSearch(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	if h.Kube == nil && h.LogStream == nil {
		return nil, perr(protocol.CodeUnavailable, "logs are not available")
	}
	q := req.LogSearch
	now := time.Now().UTC()
	until := q.Until.UTC()
	if q.Until.IsZero() || until.After(now) {
		until = now
	}
	since := q.Since.UTC()
	if q.Since.IsZero() {
		since = until.Add(-protocol.DefaultLogSearchSince)
	}
	if until.Sub(since) > protocol.MaxLogSearchSince {
		since = until.Add(-protocol.MaxLogSearchSince)
	}
	maxLines := q.MaxLines
	if maxLines == 0 {
		maxLines = protocol.DefaultLogSearchLines
	}

	select {
	case h.searches <- struct{}{}:
		defer func() { <-h.searches }()
	default:
		return nil, perr(protocol.CodeUnavailable, "%d log searches are already running; try again in a few seconds", searchesAtOnce)
	}

	pods, err := h.appPods(q.App, q.Component)
	if err != nil {
		return nil, err
	}
	res := &protocol.LogSearchResult{App: q.App, Since: since, Until: until, Entries: []protocol.LogEntry{},
		Levels: map[string]int{}, Sources: []protocol.LogSource{}}
	for _, l := range protocol.LogLevels {
		res.Levels[l] = 0
	}
	if len(pods) > protocol.MaxLogSearchPods {
		var names []string
		for _, p := range pods[protocol.MaxLogSearchPods:] {
			names = append(names, p.Namespace+"/"+p.Name)
		}
		res.Notes = append(res.Notes, fmt.Sprintf("%d pods not read (at most %d per search): %s; search one component", len(names), protocol.MaxLogSearchPods, firstNames(names, 5)))
		pods = pods[:protocol.MaxLogSearchPods]
	}

	var targets []target
	for _, p := range pods {
		restarted := map[string]bool{}
		for _, cs := range p.Status.ContainerStatuses {
			restarted[cs.Name] = cs.RestartCount > 0
		}
		for _, c := range p.Spec.Containers {
			base := logsearch.Source{Namespace: p.Namespace, Pod: p.Name, Container: c.Name}
			if q.Previous && restarted[c.Name] {
				prev := base
				prev.Previous = true
				targets = append(targets, target{prev})
			}
			targets = append(targets, target{base})
		}
	}

	f := logsearch.Filter{Text: q.Text, Levels: q.Levels, Since: since, Until: until}
	rctx, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()
	type result struct {
		src     protocol.LogSource
		entries []protocol.LogEntry
		levels  map[string]int
		full    bool // the byte safety cap ended the read: the newest lines may be missing
	}
	results := make([]result, len(targets))
	sem := make(chan struct{}, searchParallel)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := result{src: protocol.LogSource{Namespace: t.src.Namespace, Pod: t.src.Pod, Container: t.src.Container, Previous: t.src.Previous}, levels: map[string]int{}}
			defer func() { results[i] = r }()
			if rctx.Err() != nil {
				r.src.Error = "not read: the search ran out of time; search one component or a shorter window"
				return
			}
			data, cut, full, err := h.readSearchLog(rctx, t.src, since)
			if err != nil {
				r.src.Error = logError(err)
				return
			}
			r.src.Truncated = cut
			r.full = full
			for _, e := range logsearch.Parse(data, t.src) {
				if !f.InWindow(e) {
					continue
				}
				r.src.Lines++
				if !f.MatchesText(e) {
					continue
				}
				r.levels[e.Level]++
				if !f.MatchesLevel(e) {
					continue
				}
				r.src.Matched++
				r.entries = append(r.entries, e)
			}
			// Only the newest maxLines of all containers are returned, so
			// no container needs to keep more.
			if len(r.entries) > maxLines {
				r.entries = r.entries[len(r.entries)-maxLines:]
			}
		}()
	}
	wg.Wait()

	var cutLogs, fullLogs []string
	for _, r := range results {
		if r.full {
			fullLogs = append(fullLogs, r.src.Pod+"/"+r.src.Container)
		}
		res.Sources = append(res.Sources, r.src)
		res.Entries = append(res.Entries, r.entries...)
		res.Matched += r.src.Matched
		for l, n := range r.levels {
			res.Levels[l] += n
		}
		if r.src.Truncated {
			cutLogs = append(cutLogs, r.src.Pod+"/"+r.src.Container)
		}
	}
	if len(cutLogs) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("only the newest %d lines (at most %d MiB) of each log were searched; older lines of the window were not searched for %s: shorten the window", searchTailLines, searchLogBytes>>20, firstNames(cutLogs, 5)))
	}
	if len(fullLogs) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("the read stopped at %d MiB (very long lines), so the newest lines may be missing for %s", maxLogRead>>20, firstNames(fullLogs, 5)))
	}
	logsearch.Merge(res.Entries)
	if len(res.Entries) > maxLines {
		res.Entries = res.Entries[len(res.Entries)-maxLines:]
	}
	res.Truncated = res.Matched > len(res.Entries)

	raw, err := json.Marshal(res)
	for err == nil && len(raw) > pageBudget && len(res.Entries) > 0 {
		// Long lines and stacks: drop the oldest quarter until it fits.
		res.Entries = res.Entries[len(res.Entries)/4+1:]
		res.Truncated = true
		raw, err = json.Marshal(res)
	}
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		res.Notes = append(res.Notes, fmt.Sprintf("%d entries matched; the newest %d are shown: narrow the text, the levels or the window", res.Matched, len(res.Entries)))
		if raw, err = json.Marshal(res); err != nil {
			return nil, err
		}
	}
	return &protocol.Response{Object: raw}, nil
}

// appPods returns the pods of an app's components (or one component), from
// the app map, sorted by namespace and name.
func (h *Handler) appPods(app, component string) ([]*corev1.Pod, error) {
	m := appmap.Build(h.analyse())
	i := slices.IndexFunc(m.Apps, func(a protocol.MapApp) bool { return a.ID == app })
	if i < 0 {
		return nil, perr(protocol.CodeNotFound, "no app %s", app)
	}
	found := component == ""
	var pods []*corev1.Pod
	seen := map[string]bool{}
	for _, c := range m.Apps[i].Components {
		if component != "" && c.ID != component {
			continue
		}
		found = true
		for _, mp := range c.Pods {
			key := c.Namespace + "/" + mp.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			if p := h.cachedPod(c.Namespace, mp.Name); p != nil {
				pods = append(pods, p)
			}
		}
	}
	if !found {
		return nil, perr(protocol.CodeNotFound, "app %s has no component %s", app, component)
	}
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return pods, nil
}

// readSearchLog reads the newest lines of one container log inside the
// window, with the kubelet's timestamps: at most searchTailLines, of which
// the newest searchLogBytes are kept. cut is true when older lines of the
// window may exist that were not read; full when the byte safety cap ended
// the read, so even the newest lines may be missing.
func (h *Handler) readSearchLog(ctx context.Context, src logsearch.Source, since time.Time) (data []byte, cut, full bool, err error) {
	limit, tail := int64(maxLogRead), int64(searchTailLines)
	opts := &corev1.PodLogOptions{Container: src.Container, Previous: src.Previous, Timestamps: true,
		SinceTime: &metav1.Time{Time: since}, TailLines: &tail, LimitBytes: &limit}
	var stream io.ReadCloser
	if h.LogStream != nil {
		stream, err = h.LogStream(ctx, src.Namespace, src.Pod, opts)
	} else {
		stream, err = h.Kube.CoreV1().Pods(src.Namespace).GetLogs(src.Pod, opts).Stream(ctx)
	}
	if err != nil {
		return nil, false, false, err
	}
	defer stream.Close()
	counted := &countingReader{r: stream}
	data, cut, err = readTail(counted, searchLogBytes)
	if err != nil {
		return nil, false, false, err
	}
	// The kubelet sent searchTailLines lines: the window may hold more.
	return data, cut || counted.lines >= searchTailLines, counted.bytes >= limit, nil
}

// countingReader counts the bytes and lines read through it.
type countingReader struct {
	r     io.Reader
	bytes int64
	lines int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += int64(n)
	c.lines += bytes.Count(p[:n], []byte{'\n'})
	return n, err
}

// logError is the short reason a container log could not be read.
func logError(err error) string {
	r := fail(err)
	if r.Error == nil {
		return err.Error()
	}
	return string(r.Error.Code) + ": " + clipMsg(r.Error.Message)
}

func clipMsg(s string) string {
	if len(s) > 300 {
		return s[:300] + " …"
	}
	return s
}

// firstNames lists at most n names, then how many more.
func firstNames(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
}
