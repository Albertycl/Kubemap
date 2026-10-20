package tunnel

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"kubemap/pkg/protocol"
)

// streamBuffer is how many stream.data chunks may wait for the reader (the
// browser's HTTP response). A reader that falls further behind ends the
// stream: the connection's read loop never waits for one slow stream.
const streamBuffer = 32

// Stream is a stream the portal opened on a cluster agent, for example
// following a pod log (protocol stream.open). Read it with Next until it
// ends; Close cancels it.
type Stream struct {
	id string
	cc *clusterConn

	events chan []byte   // data chunks, in order
	done   chan struct{} // closed when the stream has ended
	mu     sync.Mutex
	final  *protocol.StreamClose // why it ended; set before done is closed
	next   uint64                // expected seq of the next chunk
	ended  bool
}

// OpenStream starts a stream on the cluster agent. A cluster with no agent
// attached gives ErrNotConnected.
func (h *Hub) OpenStream(ctx context.Context, clusterID string, o *protocol.StreamOpen) (*Stream, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	c := h.clus[clusterID]
	h.mu.Unlock()
	if c == nil {
		return nil, ErrNotConnected
	}
	s := &Stream{
		id: "s-" + strconv.FormatUint(h.seq.Add(1), 10), cc: c,
		events: make(chan []byte, streamBuffer), done: make(chan struct{}),
	}
	c.mu.Lock()
	c.streams[s.id] = s
	c.mu.Unlock()
	if err := c.send(ctx, s.id, o); err != nil {
		s.forget()
		return nil, fmt.Errorf("tunnel: send stream.open: %w", err)
	}
	return s, nil
}

// Next returns the next chunk of data, or, once the stream has ended and
// every chunk was read, the stream.close that ended it (with a nil chunk).
// It returns ctx's error if ctx ends first; the stream stays open.
func (s *Stream) Next(ctx context.Context) ([]byte, *protocol.StreamClose, error) {
	select {
	case b := <-s.events:
		return b, nil, nil
	case <-s.done:
		select { // chunks that came before the end are delivered first
		case b := <-s.events:
			return b, nil, nil
		default:
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return nil, s.final, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// Close cancels the stream: the agent is told with stream.close (it does not
// answer). Closing an ended stream does nothing.
func (s *Stream) Close() {
	if s.end(&protocol.StreamClose{Reason: "closed by the portal"}) {
		s.cc.send(context.Background(), s.id, &protocol.StreamClose{Reason: "closed by the portal"})
	}
}

// end records why the stream ended and forgets it. It reports whether this
// call ended it (false if it had already ended).
func (s *Stream) end(final *protocol.StreamClose) bool {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return false
	}
	s.ended, s.final = true, final
	close(s.done)
	s.mu.Unlock()
	s.forget()
	return true
}

func (s *Stream) forget() {
	s.cc.mu.Lock()
	if s.cc.streams[s.id] == s {
		delete(s.cc.streams, s.id)
	}
	s.cc.mu.Unlock()
}

// data delivers one chunk from the agent. It never blocks: a chunk out of
// order, or one the reader has no room for, ends the stream, and the agent
// is told to stop.
func (s *Stream) data(m *protocol.StreamData) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	var fail *protocol.Error
	if m.Seq != s.next {
		fail = &protocol.Error{Code: protocol.CodeInternal, Message: fmt.Sprintf("stream data out of order: got seq %d, want %d", m.Seq, s.next)}
	} else {
		select {
		case s.events <- m.Data:
			s.next++
		default:
			fail = &protocol.Error{Code: protocol.CodeUnavailable, Message: "the reader is too slow; open the stream again"}
		}
	}
	s.mu.Unlock()
	if fail != nil && s.end(&protocol.StreamClose{Error: fail}) {
		s.cc.send(context.Background(), s.id, &protocol.StreamClose{Error: fail})
	}
}

// failAll ends every open stream of a connection that has gone.
func (c *clusterConn) failAll() {
	c.mu.Lock()
	all := make([]*Stream, 0, len(c.streams))
	for _, s := range c.streams {
		all = append(all, s)
	}
	c.mu.Unlock()
	for _, s := range all {
		s.end(&protocol.StreamClose{Error: &protocol.Error{Code: protocol.CodeUnavailable, Message: "the cluster agent disconnected"}})
	}
}
