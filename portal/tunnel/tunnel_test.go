package tunnel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fmt"
	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"sync/atomic"
)

type env struct {
	t    *testing.T
	st   *store.SQLite
	hub  *Hub
	url  string
	logs *syncBuffer
	sink *fakeSink
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeSink struct {
	mu  sync.Mutex
	got []*protocol.Metrics
}

func (f *fakeSink) Observe(_ string, m *protocol.Metrics) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, m)
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	sink := &fakeSink{}
	cfg.Store, cfg.Log, cfg.Metrics = st, slog.New(slog.NewTextHandler(logs, nil)), sink
	if cfg.DisconnectAfter == 0 {
		cfg.DisconnectAfter = 5 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour // tests send their own
	}
	hub := New(cfg)
	mux := http.NewServeMux()
	hub.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		hub.Close(time.Second)
		srv.Close()
		st.Close()
	})
	return &env{t: t, st: st, hub: hub, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/connect", logs: logs, sink: sink}
}

// agent is a fake agent: one WebSocket with typed send and receive.
type agent struct {
	t  *testing.T
	ws *websocket.Conn
}

func (e *env) dial() *agent {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, e.url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	ws.SetReadLimit(protocol.MaxMessageSize)
	e.t.Cleanup(func() { ws.CloseNow() })
	return &agent{t: e.t, ws: ws}
}

func (a *agent) send(id string, p protocol.Payload) {
	a.t.Helper()
	b, err := protocol.Encode(id, p)
	if err != nil {
		a.t.Fatal(err)
	}
	a.sendRaw(b)
}

func (a *agent) sendRaw(b []byte) {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.ws.Write(ctx, websocket.MessageText, b); err != nil {
		a.t.Fatal(err)
	}
}

// recv returns the next message that is not a heartbeat.
func (a *agent) recv() (*protocol.Envelope, protocol.Payload, error) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := a.ws.Read(ctx)
		cancel()
		if err != nil {
			return nil, nil, err
		}
		env, body, err := protocol.Decode(data)
		if err != nil {
			a.t.Fatalf("portal sent an invalid message: %v", err)
		}
		if env.Type != protocol.TypeHeartbeat {
			return env, body, nil
		}
	}
}

func (a *agent) expect(typ protocol.Type) (*protocol.Envelope, protocol.Payload) {
	a.t.Helper()
	env, body, err := a.recv()
	if err != nil {
		a.t.Fatalf("waiting for %s: %v", typ, err)
	}
	if env.Type != typ {
		a.t.Fatalf("got %s %+v, want %s", env.Type, body, typ)
	}
	return env, body
}

// expectClosed waits until the portal closes the connection.
func (a *agent) expectClosed() {
	a.t.Helper()
	for {
		env, _, err := a.recv()
		if err != nil {
			return
		}
		if env.Type != protocol.TypeError {
			a.t.Fatalf("got %s, want the connection to close", env.Type)
		}
	}
}

// fakeLocal is the in-process cluster agent in tests.
type fakeLocal struct {
	answer func(req *protocol.Request) *protocol.Response
	opened chan openedStream
	closed chan closedStream
}

type openedStream struct {
	id string
	o  *protocol.StreamOpen
	s  Sender
}

type closedStream struct{ id string }

func newFakeLocal(answer func(req *protocol.Request) *protocol.Response) *fakeLocal {
	return &fakeLocal{answer: answer, opened: make(chan openedStream, 8), closed: make(chan closedStream, 8)}
}

func (f *fakeLocal) Handle(_ context.Context, req *protocol.Request) *protocol.Response {
	return f.answer(req)
}
func (f *fakeLocal) OpenStream(_ context.Context, id string, o *protocol.StreamOpen, s Sender) {
	f.opened <- openedStream{id, o, s}
}
func (f *fakeLocal) CloseStream(id string) { f.closed <- closedStream{id} }
func (f *fakeLocal) CloseStreams()         {}

// attach creates the local cluster and attaches l to it.
func (e *env) attach(l Local) (store.Cluster, Sender, func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	c, err := e.st.EnsureCluster(ctx, "local", "this cluster")
	if err != nil {
		e.t.Fatal(err)
	}
	s, detach, err := e.hub.AttachLocal(ctx, c.ID, "v0.1.0", l)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, s, detach
}

func (e *env) status(id string) string {
	e.t.Helper()
	c, err := e.st.GetCluster(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c.Status
}

// waitFor polls cond for up to 3 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) activeCluster(answer func(req *protocol.Request) *protocol.Response) (store.Cluster, *fakeLocal) {
	e.t.Helper()
	l := newFakeLocal(answer)
	c, _, _ := e.attach(l)
	return c, l
}

func TestAttachLocalMarksClusterActiveAndConnected(t *testing.T) {
	e := newEnv(t, Config{})
	c, _, detach := e.attach(newFakeLocal(nil))
	if !e.hub.Connected(c.ID) || e.status(c.ID) != store.StatusActive {
		t.Fatalf("connected %v status %s", e.hub.Connected(c.ID), e.status(c.ID))
	}
	got, _ := e.st.GetCluster(context.Background(), c.ID)
	if got.AgentVersion != "v0.1.0" {
		t.Fatalf("agent version %q", got.AgentVersion)
	}
	detach()
	waitFor(t, "disconnected", func() bool { return e.status(c.ID) == store.StatusDisconnected && !e.hub.Connected(c.ID) })
}

func TestAttachLocalUnknownCluster(t *testing.T) {
	e := newEnv(t, Config{})
	if _, _, err := e.hub.AttachLocal(context.Background(), "c-0000000000000000", "v", newFakeLocal(nil)); err == nil {
		t.Fatal("attached to a cluster that does not exist")
	}
}

func TestLocalSummary(t *testing.T) {
	e := newEnv(t, Config{})
	c, s, _ := e.attach(newFakeLocal(nil))
	if _, _, ok := e.hub.Summary(c.ID); ok {
		t.Fatal("summary before any was sent")
	}
	if err := s.Send("", &protocol.Summary{Namespaces: 2}); err != nil {
		t.Fatal(err)
	}
	got, at, ok := e.hub.Summary(c.ID)
	if !ok || got.Namespaces != 2 || at.IsZero() {
		t.Fatalf("summary %+v %v %v", got, at, ok)
	}
}

func TestRequestResponse(t *testing.T) {
	e := newEnv(t, Config{RequestTimeout: 300 * time.Millisecond})
	c, _ := e.activeCluster(func(req *protocol.Request) *protocol.Response {
		if req.Resource == "slow" {
			time.Sleep(time.Second)
		}
		return &protocol.Response{Object: []byte(`{"kind":"Pod"}`)}
	})
	resp, err := e.hub.Request(context.Background(), c.ID, &protocol.Request{Op: protocol.OpGet, Version: "v1", Resource: "pods", Namespace: "default", Name: "x"})
	if err != nil || !bytes.Contains(resp.Object, []byte("Pod")) {
		t.Fatalf("request: %v %+v", err, resp)
	}
	if _, err := e.hub.Request(context.Background(), c.ID, &protocol.Request{Op: protocol.OpGet, Version: "v1", Resource: "slow", Namespace: "default", Name: "x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow request: %v", err)
	}
	if _, err := e.hub.Request(context.Background(), "c-0000000000000000", &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("unknown cluster: %v", err)
	}
}

func TestRequestAfterDetach(t *testing.T) {
	e := newEnv(t, Config{})
	c, _, detach := e.attach(newFakeLocal(func(*protocol.Request) *protocol.Response { return &protocol.Response{} }))
	detach()
	waitFor(t, "detached", func() bool { return !e.hub.Connected(c.ID) })
	if _, err := e.hub.Request(context.Background(), c.ID, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after detach: %v", err)
	}
}

func TestCloseWaitsForHandlers(t *testing.T) {
	e := newEnv(t, Config{})
	c, _ := e.activeCluster(nil)
	if !e.hub.Close(3 * time.Second) {
		t.Fatal("Close did not finish in time")
	}
	if got := e.status(c.ID); got != store.StatusDisconnected || e.hub.Connected(c.ID) {
		t.Fatalf("after Close: %s connected=%v", got, e.hub.Connected(c.ID))
	}
}

func TestClusterAgentCannotRegisterOverTheNetwork(t *testing.T) {
	e := newEnv(t, Config{})
	a := e.dial()
	a.sendRaw([]byte(`{"v":1,"type":"register","id":"reg-1","payload":{"role":"cluster","agentVersion":"v0.1.0","clusterId":"local","nodeName":"n","serviceAccountToken":"t"}}`))
	if _, body := a.expect(protocol.TypeError); body.(*protocol.Error).Code != protocol.CodeBadRequest {
		t.Fatalf("cluster register: %+v", body)
	}
	a.expectClosed()
}

func TestUnknownTypeIsAnsweredNotFatal(t *testing.T) {
	e := newEnv(t, Config{})
	c, _ := e.activeCluster(goodReview)
	a := e.dial()
	a.send("reg-n", nodeReg(c.ID, "node-a"))
	a.expect(protocol.TypeRegisterAck)
	a.sendRaw([]byte(`{"v":1,"type":"future.thing","id":"x-9","payload":{}}`))
	env, body := a.expect(protocol.TypeError)
	if env.ID != "x-9" || body.(*protocol.Error).Code != protocol.CodeUnsupported {
		t.Fatalf("unknown type: %s %+v", env.ID, body)
	}
	a.send("", &protocol.Heartbeat{Time: time.Now()}) // still open
	a.sendRaw([]byte(`{"v":1,"type":"future.thing","id":"x-10","payload":{}}`))
	a.expect(protocol.TypeError)
}

func TestNodeHeartbeatTimeout(t *testing.T) {
	e := newEnv(t, Config{DisconnectAfter: 300 * time.Millisecond})
	c, _ := e.activeCluster(goodReview)
	a := e.dial()
	a.send("reg-n", nodeReg(c.ID, "node-a"))
	a.expect(protocol.TypeRegisterAck)
	a.expectClosed()
	waitFor(t, "node dropped", func() bool { return !e.hub.NodeConnected(c.ID, "node-a") })
}

func TestPortalSendsHeartbeatsToNodes(t *testing.T) {
	e := newEnv(t, Config{HeartbeatInterval: 50 * time.Millisecond})
	c, _ := e.activeCluster(goodReview)
	a := e.dial()
	a.send("reg-n", nodeReg(c.ID, "node-a"))
	a.expect(protocol.TypeRegisterAck)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := a.ws.Read(ctx)
	if err != nil || !bytes.Contains(data, []byte(`"type":"heartbeat"`)) {
		t.Fatalf("no heartbeat from the portal: %v %s", err, data)
	}
}

func nodeReg(clusterID, node string) *protocol.Register {
	return &protocol.Register{Role: protocol.RoleNode, AgentVersion: "v0.1.0", ClusterID: clusterID, NodeName: node, ServiceAccountToken: "sa-token-" + node}
}

func goodReview(req *protocol.Request) *protocol.Response {
	node := strings.TrimPrefix(req.TokenReview.Token, "sa-token-")
	return &protocol.Response{TokenReview: &protocol.TokenReviewResult{
		Authenticated: true, Audiences: []string{protocol.NodeTokenAudience},
		Username: protocol.NodeAgentUsername, NodeName: node, PodName: "kubemap-node-agent-x",
	}}
}

func TestNodeAgent(t *testing.T) {
	e := newEnv(t, Config{})
	var mu sync.Mutex
	var reviewed []string
	c, _ := e.activeCluster(func(req *protocol.Request) *protocol.Response {
		mu.Lock()
		reviewed = append(reviewed, req.TokenReview.Audience)
		mu.Unlock()
		return goodReview(req)
	})
	n := e.dial()
	n.send("reg-n", nodeReg(c.ID, "node-a"))
	_, body := n.expect(protocol.TypeRegisterAck)
	if body.(*protocol.RegisterAck).Status != protocol.StatusActive {
		t.Fatalf("node ack %+v", body)
	}
	mu.Lock()
	if len(reviewed) != 1 || reviewed[0] != protocol.NodeTokenAudience {
		t.Fatalf("token reviews %v", reviewed)
	}
	mu.Unlock()
	if !e.hub.NodeConnected(c.ID, "node-a") {
		t.Fatal("node not tracked")
	}

	n.send("", &protocol.Metrics{Time: time.Now(), NodeName: "node-a", CPU: protocol.CPUStats{Cores: 4, UsagePercent: 12}})
	waitFor(t, "metrics", func() bool { return e.sink.count() == 1 })
	n.send("", &protocol.Metrics{Time: time.Now(), NodeName: "other-node"})
	if _, body := n.expect(protocol.TypeError); body.(*protocol.Error).Code != protocol.CodeBadRequest {
		t.Fatalf("metrics for another node: %+v", body)
	}
	if e.sink.count() != 1 {
		t.Fatal("metrics for another node were accepted")
	}

	// Disconnect: node marked disconnected.
	n.ws.CloseNow()
	waitFor(t, "node dropped", func() bool { return !e.hub.NodeConnected(c.ID, "node-a") })

	// Rejected tokens: wrong node binding, wrong audience, not authenticated.
	bad := map[string]*protocol.TokenReviewResult{
		"bound to another node": {Authenticated: true, Audiences: []string{protocol.NodeTokenAudience}, Username: protocol.NodeAgentUsername, NodeName: "elsewhere"},
		"no audience":           {Authenticated: true, Username: protocol.NodeAgentUsername, NodeName: "node-a"},
		"other account":         {Authenticated: true, Audiences: []string{protocol.NodeTokenAudience}, Username: "system:serviceaccount:default:default", NodeName: "node-a"},
		"not authenticated":     {Authenticated: false},
	}
	for name, res := range bad {
		t.Run(name, func(t *testing.T) {
			e2 := newEnv(t, Config{})
			c2, _ := e2.activeCluster(func(*protocol.Request) *protocol.Response { return &protocol.Response{TokenReview: res} })
			n := e2.dial()
			n.send("reg-n", nodeReg(c2.ID, "node-a"))
			_, body := n.expect(protocol.TypeError)
			if body.(*protocol.Error).Code != protocol.CodeUnauthorized {
				t.Fatalf("%s: %+v", name, body)
			}
			n.expectClosed()
		})
	}
}

func TestNodeAgentNeedsClusterAgent(t *testing.T) {
	e := newEnv(t, Config{})
	c, _, detach := e.attach(newFakeLocal(goodReview))
	m := e.dial()
	m.send("reg-n", nodeReg("c-0000000000000000", "node-a"))
	if _, body := m.expect(protocol.TypeError); body.(*protocol.Error).Code != protocol.CodeUnauthorized {
		t.Fatalf("unknown cluster: %+v", body)
	}
	detach()
	waitFor(t, "detached", func() bool { return !e.hub.Connected(c.ID) })
	o := e.dial()
	o.send("reg-n", nodeReg(c.ID, "node-a"))
	if _, body := o.expect(protocol.TypeError); body.(*protocol.Error).Code != protocol.CodeUnavailable {
		t.Fatalf("cluster agent not attached: %+v", body)
	}
}

// Several nodes of one cluster: each node agent is checked by its own
// TokenReview, tracked under its own name, and one leaving does not touch
// the others. A token bound to node-a cannot register as node-b.
func TestTwoNodeAgents(t *testing.T) {
	e := newEnv(t, Config{})
	c, _ := e.activeCluster(goodReview)
	a, b := e.dial(), e.dial()
	a.send("reg-a", nodeReg(c.ID, "node-a"))
	b.send("reg-b", nodeReg(c.ID, "node-b"))
	a.expect(protocol.TypeRegisterAck)
	b.expect(protocol.TypeRegisterAck)
	waitFor(t, "both tracked", func() bool { return e.hub.NodeConnected(c.ID, "node-a") && e.hub.NodeConnected(c.ID, "node-b") })
	var nodes []store.Node
	waitFor(t, "both stored", func() bool {
		nodes, _ = e.st.ListNodes(context.Background(), c.ID)
		return len(nodes) == 2
	})
	for _, n := range nodes {
		if n.Status != nodeConnected {
			t.Errorf("%s status %s", n.Name, n.Status)
		}
	}
	a.send("", &protocol.Metrics{Time: time.Now(), NodeName: "node-a"})
	b.send("", &protocol.Metrics{Time: time.Now(), NodeName: "node-b"})
	waitFor(t, "metrics from both", func() bool { return e.sink.count() == 2 })

	// node-a's agent leaves: node-b is still connected.
	a.ws.CloseNow()
	waitFor(t, "node-a dropped", func() bool { return !e.hub.NodeConnected(c.ID, "node-a") })
	if !e.hub.NodeConnected(c.ID, "node-b") {
		t.Fatal("node-b dropped with node-a")
	}
	waitFor(t, "node-a stored as disconnected", func() bool {
		nodes, _ = e.st.ListNodes(context.Background(), c.ID)
		status := map[string]string{}
		for _, n := range nodes {
			status[n.Name] = n.Status
		}
		return status["node-a"] == nodeDisconnected && status["node-b"] == nodeConnected
	})

	// node-a's token cannot register as node-b.
	x := e.dial()
	r := nodeReg(c.ID, "node-b")
	r.ServiceAccountToken = "sa-token-node-a"
	x.send("reg-x", r)
	if _, body := x.expect(protocol.TypeError); body.(*protocol.Error).Code != protocol.CodeUnauthorized {
		t.Fatalf("stolen token: %+v", body)
	}
}

func TestNodeSessionEndsAtTokenExpiry(t *testing.T) {
	e := newEnv(t, Config{})
	c, _ := e.activeCluster(func(req *protocol.Request) *protocol.Response {
		r := goodReview(req)
		exp := time.Now().Add(300 * time.Millisecond)
		r.TokenReview.ExpiresAt = &exp
		return r
	})
	n := e.dial()
	n.send("reg-n", nodeReg(c.ID, "node-a"))
	if _, body := n.expect(protocol.TypeRegisterAck); body.(*protocol.RegisterAck).Status != protocol.StatusActive {
		t.Fatalf("ack %+v", body)
	}
	// Read like a node agent does, so the close handshake completes.
	closed := make(chan websocket.StatusCode, 1)
	go func() {
		for {
			if _, _, err := n.ws.Read(context.Background()); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	waitFor(t, "session ended at token expiry", func() bool { return !e.hub.NodeConnected(c.ID, "node-a") })
	select {
	case code := <-closed:
		if code != websocket.StatusPolicyViolation {
			t.Errorf("close status %v", code)
		}
	case <-time.After(3 * time.Second):
		t.Error("the node agent was not told")
	}
}

// review-km-44 L6: a burst of node registrations from one address is
// refused before any token review is asked for.
func TestNodeRegistrationLimit(t *testing.T) {
	e := newEnv(t, Config{})
	var reviews atomic.Int32
	c, _ := e.activeCluster(func(req *protocol.Request) *protocol.Response {
		reviews.Add(1)
		return goodReview(req)
	})
	refused := 0
	for i := 0; i < nodeRegPerIPBurst+3; i++ {
		n := e.dial()
		n.send("reg-n", nodeReg(c.ID, fmt.Sprintf("n%d", i)))
		env, body, err := n.recv()
		if err == nil && env.Type == protocol.TypeError && body.(*protocol.Error).Code == protocol.CodeUnavailable {
			refused++
		}
		n.ws.CloseNow()
	}
	if refused != 3 || int(reviews.Load()) != nodeRegPerIPBurst {
		t.Errorf("refused %d (want 3), token reviews %d (want %d)", refused, reviews.Load(), nodeRegPerIPBurst)
	}
}
