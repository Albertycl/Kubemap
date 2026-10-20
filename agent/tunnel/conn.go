package tunnel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
)

// writeTimeout bounds one write; a stuck connection is closed.
const writeTimeout = 10 * time.Second

// queueLen is how many messages wait in each lane.
const queueLen = 64

// incoming is one decoded message, or an error that ends the connection
// (env nil) or that deserves an error reply (env set).
type incoming struct {
	env  *protocol.Envelope
	body protocol.Payload
	err  error
}

// conn wraps one WebSocket with a reader goroutine and a writer goroutine.
// The writer has two lanes: priority (register, heartbeats, responses,
// errors, stream.close) and bulk (summary, metrics, stream.data). The
// priority lane always goes first, so a busy log stream cannot delay a
// heartbeat into a false Disconnected (transport rule 5).
type conn struct {
	ctx   context.Context
	stop  context.CancelFunc
	ws    *websocket.Conn
	log   *slog.Logger
	in    chan incoming
	prio  chan []byte
	bulk  chan []byte
	errMu sync.Mutex
	first error
	done  chan struct{}
}

func newConn(ctx context.Context, ws *websocket.Conn, stop context.CancelFunc, log *slog.Logger) *conn {
	c := &conn{ctx: ctx, stop: stop, ws: ws, log: log,
		in: make(chan incoming, 16), prio: make(chan []byte, queueLen), bulk: make(chan []byte, queueLen),
		done: make(chan struct{})}
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *conn) fail(err error) {
	c.errMu.Lock()
	if c.first == nil {
		c.first = err
	}
	c.errMu.Unlock()
	c.stop()
}

func (c *conn) err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.first
}

func (c *conn) readLoop() {
	defer close(c.in)
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.fail(err)
			return
		}
		var m incoming
		if typ != websocket.MessageText {
			m.err = &protocol.Error{Code: protocol.CodeBadRequest, Message: "binary frames are not used"}
		} else {
			m.env, m.body, m.err = protocol.Decode(data)
		}
		select {
		case c.in <- m:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *conn) writeLoop() {
	defer close(c.done)
	write := func(b []byte) bool {
		wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
		defer cancel()
		if err := c.ws.Write(wctx, websocket.MessageText, b); err != nil {
			c.fail(err)
			return false
		}
		return true
	}
	for {
		// Drain the priority lane first.
		select {
		case b := <-c.prio:
			if !write(b) {
				return
			}
			continue
		default:
		}
		select {
		case <-c.ctx.Done():
			return
		case b := <-c.prio:
			if !write(b) {
				return
			}
		case b := <-c.bulk:
			if !write(b) {
				return
			}
		}
	}
}

func (c *conn) enqueue(lane chan []byte, id string, p protocol.Payload) error {
	b, err := protocol.Encode(id, p)
	if err != nil {
		return err
	}
	select {
	case lane <- b:
		return nil
	case <-c.ctx.Done():
		if err := c.err(); err != nil {
			return err
		}
		return c.ctx.Err()
	}
}

func (c *conn) sendPrio(id string, p protocol.Payload) error { return c.enqueue(c.prio, id, p) }

// Send implements Sender: heartbeats, responses, errors and stream.close go
// in the priority lane, everything else in the bulk lane.
func (c *conn) Send(id string, p protocol.Payload) error {
	switch p.(type) {
	case *protocol.Heartbeat, *protocol.Response, *protocol.Error, *protocol.StreamClose:
		return c.sendPrio(id, p)
	}
	return c.enqueue(c.bulk, id, p)
}

// close ends the connection without waiting for the portal's close reply
// (coder/websocket's Close waits up to 5 s).
func (c *conn) close() {
	c.stop()
	_ = c.ws.CloseNow()
	<-c.done
}
