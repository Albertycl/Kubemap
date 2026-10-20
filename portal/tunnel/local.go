package tunnel

import (
	"context"
	"errors"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

// Local is the cluster agent that runs inside the server process. It is the
// same logic the old remote cluster agent ran (query handler, log streams),
// called directly instead of through a WebSocket.
type Local interface {
	// Handle answers one request. Its context ends when the caller gives up.
	Handle(ctx context.Context, req *protocol.Request) *protocol.Response
	// OpenStream starts a stream; it returns at once and sends stream.data
	// and stream.close through s until the stream ends.
	OpenStream(ctx context.Context, id string, o *protocol.StreamOpen, s Sender)
	CloseStream(id string)
	CloseStreams()
}

// Sender delivers a message from the local cluster agent to the hub. The
// summary pusher and the stream manager use it.
type Sender interface {
	Send(id string, p protocol.Payload) error
}

// AttachLocal makes the in-process cluster agent the connection of the
// cluster clusterID. After it, Request, OpenStream, Connected and Summary
// work for that cluster exactly as they did for a remote agent. The returned
// Sender takes the agent's summaries and stream data; detach marks the
// cluster disconnected (call it at shutdown). A cluster that already has a
// connection is replaced.
func (h *Hub) AttachLocal(ctx context.Context, clusterID, agentVersion string, l Local) (s Sender, detach func(), err error) {
	if _, err := h.cfg.Store.GetCluster(ctx, clusterID); err != nil {
		return nil, nil, err
	}
	if err := h.cfg.Store.MarkClusterSeen(ctx, clusterID, agentVersion); err != nil {
		return nil, nil, err
	}
	c := &conn{closed: make(chan struct{})}
	cc := &clusterConn{conn: c, id: clusterID, waiters: map[string]chan *protocol.Response{}, streams: map[string]*Stream{}}
	port := &localPort{h: h, cc: cc, l: l, ctx: ctx}
	c.local = port.deliver

	h.mu.Lock()
	old := h.clus[clusterID]
	h.clus[clusterID] = cc
	h.mu.Unlock()
	if old != nil {
		old.close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	h.conns.Add(1)
	go func() { // the one place a local connection is torn down
		defer h.conns.Done()
		select {
		case <-ctx.Done():
			c.close(websocket.StatusGoingAway, "server stopping")
		case <-c.closed:
		}
		l.CloseStreams()
		h.dropCluster(cc)
	}()
	go c.heartbeatLocal(ctx, h.cfg.HeartbeatInterval, func() {
		if err := h.cfg.Store.MarkClusterSeen(ctx, clusterID, ""); err != nil && !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			h.cfg.Log.Error("mark cluster seen", "cluster", clusterID, "err", err)
		}
	})
	h.cfg.Log.Info("cluster agent attached in process", "cluster", clusterID, "agent_version", agentVersion)
	return port, func() { c.close(websocket.StatusNormalClosure, "") }, nil
}

// localPort carries messages both ways between the hub and the local agent.
type localPort struct {
	h   *Hub
	cc  *clusterConn
	l   Local
	ctx context.Context // the server's lifetime; streams live as long as it
}

// deliver is what the hub's send does for the local cluster: it hands the
// message to the agent logic instead of writing it to a WebSocket.
func (p *localPort) deliver(ctx context.Context, id string, body protocol.Payload) error {
	switch m := body.(type) {
	case *protocol.Request:
		go func() {
			resp := p.l.Handle(ctx, m)
			p.Send(id, resp)
		}()
	case *protocol.StreamOpen:
		p.l.OpenStream(p.ctx, id, m, p)
	case *protocol.StreamClose:
		p.l.CloseStream(id)
	}
	return nil // heartbeats and acks need no answer in process
}

// Send is the agent-to-hub direction: summary, response, stream data.
func (p *localPort) Send(id string, body protocol.Payload) error {
	select {
	case <-p.cc.closed:
		return ErrNotConnected
	default:
	}
	if e := p.h.clusterMessage(p.ctx, p.cc, &protocol.Envelope{ID: id}, body); e != nil {
		return errors.New(e.Message)
	}
	return nil
}

func (cc *clusterConn) stream(id string) *Stream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.streams[id]
}

func (h *Hub) clusterMessage(ctx context.Context, cc *clusterConn, env *protocol.Envelope, body protocol.Payload) *protocol.Error {
	switch m := body.(type) {
	case *protocol.Heartbeat:
		if err := h.cfg.Store.MarkClusterSeen(ctx, cc.id, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
			h.cfg.Log.Error("mark cluster seen", "cluster", cc.id, "err", err)
		}
		return nil
	case *protocol.Summary:
		h.mu.Lock()
		h.sum[cc.id] = summaryAt{m, h.cfg.Now()}
		h.mu.Unlock()
		h.cfg.Store.MarkClusterSeen(ctx, cc.id, "")
		return nil
	case *protocol.Response:
		cc.mu.Lock()
		ch := cc.waiters[env.ID]
		cc.mu.Unlock()
		if ch == nil {
			return nil // the caller gave up; the answer is dropped
		}
		select {
		case ch <- m:
		default: // a duplicate response; the first one wins
		}
		return nil
	case *protocol.StreamData:
		if s := cc.stream(env.ID); s != nil {
			s.data(m)
		}
		return nil
	case *protocol.StreamClose:
		if s := cc.stream(env.ID); s != nil {
			s.end(m)
		}
		return nil
	case *protocol.Error:
		h.cfg.Log.Warn("error from the local cluster agent", "cluster", cc.id, "id", env.ID, "code", m.Code, "message", m.Message)
		return nil
	default:
		return &protocol.Error{Code: protocol.CodeUnsupported, Message: string(env.Type) + " is not handled on the cluster connection"}
	}
}

// dropCluster forgets a closed connection (unless a newer one replaced it)
// and marks the cluster disconnected.
func (h *Hub) dropCluster(cc *clusterConn) {
	cc.close(websocket.StatusNormalClosure, "")
	cc.failAll()
	h.mu.Lock()
	current := h.clus[cc.id] == cc
	if current {
		delete(h.clus, cc.id)
	}
	h.mu.Unlock()
	if !current {
		return
	}
	err := h.cfg.Store.MarkClusterDisconnected(context.Background(), cc.id)
	if err != nil && !errors.Is(err, store.ErrInvalidState) && !errors.Is(err, store.ErrNotFound) {
		h.cfg.Log.Error("mark cluster disconnected", "cluster", cc.id, "err", err)
	}
	h.cfg.Log.Info("cluster agent disconnected", "cluster", cc.id)
}
