// Package protocol defines the tunnel messages shared by the agent and the portal.
// Owned by km-agent; changes are also reviewed by km-portal.
//
// Every WebSocket text frame carries one JSON Envelope {v, type, id, payload}.
// Each message type has one payload struct (Register, Heartbeat, Summary, ...)
// that implements Payload. Use Encode to build a frame and Decode to parse and
// check one; Validate checks the envelope rules alone.
//
// Never log a raw message or Envelope bytes: payloads carry tokens and
// credentials. Envelope, Register, RegisterAck, Request and
// TokenReviewRequest implement slog.LogValuer and leave secrets out.
//
// The package has no network code. The transport lives in the portal (KM-18)
// and the agent (KM-21). The wire format is documented in docs/protocol.md.
package protocol
