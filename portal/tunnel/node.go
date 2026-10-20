package tunnel

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
)

const (
	nodeConnected    = "connected"
	nodeDisconnected = "disconnected"
	tokenReviewWait  = 10 * time.Second
	// maxNodeSession bounds a session whose token expiry is unknown: the
	// bundle's projected token lives an hour.
	maxNodeSession = time.Hour
)

func (h *Hub) serveNode(ctx context.Context, c *conn, regID string, reg *protocol.Register) {
	if !h.regByIP.allow(c.ip, h.cfg.Now()) {
		h.refuse(ctx, c, regID, protocol.CodeUnavailable, "too many node registrations from this address", "cluster", reg.ClusterID, "node", reg.NodeName)
		return
	}
	st := h.cfg.Store
	cl, err := st.GetCluster(ctx, reg.ClusterID)
	if errors.Is(err, store.ErrNotFound) {
		h.refuse(ctx, c, regID, protocol.CodeUnauthorized, "unknown cluster", "cluster", reg.ClusterID, "node", reg.NodeName)
		return
	}
	if err != nil {
		h.refuse(ctx, c, regID, protocol.CodeUnavailable, "get cluster: "+err.Error(), "cluster", reg.ClusterID)
		return
	}

	if !h.regByCluster.allow(cl.ID, h.cfg.Now()) {
		h.refuse(ctx, c, regID, protocol.CodeUnavailable, "too many node registrations for this cluster", "cluster", cl.ID, "node", reg.NodeName)
		return
	}

	// The cluster agent checks the token with the Kubernetes TokenReview API.
	rctx, cancel := context.WithTimeout(ctx, tokenReviewWait)
	resp, err := h.Request(rctx, cl.ID, &protocol.Request{
		Op:          protocol.OpTokenReview,
		TokenReview: &protocol.TokenReviewRequest{Token: reg.ServiceAccountToken, Audience: protocol.NodeTokenAudience},
	})
	cancel()
	if err != nil {
		h.refuse(ctx, c, regID, protocol.CodeUnavailable, "token review: "+err.Error(), "cluster", cl.ID, "node", reg.NodeName)
		return
	}
	if resp.Error != nil {
		h.refuse(ctx, c, regID, protocol.CodeUnavailable, "token review failed in the cluster: "+resp.Error.Message, "cluster", cl.ID, "node", reg.NodeName)
		return
	}
	if err := resp.TokenReview.AcceptNode(reg.NodeName); err != nil {
		h.refuse(ctx, c, regID, protocol.CodeUnauthorized, err.Error(), "cluster", cl.ID, "node", reg.NodeName)
		return
	}

	key := nodeKey{cl.ID, reg.NodeName}
	h.mu.Lock()
	old := h.node[key]
	h.node[key] = c
	h.mu.Unlock()
	if old != nil {
		old.close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	defer h.dropNode(key, c)

	// The session lasts as long as the token (review-km-44 L7): a stolen
	// token's session ends when the token does, and the node agent
	// reconnects with its refreshed one. An agent too old to report the
	// expiry gets the bundle's token lifetime.
	until := h.cfg.Now().Add(maxNodeSession)
	if exp := resp.TokenReview.ExpiresAt; exp != nil && exp.Before(until) {
		until = *exp
	}
	expire := time.AfterFunc(until.Sub(h.cfg.Now()), func() {
		h.cfg.Log.Info("node agent session ended: token expired", "cluster", cl.ID, "node", reg.NodeName)
		c.close(websocket.StatusPolicyViolation, "token expired; reconnect with the refreshed token")
	})
	defer expire.Stop()

	if err := c.send(ctx, regID, &protocol.RegisterAck{ClusterID: cl.ID, Status: protocol.StatusActive}); err != nil {
		return
	}
	h.seenNode(ctx, key, reg.AgentVersion)
	h.cfg.Log.Info("node agent connected", "cluster", cl.ID, "node", reg.NodeName, "client", c.ip, "agent_version", reg.AgentVersion)

	h.loop(ctx, c, func(env *protocol.Envelope, body protocol.Payload) *protocol.Error {
		switch m := body.(type) {
		case *protocol.Heartbeat:
			h.seenNode(ctx, key, reg.AgentVersion)
			return nil
		case *protocol.Metrics:
			if m.NodeName != reg.NodeName {
				return &protocol.Error{Code: protocol.CodeBadRequest, Message: "metrics.nodeName differs from the registered node"}
			}
			if h.cfg.Metrics != nil {
				h.cfg.Metrics.Observe(cl.ID, m)
			}
			h.seenNode(ctx, key, reg.AgentVersion)
			return nil
		case *protocol.Error:
			h.cfg.Log.Warn("error from node agent", "cluster", cl.ID, "node", reg.NodeName, "code", m.Code, "message", m.Message)
			return nil
		case *protocol.Register:
			return &protocol.Error{Code: protocol.CodeBadRequest, Message: "already registered"}
		default:
			return &protocol.Error{Code: protocol.CodeUnsupported, Message: string(env.Type) + " is not handled on a node agent connection"}
		}
	})
}

func (h *Hub) seenNode(ctx context.Context, k nodeKey, agentVersion string) {
	if err := h.cfg.Store.UpsertNode(ctx, store.Node{ClusterID: k.cluster, Name: k.node, Status: nodeConnected, LastSeen: h.cfg.Now(), AgentVersion: agentVersion}); err != nil {
		h.cfg.Log.Error("upsert node", "cluster", k.cluster, "node", k.node, "err", err)
	}
}

func (h *Hub) dropNode(k nodeKey, c *conn) {
	c.close(websocket.StatusNormalClosure, "")
	h.mu.Lock()
	current := h.node[k] == c
	if current {
		delete(h.node, k)
	}
	h.mu.Unlock()
	if !current {
		return
	}
	// Keep last_seen: it tells when the node was last heard from.
	if err := h.cfg.Store.SetNodeStatus(context.Background(), k.cluster, k.node, nodeDisconnected); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.cfg.Log.Error("mark node disconnected", "cluster", k.cluster, "node", k.node, "err", err)
	}
	h.cfg.Log.Info("node agent disconnected", "cluster", k.cluster, "node", k.node)
}
