// Package stream runs pod log streams for the portal (kubectl logs -f through
// the tunnel). The portal sends stream.open; the agent answers with
// stream.data chunks and ends with stream.close. Either side may close.
//
// Each stream is bounded: output is batched into chunks of at most
// ChunkSize bytes, at most QueueLen chunks wait to be sent, and a stream
// whose reader falls behind is closed with code "unavailable" instead of
// buffering without limit (transport rule 5 in docs/protocol.md).
package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"kubemap/pkg/protocol"
)

// Defaults for Manager; zero fields take these.
const (
	DefaultMaxStreams    = 16
	DefaultQueueLen      = 8
	DefaultChunkSize     = 64 << 10
	DefaultFlushInterval = 100 * time.Millisecond
	// DefaultTailLines is where a follow stream starts when the portal gives
	// no tailLines or sinceSeconds: the last lines, then live output.
	DefaultTailLines = 100
)

// Sender delivers a message to the portal; the tunnel (KM-21) implements it.
// Send may block while the connection is busy.
type Sender interface {
	Send(id string, p protocol.Payload) error
}

// LogSource opens a pod log. The default uses the Kubernetes API; tests
// replace it.
type LogSource func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)

// KubeLogs returns a LogSource that reads through the Kubernetes API.
func KubeLogs(kube kubernetes.Interface) LogSource {
	return func(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return kube.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	}
}

// Manager owns the open streams of one tunnel connection.
type Manager struct {
	Logs          LogSource
	Sender        Sender
	MaxStreams    int
	QueueLen      int
	ChunkSize     int
	FlushInterval time.Duration

	mu      sync.Mutex
	streams map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// Open starts a stream for a stream.open message with the given id. It
// returns at once; the stream then runs until the log ends, the pod goes
// away, the portal closes it, or Close/CloseAll is called. A stream that
// cannot start is answered with a stream.close carrying the error.
func (m *Manager) Open(ctx context.Context, id string, req *protocol.StreamOpen) {
	m.OpenFor(ctx, id, req, m.Sender)
}

// OpenFor is Open with the connection to send on. Each stream keeps its own
// Sender, so streams of a new connection never share a field with those of
// the old one.
func (m *Manager) OpenFor(ctx context.Context, id string, req *protocol.StreamOpen, sender Sender) {
	if err := req.Validate(); err != nil {
		closeWith(sender, id, "", protocol.AsError(err))
		return
	}
	m.mu.Lock()
	if m.streams == nil {
		m.streams = map[string]context.CancelFunc{}
	}
	if _, dup := m.streams[id]; dup {
		m.mu.Unlock()
		closeWith(sender, id, "", &protocol.Error{Code: protocol.CodeBadRequest, Message: "stream id already in use"})
		return
	}
	if len(m.streams) >= orDefault(m.MaxStreams, DefaultMaxStreams) {
		m.mu.Unlock()
		closeWith(sender, id, "", &protocol.Error{Code: protocol.CodeUnavailable, Message: "too many open log streams"})
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	m.streams[id] = cancel
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		reason, perr := m.run(sctx, id, req, sender)
		m.mu.Lock()
		_, stillOpen := m.streams[id]
		delete(m.streams, id)
		m.mu.Unlock()
		cancel()
		if stillOpen { // closed by the portal: it knows, no reply
			closeWith(sender, id, reason, perr)
		}
	}()
}

// Close ends a stream because the portal sent stream.close. The agent does
// not answer it.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	cancel, ok := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

// CloseAll ends every stream, for example when the tunnel drops, and waits
// for them to stop.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	for id, cancel := range m.streams {
		cancel()
		delete(m.streams, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// OpenCount counts the running streams.
func (m *Manager) OpenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

func closeWith(s Sender, id, reason string, err *protocol.Error) {
	_ = s.Send(id, &protocol.StreamClose{Reason: reason, Error: err})
}

func orDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

// options turns the portal's log options into a follow request. Previous
// logs belong to a container that is gone, so they are read once, not
// followed.
func options(req *protocol.StreamOpen) *corev1.PodLogOptions {
	opts := &corev1.PodLogOptions{Follow: true}
	tail := int64(DefaultTailLines)
	if l := req.Logs; l != nil {
		opts.Container, opts.Previous, opts.Timestamps = l.Container, l.Previous, l.Timestamps
		if l.SinceSeconds > 0 {
			opts.SinceSeconds = &l.SinceSeconds
			tail = 0
		}
		if l.TailLines > 0 {
			tail = min(l.TailLines, 10000)
		}
	}
	if tail > 0 {
		opts.TailLines = &tail
	}
	if opts.Previous {
		opts.Follow = false
	}
	return opts
}

// errReaderTooSlow ends a stream whose chunks are not sent fast enough.
var errReaderTooSlow = errors.New("reader too slow")

// run streams one log. It returns the close reason and, when the stream
// failed, the error for the portal.
func (m *Manager) run(ctx context.Context, id string, req *protocol.StreamOpen, sender Sender) (string, *protocol.Error) {
	opts := options(req)
	rc, err := m.Logs(ctx, req.Namespace, req.Name, opts)
	if err != nil {
		return "", apiError(err)
	}
	defer rc.Close()

	queue := make(chan []byte, orDefault(m.QueueLen, DefaultQueueLen))
	sendErr := make(chan error, 1)
	// A failed send stops the reading too: the tunnel is gone.
	pctx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	go func() { // the sender: one chunk at a time, in order
		var seq uint64
		for chunk := range queue {
			if err := sender.Send(id, &protocol.StreamData{Seq: seq, Data: chunk}); err != nil {
				sendErr <- err
				stopReading()
				for range queue { // drain so the reader never blocks
				}
				return
			}
			seq++
		}
		sendErr <- nil
	}()

	tail := &tailKeeper{r: rc}
	readErr := m.pump(pctx, tail, queue)
	close(queue)
	serr := <-sendErr

	switch {
	case serr != nil:
		return "", &protocol.Error{Code: protocol.CodeUnavailable, Message: "tunnel send failed"}
	case errors.Is(readErr, errReaderTooSlow):
		return "", &protocol.Error{Code: protocol.CodeUnavailable, Message: "reader too slow; reopen the stream"}
	case ctx.Err() != nil:
		return "cancelled", nil
	case readErr == nil && opts.Follow && tail.followFailed():
		// The kubelet could not follow the file and ended the stream with
		// its error text: live logs are broken on that node, not finished.
		return "", &protocol.Error{Code: protocol.CodeUnavailable, Message: followFailedMessage}
	case readErr == nil && opts.Previous:
		return "end of previous logs", nil
	case readErr == nil:
		return "log ended (container stopped or pod deleted)", nil
	}
	return "", apiError(readErr)
}

// pump reads the log and queues chunks: flushed when ChunkSize is reached or
// FlushInterval passes, so a chatty pod gives few large messages and a quiet
// one still shows each line quickly. It returns nil at the end of the log.
func (m *Manager) pump(ctx context.Context, r io.Reader, queue chan<- []byte) error {
	size := min(orDefault(m.ChunkSize, DefaultChunkSize), protocol.MaxStreamChunk)
	flushEvery := m.FlushInterval
	if flushEvery <= 0 {
		flushEvery = DefaultFlushInterval
	}
	reads := make(chan []byte)
	readDone := make(chan error, 1)
	go func() {
		for {
			b := make([]byte, size)
			n, err := r.Read(b)
			if n > 0 {
				select {
				case reads <- b[:n]:
				case <-ctx.Done():
					readDone <- ctx.Err()
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readDone <- err
				return
			}
		}
	}()

	var buf []byte
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		select {
		case queue <- buf:
			buf = nil
			return nil
		default:
			return errReaderTooSlow
		}
	}
	timer := time.NewTicker(flushEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-reads:
			for len(b) > 0 {
				room := size - len(buf)
				take := min(room, len(b))
				buf = append(buf, b[:take]...)
				b = b[take:]
				if len(buf) >= size {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		case <-timer.C:
			if err := flush(); err != nil {
				return err
			}
		case err := <-readDone:
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		}
	}
}

// apiError maps a Kubernetes error to a protocol error.
func apiError(err error) *protocol.Error {
	code := protocol.CodeInternal
	switch {
	case apierrors.IsNotFound(err):
		code = protocol.CodeNotFound
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		code = protocol.CodeForbidden
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		// For example "previous terminated container not found", or a
		// container name that is not in the pod.
		code = protocol.CodeBadRequest
	case apierrors.IsTimeout(err), errors.Is(err, context.DeadlineExceeded):
		code = protocol.CodeTimeout
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return &protocol.Error{Code: code, Message: msg}
}

// followFailedMessage explains the kubelet's inotify failure. kubectl logs -f
// fails the same way; the fix is on the node.
const followFailedMessage = "the node's kubelet cannot follow this log: failed to create fsnotify watcher " +
	"(too many open files). The node has run out of inotify instances; raise " +
	"fs.inotify.max_user_instances (for example to 512) on the node. Logs without follow still work."

// kubeletFollowErrors are texts the kubelet appends to a followed log when
// it cannot watch the file any more.
var kubeletFollowErrors = []string{"failed to create fsnotify watcher", "failed to watch file"}

// tailKeeper passes reads through and remembers the last bytes, so the end
// of the stream can be checked for the kubelet's error text.
type tailKeeper struct {
	r    io.Reader
	last []byte
}

const tailKeep = 256

func (t *tailKeeper) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if n > 0 {
		t.last = append(t.last, b[:n]...)
		if len(t.last) > tailKeep {
			t.last = append(t.last[:0], t.last[len(t.last)-tailKeep:]...)
		}
	}
	return n, err
}

func (t *tailKeeper) followFailed() bool {
	// The error text is the last line, without a trailing newline.
	lastLine := t.last
	if i := bytes.LastIndexByte(bytes.TrimRight(lastLine, "\n"), '\n'); i >= 0 {
		lastLine = lastLine[i+1:]
	}
	for _, e := range kubeletFollowErrors {
		if bytes.Contains(lastLine, []byte(e)) {
			return true
		}
	}
	return false
}
