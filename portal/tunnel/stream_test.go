package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

var logOpen = &protocol.StreamOpen{Kind: protocol.StreamLogs, Namespace: "argocd", Name: "controller-0",
	Logs: &protocol.LogOptions{Container: "app", TailLines: 100}}

func next(t *testing.T, s *Stream) ([]byte, *protocol.StreamClose) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, end, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return b, end
}

func (f *fakeLocal) waitOpened(t *testing.T) openedStream {
	t.Helper()
	select {
	case o := <-f.opened:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("stream.open never reached the local agent")
	}
	return openedStream{}
}

func (f *fakeLocal) waitClosed(t *testing.T) closedStream {
	t.Helper()
	select {
	case c := <-f.closed:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("stream.close never reached the local agent")
	}
	return closedStream{}
}

func TestStreamDataThenClose(t *testing.T) {
	e := newEnv(t, Config{})
	c, l := e.activeCluster(nil)
	s, err := e.hub.OpenStream(context.Background(), c.ID, logOpen)
	if err != nil {
		t.Fatal(err)
	}
	op := l.waitOpened(t)
	if op.o.Name != "controller-0" || op.o.Logs.Container != "app" {
		t.Fatalf("stream.open %+v", op.o)
	}
	op.s.Send(op.id, &protocol.StreamData{Seq: 0, Data: []byte("line 1\n")})
	op.s.Send(op.id, &protocol.StreamData{Seq: 1, Data: []byte("line 2\n")})
	op.s.Send(op.id, &protocol.StreamClose{Reason: "log ended"})
	for _, want := range []string{"line 1\n", "line 2\n"} {
		if b, end := next(t, s); string(b) != want || end != nil {
			t.Fatalf("got %q %+v, want %q", b, end, want)
		}
	}
	if b, end := next(t, s); b != nil || end == nil || end.Reason != "log ended" {
		t.Fatalf("end: %q %+v", b, end)
	}
	s.Close() // an ended stream stays ended and sends nothing
	if _, end := next(t, s); end == nil || end.Reason != "log ended" {
		t.Fatalf("after Close: %+v", end)
	}
	select {
	case <-l.closed:
		t.Fatal("Close of an ended stream told the agent")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamPortalCloseTellsAgent(t *testing.T) {
	e := newEnv(t, Config{})
	c, l := e.activeCluster(nil)
	s, _ := e.hub.OpenStream(context.Background(), c.ID, logOpen)
	op := l.waitOpened(t)
	s.Close()
	if got := l.waitClosed(t); got.id != op.id {
		t.Fatalf("close for %s, want %s", got.id, op.id)
	}
	op.s.Send(op.id, &protocol.StreamData{Seq: 0, Data: []byte("late")}) // dropped quietly
	if _, end := next(t, s); end == nil || end.Reason != "closed by the portal" {
		t.Fatalf("end %+v", end)
	}
}

func TestStreamOutOfOrderAndSlowReader(t *testing.T) {
	e := newEnv(t, Config{})
	c, l := e.activeCluster(nil)

	s, _ := e.hub.OpenStream(context.Background(), c.ID, logOpen)
	op := l.waitOpened(t)
	op.s.Send(op.id, &protocol.StreamData{Seq: 1, Data: []byte("skipped seq 0")})
	l.waitClosed(t)
	if _, end := next(t, s); end == nil || end.Error == nil || end.Error.Code != protocol.CodeInternal {
		t.Fatalf("reader end: %+v", end)
	}

	// Nobody reads: after streamBuffer chunks the stream ends as too slow,
	// the chunks before are still delivered, and the agent is told.
	s2, _ := e.hub.OpenStream(context.Background(), c.ID, logOpen)
	op2 := l.waitOpened(t)
	for i := 0; i <= streamBuffer; i++ {
		op2.s.Send(op2.id, &protocol.StreamData{Seq: uint64(i), Data: []byte{'x'}})
	}
	if got := l.waitClosed(t); got.id != op2.id {
		t.Fatalf("slow reader close for %s", got.id)
	}
	for i := 0; i < streamBuffer; i++ {
		if b, end := next(t, s2); len(b) != 1 || end != nil {
			t.Fatalf("chunk %d: %q %+v", i, b, end)
		}
	}
	if _, end := next(t, s2); end == nil || end.Error.Code != protocol.CodeUnavailable {
		t.Fatalf("end: %+v", end)
	}
}

func TestStreamEndsWhenAgentDetaches(t *testing.T) {
	e := newEnv(t, Config{})
	c, _, detach := e.attach(newFakeLocal(nil))
	s, _ := e.hub.OpenStream(context.Background(), c.ID, logOpen)
	detach()
	if _, end := next(t, s); end == nil || end.Error == nil || end.Error.Code != protocol.CodeUnavailable {
		t.Fatalf("end: %+v", end)
	}
}

func TestOpenStreamRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	if _, err := e.hub.OpenStream(context.Background(), "c-0000000000000000", logOpen); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("unknown cluster: %v", err)
	}
	c, _ := e.activeCluster(nil)
	if _, err := e.hub.OpenStream(context.Background(), c.ID, &protocol.StreamOpen{Kind: "exec", Namespace: "a", Name: "b"}); err == nil {
		t.Fatal("an exec stream was accepted")
	}
}
