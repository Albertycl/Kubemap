// Package tunnel connects the server to the agents (KM-18).
//
// The cluster agent runs inside the server process (AttachLocal, local.go):
// Request, OpenStream, Summary and Connected work as they did for a remote
// agent, with no transport between them.
//
// Node agents dial in to GET /v1/connect over a WebSocket (protocol v1,
// pkg/protocol) on the cluster-internal Service. The first message must be
// register with the node's projected ServiceAccount token; the server has
// the cluster agent check it with a TokenReview and applies
// protocol.TokenReviewResult.AcceptNode (A8). Then the node agent sends
// metrics. Cluster agents never register over the network: a register with
// role cluster is refused.
//
// Both sides send heartbeats every protocol.HeartbeatInterval. A connection
// with no message for protocol.DisconnectAfter is closed and the node is
// marked disconnected. Tokens are never logged.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

// MetricsSink receives node metrics (KM-30).
type MetricsSink interface {
	Observe(clusterID string, m *protocol.Metrics)
}

// Config configures the Hub.
type Config struct {
	Store store.Store
	Log   *slog.Logger
	// Metrics receives node metrics; nil drops them.
	Metrics MetricsSink

	// Timings; zero means the protocol default. Tests shorten them.
	HeartbeatInterval time.Duration // default protocol.HeartbeatInterval
	DisconnectAfter   time.Duration // default protocol.DisconnectAfter
	RegisterTimeout   time.Duration // default 10 s
	RequestTimeout    time.Duration // default 30 s, when the caller's context has no deadline
	Now               func() time.Time
}

// Hub tracks the in-process cluster agent and the connected node agents.
type Hub struct {
	cfg  Config
	seq  atomic.Uint64
	mu   sync.Mutex
	clus map[string]*clusterConn // by cluster ID
	node map[nodeKey]*conn
	sum  map[string]summaryAt // latest summary per cluster

	// Node registration limits, per client address and per cluster
	// (review-km-44 L6).
	regByIP, regByCluster *limiter

	// conns counts running connection handlers, so Close can wait until
	// they have recorded the disconnect (KM-79 F4).
	conns sync.WaitGroup
}

type nodeKey struct{ cluster, node string }

type summaryAt struct {
	s  *protocol.Summary
	at time.Time
}

type clusterConn struct {
	*conn
	id string

	mu      sync.Mutex
	waiters map[string]chan *protocol.Response
	streams map[string]*Stream // open streams by stream id
}

// New returns a Hub.
func New(cfg Config) *Hub {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = protocol.HeartbeatInterval
	}
	if cfg.DisconnectAfter == 0 {
		cfg.DisconnectAfter = protocol.DisconnectAfter
	}
	if cfg.RegisterTimeout == 0 {
		cfg.RegisterTimeout = 10 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Hub{cfg: cfg, clus: map[string]*clusterConn{}, node: map[nodeKey]*conn{}, sum: map[string]summaryAt{},
		regByIP: newLimiter(nodeRegPerIPBurst, nodeRegPerIPEvery), regByCluster: newLimiter(nodeRegPerClusterBurst, nodeRegPerClusterEvery)}
}

// Routes registers GET /v1/connect.
func (h *Hub) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connect", h.serve)
}

// ErrNotConnected means the cluster agent is not attached.
var ErrNotConnected = errors.New("tunnel: cluster agent not connected")

// Connected reports whether the cluster agent holds a connection.
func (h *Hub) Connected(clusterID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clus[clusterID] != nil
}

// NodeConnected reports whether a node agent holds a connection.
func (h *Hub) NodeConnected(clusterID, node string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.node[nodeKey{clusterID, node}] != nil
}

// Summary returns the latest summary from the cluster agent, if any.
func (h *Hub) Summary(clusterID string) (*protocol.Summary, time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sum[clusterID]
	return s.s, s.at, ok
}

// Close ends every agent connection with "going away" (agents reconnect to
// the next portal process) and waits, up to wait, until every connection
// handler has finished, so their last store updates (cluster and node
// disconnected) are done before the portal closes the database. It reports
// whether they all finished in time. Call it when the portal shuts down.
func (h *Hub) Close(wait time.Duration) bool {
	h.mu.Lock()
	var all []*conn
	for _, c := range h.clus {
		all = append(all, c.conn)
	}
	for _, n := range h.node {
		all = append(all, n)
	}
	h.mu.Unlock()
	for _, c := range all {
		c.close(websocket.StatusGoingAway, "portal shutting down")
	}
	done := make(chan struct{})
	go func() {
		h.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

// Request sends a request to the cluster agent and waits for its response.
// A cluster with no agent attached gives ErrNotConnected.
func (h *Hub) Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error) {
	h.mu.Lock()
	c := h.clus[clusterID]
	h.mu.Unlock()
	if c == nil {
		return nil, ErrNotConnected
	}
	c.mu.Lock()
	id := "q-" + strconv.FormatUint(h.seq.Add(1), 10)
	ch := make(chan *protocol.Response, 1)
	c.waiters[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.cfg.RequestTimeout)
		defer cancel()
	}
	if err := c.send(ctx, id, req); err != nil {
		return nil, fmt.Errorf("tunnel: send request: %w", err)
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-c.closed:
		return nil, ErrNotConnected
	case <-ctx.Done():
		return nil, fmt.Errorf("tunnel: request %s: %w", req.Op, ctx.Err())
	}
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	h.conns.Add(1)
	defer h.conns.Done()                   // last: after the handlers' own deferred cleanup
	ws, err := websocket.Accept(w, r, nil) // default: refuses cross-origin browsers
	if err != nil {
		return
	}
	ws.SetReadLimit(protocol.MaxMessageSize)
	c := newConn(ws, httpjson.ClientIP(r))
	defer c.close(websocket.StatusNormalClosure, "")
	ctx := r.Context()

	env, body, err := c.read(ctx, h.cfg.RegisterTimeout)
	if err != nil {
		if !isTransport(err) {
			c.sendError(ctx, idOf(env), protocol.AsError(err))
		}
		h.cfg.Log.Warn("agent connection refused", "client", c.ip, "reason", "no valid register message", "err", err)
		return
	}
	reg, ok := body.(*protocol.Register)
	if !ok {
		c.sendError(ctx, env.ID, &protocol.Error{Code: protocol.CodeBadRequest, Message: "the first message must be register"})
		h.cfg.Log.Warn("agent connection refused", "client", c.ip, "reason", "first message was "+string(env.Type))
		return
	}
	// Versions are stored per cluster and node and shown on pages: keep
	// them short and plain (review km-43 N-2).
	reg.AgentVersion = versionText(reg.AgentVersion)
	h.serveNode(ctx, c, env.ID, reg)
}

func idOf(env *protocol.Envelope) string {
	if env == nil {
		return ""
	}
	return env.ID
}

// refuse answers a failed registration with one generic message, so an
// attacker learns nothing about which check failed; the log has the reason.
func (h *Hub) refuse(ctx context.Context, c *conn, id string, code protocol.ErrorCode, reason string, attrs ...any) {
	msg := "registration refused"
	if code == protocol.CodeUnavailable {
		msg = "registration not possible right now; retry later"
	}
	c.sendError(ctx, id, &protocol.Error{Code: code, Message: msg})
	h.cfg.Log.Warn("agent registration refused", append([]any{"client", c.ip, "reason", reason}, attrs...)...)
	c.close(websocket.StatusPolicyViolation, msg)
}

// loop reads messages until the connection ends or goes quiet for
// DisconnectAfter. handle returns an error to send back for one message.
func (h *Hub) loop(ctx context.Context, c *conn, handle func(env *protocol.Envelope, body protocol.Payload) *protocol.Error) {
	go c.heartbeat(ctx, h.cfg.HeartbeatInterval, h.cfg.Now)
	for {
		env, body, err := c.read(ctx, h.cfg.DisconnectAfter)
		if err != nil {
			if isTransport(err) {
				if errors.Is(err, context.DeadlineExceeded) {
					c.close(websocket.StatusPolicyViolation, "no heartbeat")
				}
				return
			}
			// A bad or unknown message is answered, never fatal.
			c.sendError(ctx, idOf(env), protocol.AsError(err))
			continue
		}
		if e := handle(env, body); e != nil {
			c.sendError(ctx, env.ID, e)
		}
	}
}

// maxVersionLen bounds a version an agent reports ("0.1.3",
// "v1.34.8+k3s1", "f52b4b7-dirty").
const maxVersionLen = 64

// versionText keeps at most maxVersionLen characters of a reported
// version and replaces anything outside [0-9A-Za-z.+_-] with "?", so a
// connected agent cannot store a long or odd string for every page that
// shows it (review km-43 N-2).
func versionText(v string) string {
	if len(v) > maxVersionLen {
		v = v[:maxVersionLen]
	}
	b := []byte(v)
	for i, c := range b {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '+', c == '_', c == '-':
		default:
			b[i] = '?'
		}
	}
	return string(b)
}
