package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const (
	// Version is the protocol version carried in every envelope as "v".
	Version = 1

	// MaxMessageSize is the largest encoded message, envelope included, that
	// either side sends or accepts.
	MaxMessageSize = 1 << 20

	// MaxStreamChunk is the most raw bytes in one stream.data message, and
	// MaxLogBytes the most in one logs response. Both fields are []byte, so
	// they travel as base64 (4/3 of the size, about 683 KiB here) whatever the
	// content: ANSI colour codes, HTML or invalid UTF-8 cannot push a message
	// over MaxMessageSize, and the bytes arrive unchanged.
	MaxStreamChunk = 512 << 10
	MaxLogBytes    = 512 << 10

	// MaxListLimit is the largest page a list request may ask for.
	MaxListLimit = 500

	// HeartbeatInterval is how often each side sends a heartbeat.
	HeartbeatInterval = 10 * time.Second

	// MetricsInterval is how often a node agent sends metrics, and
	// SummaryInterval how often the cluster agent sends a summary.
	MetricsInterval = 10 * time.Second
	SummaryInterval = 5 * time.Second

	// DisconnectAfter is how long a side waits without a heartbeat before it
	// counts the peer as Disconnected.
	DisconnectAfter = 30 * time.Second
)

// Type names a message type. It is the "type" field of the envelope.
type Type string

const (
	TypeRegister    Type = "register"
	TypeRegisterAck Type = "register.ack"
	TypeHeartbeat   Type = "heartbeat"
	TypeSummary     Type = "summary"
	TypeMetrics     Type = "metrics"
	TypeRequest     Type = "request"
	TypeResponse    Type = "response"
	TypeStreamOpen  Type = "stream.open"
	TypeStreamData  Type = "stream.data"
	TypeStreamClose Type = "stream.close"
	TypeError       Type = "error"
)

// Types lists every message type of protocol v1, in documentation order.
var Types = []Type{
	TypeRegister, TypeRegisterAck, TypeHeartbeat, TypeSummary, TypeMetrics,
	TypeRequest, TypeResponse, TypeStreamOpen, TypeStreamData, TypeStreamClose,
	TypeError,
}

// newPayload returns an empty payload value for t, or nil if t is unknown.
func newPayload(t Type) Payload {
	switch t {
	case TypeRegister:
		return &Register{}
	case TypeRegisterAck:
		return &RegisterAck{}
	case TypeHeartbeat:
		return &Heartbeat{}
	case TypeSummary:
		return &Summary{}
	case TypeMetrics:
		return &Metrics{}
	case TypeRequest:
		return &Request{}
	case TypeResponse:
		return &Response{}
	case TypeStreamOpen:
		return &StreamOpen{}
	case TypeStreamData:
		return &StreamData{}
	case TypeStreamClose:
		return &StreamClose{}
	case TypeError:
		return &Error{}
	}
	return nil
}

// Known reports whether t is a message type of protocol v1.
func Known(t Type) bool { return newPayload(t) != nil }

// NeedsID reports whether messages of type t must carry an envelope id.
// Requests and responses are correlated by id, register.ack answers the id of
// its register, and all stream messages use the id of their stream.open.
func NeedsID(t Type) bool {
	switch t {
	case TypeRegister, TypeRegisterAck, TypeRequest, TypeResponse,
		TypeStreamOpen, TypeStreamData, TypeStreamClose:
		return true
	}
	return false
}

// Envelope is the outer JSON object of every message.
type Envelope struct {
	V       int             `json:"v"`
	Type    Type            `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// LogValue logs the envelope without its payload, which can hold tokens and
// credentials. Never log an envelope's raw bytes.
func (e Envelope) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("v", e.V),
		slog.String("type", string(e.Type)),
		slog.String("id", e.ID),
		slog.Int("payloadBytes", len(e.Payload)),
	)
}

// Payload is implemented by every message body.
type Payload interface {
	// MessageType returns the envelope type this body is sent as.
	MessageType() Type
	// Validate checks the body's required fields.
	Validate() error
}

// Validate checks the envelope rules: the version, a known type, an id where
// the type needs one, and the payload size. It does not look inside the
// payload; Decode does that. An unknown type or version gives an *Error with
// code CodeUnsupported; every other problem gives CodeBadRequest or CodeTooLarge.
func Validate(env *Envelope) error {
	if env == nil {
		return errorf(CodeBadRequest, "nil envelope")
	}
	if env.Type == "" {
		return errorf(CodeBadRequest, "missing type")
	}
	if env.V != Version {
		return errorf(CodeUnsupported, "unsupported version %d, want %d", env.V, Version)
	}
	if !Known(env.Type) {
		return errorf(CodeUnsupported, "unknown message type %q", env.Type)
	}
	if NeedsID(env.Type) && env.ID == "" {
		return errorf(CodeBadRequest, "%s needs an id", env.Type)
	}
	if len(env.Payload) > MaxMessageSize {
		return errorf(CodeTooLarge, "payload is %d bytes, limit %d", len(env.Payload), MaxMessageSize)
	}
	return nil
}

// Encode builds one wire message for body with the given id. It fails if the
// body or envelope is invalid or the result is larger than MaxMessageSize.
func Encode(id string, body Payload) ([]byte, error) {
	if body == nil {
		return nil, errorf(CodeBadRequest, "nil payload")
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode %s payload: %w", body.MessageType(), err)
	}
	env := Envelope{V: Version, Type: body.MessageType(), ID: id, Payload: raw}
	if err := Validate(&env); err != nil {
		return nil, err
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode envelope: %w", err)
	}
	if len(out) > MaxMessageSize {
		return nil, errorf(CodeTooLarge, "message is %d bytes, limit %d", len(out), MaxMessageSize)
	}
	return out, nil
}

// Decode parses and checks one wire message and returns its envelope and its
// typed body (for example *Register). Unknown fields are ignored, so a newer
// v1 peer may add optional fields.
//
// On error the returned envelope is non-nil whenever the envelope itself could
// be parsed, so the caller can answer with an error message that carries the
// same id. An unknown type gives an *Error with code CodeUnsupported.
func Decode(data []byte) (*Envelope, Payload, error) {
	if len(data) > MaxMessageSize {
		return nil, nil, errorf(CodeTooLarge, "message is %d bytes, limit %d", len(data), MaxMessageSize)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil, errorf(CodeBadRequest, "invalid envelope: %v", err)
	}
	if err := Validate(&env); err != nil {
		return &env, nil, err
	}
	body := newPayload(env.Type)
	raw := bytes.TrimSpace(env.Payload)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return &env, nil, errorf(CodeBadRequest, "%s has no payload", env.Type)
	}
	if err := json.Unmarshal(raw, body); err != nil {
		return &env, nil, errorf(CodeBadRequest, "invalid %s payload: %v", env.Type, err)
	}
	if err := body.Validate(); err != nil {
		return &env, nil, err
	}
	return &env, body, nil
}
