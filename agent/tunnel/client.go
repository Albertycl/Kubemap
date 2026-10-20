// Package tunnel is the agent's side of the connection to the portal: it
// dials out (the server never connects in), registers, keeps the connection
// alive with heartbeats, reconnects with backoff, and hands the server's
// heartbeats and errors to the log. Each node agent runs one Client, and
// dials the server over the cluster-internal Service.
//
// The wire rules are in docs/protocol.md ("Rules for the transport"): 1 MiB
// read limit, no compression, register first, heartbeats ahead of bulk
// data, unknown types answered "unsupported" with the same id.
package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
)

// State is what the server has told the agent: its cluster ID.
type State struct {
	ClusterID string
}

// Registrar builds the register message for a new connection. It is called
// on every connect, so a node agent re-reads its rotated token each time.
type Registrar func(State) (*protocol.Register, error)

// Sender sends a message on the current connection. Bulk messages (summary,
// metrics, stream.data) wait behind heartbeats and responses.
type Sender interface {
	Send(id string, p protocol.Payload) error
}

// Handlers are what the agent does with a connection. Every field may be nil.
type Handlers struct {
	// OnActive runs while the agent is registered and connected (the
	// metrics pusher). Its context ends with the connection.
	OnActive func(ctx context.Context, s Sender)
	// OnDisconnect runs after each connection ends (stop streams).
	OnDisconnect func()
}

// Config configures a Client.
type Config struct {
	PortalURL string // "ws://kubemap.kubemap-system.svc:8080"; /v1/connect is added
	Register  Registrar
	Handlers  Handlers
	Log       *slog.Logger

	// Zero values take the protocol defaults.
	MinBackoff, MaxBackoff time.Duration
	RegisterTimeout        time.Duration // default 10 s
	Heartbeat              time.Duration // default protocol.HeartbeatInterval
	DisconnectAfter        time.Duration // default protocol.DisconnectAfter
	// SessionUntil, when set, is asked after each register: the connection
	// is closed at that time and a new one opened at once. A node agent uses
	// it to re-register before its projected token expires (rule 6).
	SessionUntil func() time.Time

	// TLS is used for wss:// and https:// URLs; nil takes the system roots.
	TLS *tls.Config
}

// Status of the client, for logs and health checks.
const (
	StatusConnecting   = "connecting"
	StatusActive       = "active"
	StatusDisconnected = "disconnected"
)

// Client is one agent's connection to the portal.
type Client struct {
	cfg    Config
	url    string
	http   *http.Client
	log    *slog.Logger
	mu     sync.Mutex
	state  State
	status string
	regSeq atomic.Uint64
}

// New checks the configuration and builds the HTTP client.
func New(cfg Config, initial State) (*Client, error) {
	if cfg.Register == nil {
		return nil, errors.New("tunnel: Register is required")
	}
	u, err := url.Parse(cfg.PortalURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("tunnel: portal URL %q is not a URL", cfg.PortalURL)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws" // inside the cluster; NetworkPolicy and a short-lived token protect it
	default:
		return nil, fmt.Errorf("tunnel: unsupported scheme %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/connect"
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg: cfg, url: u.String(), log: log, state: initial, status: StatusDisconnected,
		// No proxy: the node agent reaches the server directly.
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.TLS, ForceAttemptHTTP2: false}},
	}, nil
}

// Status returns the connection status.
func (c *Client) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// State returns what the portal has told the agent so far.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) setStatus(s string) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

func dur(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

// Run connects and reconnects until ctx ends. A connection that stayed up
// for a minute resets the backoff; a refused registration backs off to the
// maximum at once, since retrying quickly will not change the answer.
func (c *Client) Run(ctx context.Context) error {
	minB, maxB := dur(c.cfg.MinBackoff, time.Second), dur(c.cfg.MaxBackoff, time.Minute)
	attempt := 0
	for {
		start := time.Now()
		err := c.session(ctx)
		c.setStatus(StatusDisconnected)
		if h := c.cfg.Handlers.OnDisconnect; h != nil {
			h()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var refused *refusedError
		switch {
		case errors.Is(err, errRenew):
			attempt = 0
			c.log.Info("renewing the connection with a fresh token")
			continue // at once, no backoff
		case errors.As(err, &refused) && refused.code == protocol.CodeUnavailable:
			// "Retry later" (the portal is busy or waiting for the cluster
			// agent): normal backoff, not the refusal maximum.
			attempt++
			c.log.Warn("portal cannot register the agent yet", "reason", refused.msg, "attempt", attempt)
		case errors.As(err, &refused):
			attempt = 16
			c.log.Error("server refused registration; check the node agent token and CLUSTER_ID", "code", refused.code, "reason", refused.msg)
		case time.Since(start) > time.Minute:
			attempt = 0
			c.log.Warn("connection to portal lost", "err", err)
		default:
			attempt++
			c.log.Warn("cannot connect to portal", "err", err, "attempt", attempt)
		}
		wait := backoff(attempt, minB, maxB)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoff is exponential with full jitter in [d/2, d].
func backoff(attempt int, minB, maxB time.Duration) time.Duration {
	d := minB
	for i := 0; i < attempt && d < maxB; i++ {
		d *= 2
	}
	d = min(d, maxB)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// refusedError is a registration the portal answered with an error.
type refusedError struct {
	code protocol.ErrorCode
	msg  string
}

func (e *refusedError) Error() string {
	return fmt.Sprintf("registration refused (%s): %s", e.code, e.msg)
}

// minSession is the shortest connection a renewal allows.
const minSession = time.Second

// errRenew ends a connection on purpose, for SessionUntil.
var errRenew = errors.New("session renewal")

// errPortalSilent ends a connection without heartbeats for DisconnectAfter.
var errPortalSilent = errors.New("no message from the portal within the disconnect time")

// session runs one connection from dial to close.
func (c *Client) session(ctx context.Context) error {
	c.setStatus(StatusConnecting)
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, _, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{
		HTTPClient: c.http, CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		return err
	}
	ws.SetReadLimit(protocol.MaxMessageSize)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	conn := newConn(sctx, ws, stop, c.log)
	defer conn.close()

	reg, err := c.cfg.Register(c.State())
	if err != nil {
		return fmt.Errorf("build register: %w", err)
	}
	regID := fmt.Sprintf("reg-%d", c.regSeq.Add(1))
	if err := conn.sendPrio(regID, reg); err != nil {
		return err
	}
	ackTimer := time.NewTimer(dur(c.cfg.RegisterTimeout, 10*time.Second))
	defer ackTimer.Stop()
	heartbeat := time.NewTicker(dur(c.cfg.Heartbeat, protocol.HeartbeatInterval))
	defer heartbeat.Stop()
	silentAfter := dur(c.cfg.DisconnectAfter, protocol.DisconnectAfter)
	watchdog := time.NewTicker(silentAfter / 3)
	defer watchdog.Stop()
	// The renewal timer starts once the register is acknowledged, with a
	// floor, so a renewal can never end a session before it registered.
	var renew <-chan time.Time // nil: never
	var renewTimer *time.Timer
	defer func() {
		if renewTimer != nil {
			renewTimer.Stop()
		}
	}()
	startRenewal := func() {
		if c.cfg.SessionUntil != nil && renewTimer == nil {
			renewTimer = time.NewTimer(max(time.Until(c.cfg.SessionUntil()), minSession))
			renew = renewTimer.C
		}
	}
	lastSeen := time.Now()

	// The active context lives as long as the connection; OnActive gets it
	// once the cluster is accepted.
	actx, activeCancel := context.WithCancel(sctx)
	defer activeCancel()
	active := false
	registered := false
	for {
		select {
		case <-sctx.Done():
			if err := conn.err(); err != nil {
				return err
			}
			return sctx.Err()
		case <-ackTimer.C:
			if !registered {
				return errors.New("no register.ack within the register timeout")
			}
		case <-heartbeat.C:
			if err := conn.sendPrio("", &protocol.Heartbeat{Time: time.Now().UTC()}); err != nil {
				return err
			}
		case <-watchdog.C:
			if time.Since(lastSeen) > silentAfter {
				return errPortalSilent
			}
		case <-renew:
			return errRenew
		case in, ok := <-conn.in:
			if !ok {
				return conn.err()
			}
			lastSeen = time.Now()
			if in.err != nil {
				// A message we cannot use: answer with an error carrying
				// its id, and keep the connection (rule: never crash).
				if in.env != nil {
					if err := conn.sendPrio(in.env.ID, protocol.AsError(in.err)); err != nil {
						return err
					}
					continue
				}
				return in.err
			}
			switch body := in.body.(type) {
			case *protocol.RegisterAck:
				if in.env.ID != regID {
					continue
				}
				registered = true
				startRenewal()
				c.mu.Lock()
				c.state.ClusterID = body.ClusterID
				c.mu.Unlock()
				if body.Status == protocol.StatusActive && !active {
					active = true
					c.setStatus(StatusActive)
					c.log.Info("connected to portal", "cluster", body.ClusterID, "status", body.Status)
					if h := c.cfg.Handlers.OnActive; h != nil {
						go h(actx, conn)
					}
				}
			case *protocol.Error:
				if in.env.ID == regID && !registered {
					return &refusedError{code: body.Code, msg: body.Message}
				}
				c.log.Warn("portal reported an error", "id", in.env.ID, "code", body.Code, "message", body.Message)
			case *protocol.Heartbeat:
				// lastSeen is updated above.
			default:
				// A known type the portal must not send (register, request, stream.*,
				// summary, metrics, response).
				conn.sendPrio(in.env.ID, &protocol.Error{Code: protocol.CodeUnsupported, Message: "agent does not accept " + string(in.env.Type)})
			}
		}
	}
}
