package report

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kubemap/agent/node/identity"
	"kubemap/agent/tunnel"
	"kubemap/pkg/protocol"
)

type fakeCollector struct {
	mu    sync.Mutex
	calls int
	fail  map[int]bool
}

func (f *fakeCollector) Collect() (*protocol.Metrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail[f.calls] {
		return nil, errors.New("read /host/proc/stat: no such file")
	}
	return &protocol.Metrics{Time: time.Now(), NodeName: "k8server"}, nil
}

type fakeSender struct {
	mu   sync.Mutex
	sent []protocol.Payload
	err  error
}

func (f *fakeSender) Send(_ string, p protocol.Payload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, p)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func TestPusher(t *testing.T) {
	col := &fakeCollector{fail: map[int]bool{2: true}}
	snd := &fakeSender{}
	p := &Pusher{Collector: col, Sender: snd, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for snd.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
	if snd.count() < 3 {
		t.Fatalf("sent %d samples", snd.count())
	}
	// The failed sample was skipped, not sent and not fatal.
	if _, ok := snd.sent[0].(*protocol.Metrics); !ok {
		t.Errorf("sent %T", snd.sent[0])
	}
	// A failed send ends the loop.
	snd.err = errors.New("tunnel closed")
	if err := (&Pusher{Collector: col, Sender: snd, Interval: time.Millisecond}).Run(context.Background()); err == nil {
		t.Error("send error must end the loop")
	}
}

func TestRegistrarRenewsWithFreshToken(t *testing.T) {
	now := time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC)
	loads := 0
	load := func(at time.Time) (*identity.Identity, error) {
		loads++
		return &identity.Identity{ClusterID: "c-1", NodeName: "k8server", Token: "satok_example",
			Claims: &identity.Claims{IssuedAt: at, Expiry: at.Add(time.Hour)}}, nil
	}
	reg, until := Registrar(load, "v1", func() time.Time { return now })
	if got := until(); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("until before any register = %v", got)
	}
	r, err := reg(tunnel.State{})
	if err != nil || r.Role != protocol.RoleNode || r.NodeName != "k8server" || r.ServiceAccountToken != "satok_example" {
		t.Fatalf("register = %+v, %v", r, err)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("register invalid: %v", err)
	}
	// 80% of a 1 h token.
	if got := until(); !got.Equal(now.Add(48 * time.Minute)) {
		t.Errorf("renew at %v, want +48m", got.Sub(now))
	}
	reg(tunnel.State{})
	if loads != 2 {
		t.Errorf("identity loaded %d times; must reload on every connect", loads)
	}
	_, err = func() (*protocol.Register, error) {
		bad, _ := Registrar(func(time.Time) (*identity.Identity, error) { return nil, errors.New("NODE_NAME is not set") }, "v1", time.Now)
		return bad(tunnel.State{})
	}()
	if err == nil {
		t.Error("identity error must be returned")
	}
}
