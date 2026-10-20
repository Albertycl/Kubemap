package protocol

import (
	"errors"
	"fmt"
)

// ErrorCode is a machine-readable error reason.
type ErrorCode string

const (
	CodeBadRequest   ErrorCode = "bad_request"  // malformed or invalid message
	CodeUnsupported  ErrorCode = "unsupported"  // unknown message type or operation
	CodeTooLarge     ErrorCode = "too_large"    // over MaxMessageSize
	CodeUnauthorized ErrorCode = "unauthorized" // token missing, wrong or expired
	CodeForbidden    ErrorCode = "forbidden"    // authenticated, but RBAC or policy says no
	CodeNotFound     ErrorCode = "not_found"    // object or stream does not exist
	CodeTimeout      ErrorCode = "timeout"      // the Kubernetes API or peer did not answer in time
	CodeUnavailable  ErrorCode = "unavailable"  // temporarily unable; retry later
	CodeInternal     ErrorCode = "internal"     // bug or unexpected failure
)

var errorCodes = map[ErrorCode]bool{
	CodeBadRequest: true, CodeUnsupported: true, CodeTooLarge: true,
	CodeUnauthorized: true, CodeForbidden: true, CodeNotFound: true,
	CodeTimeout: true, CodeUnavailable: true, CodeInternal: true,
}

// Error is both the payload of an "error" message and the error value that
// Encode, Decode and Validate return. Response and StreamClose embed it too.
// For an "error" message, the envelope id is the id of the message that caused
// it, or empty when that id is unknown.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("protocol: %s: %s", e.Code, e.Message) }

// MessageType implements Payload.
func (*Error) MessageType() Type { return TypeError }

// Validate implements Payload.
func (e *Error) Validate() error {
	if !errorCodes[e.Code] {
		return errorf(CodeBadRequest, "unknown error code %q", e.Code)
	}
	return nil
}

// AsError converts any error to an *Error. A wrapped *Error keeps its code;
// anything else becomes CodeInternal. It returns nil for a nil error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

func errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
