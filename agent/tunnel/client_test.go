package tunnel

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
)

// fakeServer runs script for every agent connection (plain ws, like the
// cluster-internal Service).
type fakeServer struct {
	srv   *httptest.Server
	conns atomic.Int32
}

func startServer(t *testing.T, script func(t *testing.T, ws *websocket.Conn, n int)) *fakeServer {
	t.Helper()
	p := &fakeServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/connect" {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(protocol.MaxMessageSize)
		script(t, ws, int(p.conns.Add(1)))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func read(t *testing.T, ctx context.Context, ws *websocket.Conn) (*protocol.Envelope, protocol.Payload) {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return nil, nil
	}
	env, body, err := protocol.Decode(data)
	if err != nil {
		t.Errorf("server got a bad message: %v", err)
	}
	return env, body
}

func write(ctx context.Context, ws *websocket.Conn, id string, p protocol.Payload) {
	b, err := protocol.Encode(id, p)
	if err != nil {
		panic(err)
	}
	_ = ws.Write(ctx, websocket.MessageText, b)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)) }

type captureLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func nodeRegistrar(node, token string) Registrar {
	return func(State) (*protocol.Register, error) {
		return &protocol.Register{Role: protocol.RoleNode, AgentVersion: "v", ClusterID: "local", NodeName: node, ServiceAccountToken: token}, nil
	}
}

func baseConfig(p *fakeServer, reg Registrar) Config {
	return Config{
		PortalURL: p.srv.URL, Register: reg, Log: quietLog(),
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		RegisterTimeout: time.Second, Heartbeat: 50 * time.Millisecond, DisconnectAfter: 300 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ackAndDrain(t *testing.T, ws *websocket.Conn) {
	ctx := context.Background()
	env, _ := read(t, ctx, ws)
	write(ctx, ws, env.ID, &protocol.RegisterAck{ClusterID: "local", Status: protocol.StatusActive})
	for {
		if e, _ := read(t, ctx, ws); e == nil {
			return
		}
	}
}

// A node agent registers with its token, becomes active, sends metrics on
// the active context, and keeps heartbeating.
func TestNodeRegistrationFlow(t *testing.T) {
	var gotReg atomic.Pointer[protocol.Register]
	var beats atomic.Int32
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) {
		ctx := context.Background()
		env, body := read(t, ctx, ws)
		reg := body.(*protocol.Register)
		gotReg.Store(reg)
		write(ctx, ws, env.ID, &protocol.RegisterAck{ClusterID: "local", Status: protocol.StatusActive})
		for {
			e, b := read(t, ctx, ws)
			if e == nil {
				return
			}
			if _, ok := b.(*protocol.Heartbeat); ok {
				beats.Add(1)
			}
		}
	})
	active := make(chan struct{}, 1)
	cfg := baseConfig(p, nodeRegistrar("node-b", "satok_example"))
	cfg.Handlers.OnActive = func(ctx context.Context, s Sender) { active <- struct{}{} }
	c, err := New(cfg, State{ClusterID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	select {
	case <-active:
	case <-time.After(5 * time.Second):
		t.Fatal("never became active")
	}
	if c.Status() != StatusActive || c.State().ClusterID != "local" {
		t.Errorf("status %q state %+v", c.Status(), c.State())
	}
	if r := gotReg.Load(); r.NodeName != "node-b" || r.ClusterID != "local" || r.ServiceAccountToken != "satok_example" {
		t.Errorf("register = %+v", r)
	}
	waitFor(t, "heartbeats", func() bool { return beats.Load() >= 2 })
}

func TestRefusedRegistrationBacksOff(t *testing.T) {
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) {
		env, _ := read(t, context.Background(), ws)
		write(context.Background(), ws, env.ID, &protocol.Error{Code: protocol.CodeUnauthorized, Message: "registration refused"})
	})
	logs := &captureLog{}
	cfg := baseConfig(p, nodeRegistrar("n", "tok_secret_value"))
	cfg.Log = slog.New(slog.NewTextHandler(logs, nil))
	cfg.MaxBackoff = 200 * time.Millisecond
	c, _ := New(cfg, State{})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if n := p.conns.Load(); n < 1 || n > 5 {
		t.Errorf("%d attempts in 600 ms", n)
	}
	out := logs.String()
	if !strings.Contains(out, "server refused registration") || strings.Contains(out, "tok_secret_value") {
		t.Errorf("log = %s", out)
	}
}

func TestUnavailableRetriesSoon(t *testing.T) {
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) {
		env, _ := read(t, context.Background(), ws)
		write(context.Background(), ws, env.ID, &protocol.Error{Code: protocol.CodeUnavailable, Message: "cluster agent not connected"})
	})
	cfg := baseConfig(p, nodeRegistrar("n", "tok"))
	cfg.MaxBackoff = time.Minute
	c, _ := New(cfg, State{})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if n := p.conns.Load(); n < 3 {
		t.Errorf("%d attempts in 600 ms; unavailable must not wait the maximum backoff", n)
	}
}

func TestSilentServerReconnects(t *testing.T) {
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) { ackAndDrain(t, ws) })
	c, _ := New(baseConfig(p, nodeRegistrar("n", "tok")), State{})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	waitFor(t, "a second connection after silence", func() bool { return p.conns.Load() >= 2 })
	cancel()
}

func TestNoAckTimesOut(t *testing.T) {
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) {
		read(t, context.Background(), ws)
		time.Sleep(2 * time.Second)
	})
	cfg := baseConfig(p, nodeRegistrar("n", "tok"))
	cfg.RegisterTimeout = 100 * time.Millisecond
	cfg.DisconnectAfter = 10 * time.Second
	c, _ := New(cfg, State{})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	waitFor(t, "a retry after the ack timeout", func() bool { return p.conns.Load() >= 2 })
	cancel()
}

func TestNewValidation(t *testing.T) {
	reg := nodeRegistrar("n", "t")
	for i, cfg := range []Config{
		{PortalURL: "ws://p:8080"},            // no registrar
		{PortalURL: "::::", Register: reg},    // not a URL
		{PortalURL: "ftp://p", Register: reg}, // scheme
		{PortalURL: "ws://", Register: reg},   // no host
	} {
		if _, err := New(cfg, State{}); err == nil {
			t.Errorf("case %d: accepted", i)
		}
	}
	for _, u := range []string{"ws://kubemap.kubemap-system.svc:8080", "http://p:8080", "https://p", "wss://p"} {
		if _, err := New(Config{PortalURL: u, Register: reg}, State{}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	c, _ := New(Config{PortalURL: "http://p:8080", Register: reg}, State{})
	if c.url != "ws://p:8080/v1/connect" {
		t.Errorf("url = %s", c.url)
	}
}

func TestBackoff(t *testing.T) {
	for attempt, max := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 3: 8 * time.Second, 30: time.Minute} {
		d := backoff(attempt, time.Second, time.Minute)
		if d < max/2 || d > max {
			t.Errorf("backoff(%d) = %v, want in [%v, %v]", attempt, d, max/2, max)
		}
	}
}

func TestSendLanes(t *testing.T) {
	c := &conn{ctx: context.Background(), prio: make(chan []byte, 4), bulk: make(chan []byte, 4)}
	c.Send("", &protocol.Heartbeat{Time: time.Now()})
	c.Send("q", &protocol.Response{})
	c.Send("", &protocol.Metrics{NodeName: "n", Time: time.Now()})
	if len(c.prio) != 2 || len(c.bulk) != 1 {
		t.Errorf("prio %d bulk %d", len(c.prio), len(c.bulk))
	}
	if err := c.Send("", &protocol.Heartbeat{}); err == nil {
		t.Error("invalid message accepted")
	}
}

// A node agent renews its connection before its token expires.
func TestSessionRenewal(t *testing.T) {
	p := startServer(t, func(t *testing.T, ws *websocket.Conn, _ int) { ackAndDrain(t, ws) })
	var registers atomic.Int32
	cfg := baseConfig(p, func(State) (*protocol.Register, error) {
		registers.Add(1)
		return nodeRegistrar("n", "satok_example")(State{})
	})
	cfg.MinBackoff = time.Hour
	cfg.DisconnectAfter = time.Minute
	cfg.SessionUntil = func() time.Time { return time.Now().Add(-time.Minute) }
	c, _ := New(cfg, State{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !(p.conns.Load() >= 3 && registers.Load() >= 3) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for three renewed sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := p.conns.Load()
	time.Sleep(1500 * time.Millisecond)
	if extra := p.conns.Load() - n; extra > 2 {
		t.Errorf("%d reconnects: renewal loop", extra)
	}
}
