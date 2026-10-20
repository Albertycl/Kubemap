package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"kubemap/pkg/protocol"
)

// recorder is a Sender that keeps every message, optionally slowly.
type recorder struct {
	mu    sync.Mutex
	msgs  []msg
	delay time.Duration
	fail  bool
	block chan struct{} // when set, Send waits for it to close
}

type msg struct {
	id string
	p  protocol.Payload
}

func (r *recorder) Send(id string, p protocol.Payload) error {
	if r.block != nil {
		if _, data := p.(*protocol.StreamData); data {
			<-r.block
		}
	}
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		if _, data := p.(*protocol.StreamData); data {
			return errors.New("tunnel closed")
		}
	}
	r.msgs = append(r.msgs, msg{id, p})
	return nil
}

func (r *recorder) snapshot() []msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]msg(nil), r.msgs...)
}

// waitClose waits for the stream.close of id and returns it with all data.
func (r *recorder) waitClose(t *testing.T, id string) (*protocol.StreamClose, []byte, []uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var data []byte
		var seqs []uint64
		for _, m := range r.snapshot() {
			if m.id != id {
				continue
			}
			switch p := m.p.(type) {
			case *protocol.StreamData:
				data = append(data, p.Data...)
				seqs = append(seqs, p.Seq)
			case *protocol.StreamClose:
				return p, data, seqs
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no stream.close for %s; got %d messages", id, len(r.snapshot()))
	return nil, nil, nil
}

// pipeLogs serves each pod name from its own pipe, like a followed log, and
// records the options it was opened with.
type pipeLogs struct {
	mu    sync.Mutex
	pipes map[string]*io.PipeWriter
	opts  map[string]*corev1.PodLogOptions
	err   error
}

func newPipeLogs() *pipeLogs {
	return &pipeLogs{pipes: map[string]*io.PipeWriter{}, opts: map[string]*corev1.PodLogOptions{}}
}

func (p *pipeLogs) source(_ context.Context, _, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	r, w := io.Pipe()
	p.pipes[pod], p.opts[pod] = w, opts
	return r, nil
}

func (p *pipeLogs) writer(t *testing.T, pod string) *io.PipeWriter {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		w := p.pipes[pod]
		p.mu.Unlock()
		if w != nil {
			return w
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("log for %s never opened", pod)
	return nil
}

func open(ns, pod string, opts *protocol.LogOptions) *protocol.StreamOpen {
	return &protocol.StreamOpen{Kind: protocol.StreamLogs, Namespace: ns, Name: pod, Logs: opts}
}

func TestFollowUntilLogEnds(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: 5 * time.Millisecond}
	m.Open(context.Background(), "s-1", open("shop", "web-1", &protocol.LogOptions{Container: "app", Timestamps: true}))
	w := logs.writer(t, "web-1")
	var want bytes.Buffer
	for i := range 50 {
		line := strings.Repeat("x", i) + "\x1b[32m ok\x1b[0m\n" // colour codes survive
		want.WriteString(line)
		io.WriteString(w, line)
		if i%10 == 0 {
			time.Sleep(10 * time.Millisecond) // let a few flushes happen
		}
	}
	w.Close() // the container stopped
	closed, data, seqs := rec.waitClose(t, "s-1")
	if closed.Error != nil || !strings.Contains(closed.Reason, "log ended") {
		t.Errorf("close = %+v", closed)
	}
	if !bytes.Equal(data, want.Bytes()) {
		t.Errorf("data differs: got %d bytes, want %d", len(data), want.Len())
	}
	for i, s := range seqs {
		if s != uint64(i) {
			t.Fatalf("seqs = %v, want 0,1,2,…", seqs)
		}
	}
	if len(seqs) < 2 {
		t.Errorf("expected several chunks, got %d", len(seqs))
	}
	o := logs.opts["web-1"]
	if !o.Follow || o.Container != "app" || !o.Timestamps || *o.TailLines != DefaultTailLines {
		t.Errorf("log options = %+v", o)
	}
	if m.OpenCount() != 0 {
		t.Errorf("%d streams still open", m.OpenCount())
	}
}

func TestChunksAreBounded(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, ChunkSize: 1000, QueueLen: 1000, FlushInterval: time.Hour}
	m.Open(context.Background(), "s-1", open("a", "p", nil))
	w := logs.writer(t, "p")
	io.WriteString(w, strings.Repeat("y", 5500))
	w.Close()
	_, data, seqs := rec.waitClose(t, "s-1")
	if len(data) != 5500 || len(seqs) != 6 {
		t.Errorf("%d bytes in %d chunks, want 5500 in 6", len(data), len(seqs))
	}
	for _, m := range rec.snapshot() {
		if d, ok := m.p.(*protocol.StreamData); ok && len(d.Data) > 1000 {
			t.Errorf("chunk of %d bytes", len(d.Data))
		}
	}
}

func TestPortalCancels(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: 5 * time.Millisecond}
	m.Open(context.Background(), "s-1", open("a", "p", nil))
	w := logs.writer(t, "p")
	io.WriteString(w, "hello\n")
	time.Sleep(30 * time.Millisecond)
	m.Close("s-1")
	m.CloseAll() // waits for the goroutines
	for _, x := range rec.snapshot() {
		if _, ok := x.p.(*protocol.StreamClose); ok {
			t.Error("the agent must not answer the portal's own stream.close")
		}
	}
	if m.OpenCount() != 0 {
		t.Error("stream still open")
	}
	m.Close("unknown") // harmless
}

func TestSlowReaderIsClosed(t *testing.T) {
	logs := newPipeLogs()
	rec := &recorder{block: make(chan struct{})}
	m := &Manager{Logs: logs.source, Sender: rec, ChunkSize: 100, QueueLen: 2, FlushInterval: time.Millisecond}
	m.Open(context.Background(), "s-1", open("a", "chatty", nil))
	w := logs.writer(t, "chatty")
	go func() {
		for range 1000 {
			if _, err := io.WriteString(w, strings.Repeat("z", 100)); err != nil {
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(rec.block) // the tunnel moves again
	closed, _, _ := rec.waitClose(t, "s-1")
	if closed.Error == nil || closed.Error.Code != protocol.CodeUnavailable || !strings.Contains(closed.Error.Message, "too slow") {
		t.Errorf("close = %+v", closed)
	}
}

func TestSendFailure(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{fail: true}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: time.Millisecond}
	m.Open(context.Background(), "s-1", open("a", "p", nil))
	w := logs.writer(t, "p")
	go func() {
		for {
			if _, err := io.WriteString(w, "line\n"); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	closed, _, _ := rec.waitClose(t, "s-1")
	if closed.Error == nil || closed.Error.Message != "tunnel send failed" {
		t.Errorf("close = %+v", closed)
	}
}

func TestPreviousLogsAreNotFollowed(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: time.Millisecond}
	m.Open(context.Background(), "s-1", open("a", "crashed", &protocol.LogOptions{Previous: true, TailLines: 20000}))
	w := logs.writer(t, "crashed")
	io.WriteString(w, "panic: config missing\n")
	w.Close()
	closed, data, _ := rec.waitClose(t, "s-1")
	if closed.Reason != "end of previous logs" || string(data) != "panic: config missing\n" {
		t.Errorf("close = %+v data %q", closed, data)
	}
	if o := logs.opts["crashed"]; o.Follow || !o.Previous || *o.TailLines != 10000 {
		t.Errorf("options = %+v", o)
	}
}

func TestSinceSeconds(t *testing.T) {
	o := options(open("a", "p", &protocol.LogOptions{SinceSeconds: 300}))
	if o.TailLines != nil || *o.SinceSeconds != 300 || !o.Follow {
		t.Errorf("options = %+v", o)
	}
}

func TestOpenErrors(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, MaxStreams: 1, FlushInterval: time.Millisecond}
	// Invalid open: unknown kind.
	m.Open(context.Background(), "bad", &protocol.StreamOpen{Kind: "exec", Namespace: "a", Name: "p"})
	if c, _, _ := rec.waitClose(t, "bad"); c.Error.Code != protocol.CodeUnsupported {
		t.Errorf("exec stream: %+v", c.Error)
	}
	// One stream is allowed; a second is refused; a duplicate id too.
	m.Open(context.Background(), "s-1", open("a", "p", nil))
	logs.writer(t, "p")
	m.Open(context.Background(), "s-1", open("a", "q", nil))
	if c, _, _ := rec.waitClose(t, "s-1"); c.Error.Code != protocol.CodeBadRequest {
		t.Errorf("duplicate id: %+v", c.Error)
	}
	m.Open(context.Background(), "s-2", open("a", "q", nil))
	if c, _, _ := rec.waitClose(t, "s-2"); c.Error.Code != protocol.CodeUnavailable {
		t.Errorf("too many streams: %+v", c.Error)
	}
	m.CloseAll()
	// The API refuses: the error code reaches the portal.
	logs.err = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone")
	m.Open(context.Background(), "s-3", open("a", "gone", nil))
	if c, _, _ := rec.waitClose(t, "s-3"); c.Error.Code != protocol.CodeNotFound {
		t.Errorf("missing pod: %+v", c.Error)
	}
	logs.err = apierrors.NewBadRequest("previous terminated container \"app\" in pod \"x\" not found")
	m.Open(context.Background(), "s-4", open("a", "x", &protocol.LogOptions{Previous: true}))
	if c, _, _ := rec.waitClose(t, "s-4"); c.Error.Code != protocol.CodeBadRequest || !strings.Contains(c.Error.Message, "previous terminated") {
		t.Errorf("no previous container: %+v", c.Error)
	}
}

func TestReadError(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: time.Millisecond}
	m.Open(context.Background(), "s-1", open("a", "p", nil))
	w := logs.writer(t, "p")
	io.WriteString(w, "partial\n")
	w.CloseWithError(errors.New("connection reset by peer"))
	closed, data, _ := rec.waitClose(t, "s-1")
	if closed.Error == nil || closed.Error.Code != protocol.CodeInternal || string(data) != "partial\n" {
		t.Errorf("close = %+v data %q", closed, data)
	}
}

func TestKubeLogs(t *testing.T) {
	src := KubeLogs(kubefake.NewClientset())
	rc, err := src(context.Background(), "a", "p", &corev1.PodLogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "fake logs" {
		t.Errorf("fake logs = %q", b)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	for err, want := range map[error]protocol.ErrorCode{
		apierrors.NewForbidden(schema.GroupResource{}, "x", nil): protocol.CodeForbidden,
		apierrors.NewTimeoutError("slow", 1):                     protocol.CodeTimeout,
		errors.New(strings.Repeat("e", 500)):                     protocol.CodeInternal,
	} {
		e := apiError(err)
		if e.Code != want || len(e.Message) > 300 {
			t.Errorf("apiError(%.20v) = %s (%d chars)", err, e.Code, len(e.Message))
		}
	}
}

// The dev node really does this: the kubelet ends a followed log with its
// inotify error. The stream must say so, not "log ended".
func TestKubeletFollowFailure(t *testing.T) {
	logs, rec := newPipeLogs(), &recorder{}
	m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: time.Millisecond}
	m.Open(context.Background(), "s-1", open("kube-system", "traefik", nil))
	w := logs.writer(t, "traefik")
	io.WriteString(w, "INF Starting provider\nfailed to create fsnotify watcher: too many open files")
	w.Close()
	closed, data, _ := rec.waitClose(t, "s-1")
	if closed.Error == nil || closed.Error.Code != protocol.CodeUnavailable || !strings.Contains(closed.Error.Message, "fs.inotify.max_user_instances") {
		t.Errorf("close = %+v", closed)
	}
	if !strings.HasPrefix(string(data), "INF Starting provider") {
		t.Errorf("data = %q", data)
	}
	// The same text in the middle of a log, or in previous logs, is just text.
	for name, c := range map[string]struct {
		text     string
		previous bool
	}{
		"mid-log":  {"failed to create fsnotify watcher: x\nlater line\n", false},
		"previous": {"failed to create fsnotify watcher: x", true},
	} {
		logs, rec := newPipeLogs(), &recorder{}
		m := &Manager{Logs: logs.source, Sender: rec, FlushInterval: time.Millisecond}
		m.Open(context.Background(), "s", open("a", "p", &protocol.LogOptions{Previous: c.previous}))
		w := logs.writer(t, "p")
		io.WriteString(w, c.text)
		w.Close()
		if closed, _, _ := rec.waitClose(t, "s"); closed.Error != nil {
			t.Errorf("%s: %+v", name, closed.Error)
		}
	}
}

func TestTailKeeper(t *testing.T) {
	tk := &tailKeeper{r: strings.NewReader(strings.Repeat("a", 1000) + "\nfailed to watch file \"/var/log/pods/x\": no space left on device")}
	io.ReadAll(tk)
	if len(tk.last) != tailKeep || !tk.followFailed() {
		t.Errorf("tail %d bytes, followFailed %v", len(tk.last), tk.followFailed())
	}
}
