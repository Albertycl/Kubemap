package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"kubemap/pkg/protocol"
)

// conn is one agent WebSocket. Writes may come from several goroutines (the
// read loop, heartbeats, Request, Accept); coder/websocket allows that.
type conn struct {
	ws     *websocket.Conn
	ip     string
	closed chan struct{}
	once   sync.Once
	// local, when set, replaces the WebSocket: the in-process cluster agent
	// receives messages through it (local.go).
	local func(ctx context.Context, id string, body protocol.Payload) error
}

func newConn(ws *websocket.Conn, ip string) *conn {
	return &conn{ws: ws, ip: ip, closed: make(chan struct{})}
}

const writeTimeout = 10 * time.Second

// send encodes body and writes it as one text message.
func (c *conn) send(ctx context.Context, id string, body protocol.Payload) error {
	if c.local != nil {
		select {
		case <-c.closed:
			return ErrNotConnected
		default:
		}
		return c.local(ctx, id, body)
	}
	b, err := protocol.Encode(id, body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// sendError answers with an error message. Failures are ignored: the
// connection is usually about to close.
func (c *conn) sendError(ctx context.Context, id string, e *protocol.Error) {
	c.send(ctx, id, e)
}

// read waits up to timeout for the next message. A message that cannot be
// decoded is returned with its error, and env set when the envelope parsed,
// so the caller can answer with the same id.
func (c *conn) read(ctx context.Context, timeout time.Duration) (*protocol.Envelope, protocol.Payload, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, nil, &readError{err}
	}
	if typ != websocket.MessageText {
		return nil, nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "binary messages are not used"}
	}
	return protocol.Decode(data)
}

// readError is a transport failure (closed, timed out), as opposed to a bad
// message on a working connection.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

func isTransport(err error) bool {
	var re *readError
	return errors.As(err, &re)
}

// close ends the connection once, with a status code and a short reason.
// It does not wait: the close handshake waits up to 5 s for the peer, and a
// peer being replaced is often a dead connection.
func (c *conn) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.closed)
		if c.ws != nil {
			go c.ws.Close(code, reason)
		}
	})
}

// heartbeat sends a heartbeat every interval until the connection closes.
func (c *conn) heartbeat(ctx context.Context, interval time.Duration, now func() time.Time) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			if err := c.send(ctx, "", &protocol.Heartbeat{Time: now().UTC()}); err != nil {
				c.close(websocket.StatusGoingAway, "heartbeat failed")
				return
			}
		}
	}
}

// heartbeatLocal calls beat every interval until the connection closes: the
// in-process agent has no socket to send heartbeats on.
func (c *conn) heartbeatLocal(ctx context.Context, interval time.Duration, beat func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			beat()
		}
	}
}
