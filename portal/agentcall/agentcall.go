// Package agentcall holds what every portal API that asks a cluster agent
// through the tunnel needs: finding a cluster that can be asked, and turning
// tunnel and agent errors into HTTP answers (portal/appmap, portal/resources).
package agentcall

import (
	"context"
	"errors"
	"net/http"

	"kubemap/pkg/protocol"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

// Requester sends a request to a cluster agent (tunnel.Hub).
type Requester interface {
	Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error)
}

// Error answers a failed call: a tunnel error, or an agent error returned
// as *protocol.Error. tooLarge is the hint for an answer too large for one
// message.
func Error(w http.ResponseWriter, err error, tooLarge string) {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		AgentError(w, pe, tooLarge)
		return
	}
	TunnelError(w, err)
}

// Cluster returns the cluster named by the {id} path value, if the portal
// may ask its agent. Otherwise it has written the answer: 404 for an unknown
// cluster.
func Cluster(w http.ResponseWriter, r *http.Request, st store.Store) (store.Cluster, bool) {
	c, err := st.GetCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such cluster")
		return c, false
	}
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return c, false
	}
	return c, true
}

// TunnelError answers a failed Request or OpenStream.
func TunnelError(w http.ResponseWriter, err error) {
	status, code, msg := TunnelStatus(err)
	httpjson.Error(w, status, code, msg)
}

// TunnelStatus maps a failed Request or OpenStream to an HTTP status, an
// error code and a message, for answers that report several clusters.
func TunnelStatus(err error) (int, string, string) {
	switch {
	case errors.Is(err, tunnel.ErrNotConnected):
		return http.StatusServiceUnavailable, "agent_offline", "the cluster agent is not connected"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", "the cluster agent did not answer in time"
	}
	return http.StatusBadGateway, "agent_error", "the request to the cluster agent failed"
}

// AgentError answers an error the agent returned. tooLarge is the hint for
// an answer too large for one message ("choose one namespace").
func AgentError(w http.ResponseWriter, e *protocol.Error, tooLarge string) {
	status, code, msg := Status(e, tooLarge)
	httpjson.Error(w, status, code, msg)
}

// Status maps an agent error to an HTTP status, an error code and a message
// for the UI. An agent too old for an op answers "unsupported".
func Status(e *protocol.Error, tooLarge string) (int, string, string) {
	switch e.Code {
	case protocol.CodeUnsupported:
		return http.StatusNotImplemented, "unsupported", "the cluster agent is too old for this; update it"
	case protocol.CodeNotFound:
		return http.StatusNotFound, "not_found", e.Message
	case protocol.CodeBadRequest:
		return http.StatusBadRequest, "bad_request", e.Message
	case protocol.CodeTooLarge:
		return http.StatusUnprocessableEntity, "too_large", "the answer is too large; " + tooLarge
	case protocol.CodeTimeout:
		return http.StatusGatewayTimeout, "timeout", e.Message
	case protocol.CodeUnavailable:
		return http.StatusServiceUnavailable, "unavailable", e.Message
	case protocol.CodeForbidden:
		return http.StatusBadGateway, "agent_forbidden", "the cluster agent is not allowed to read this: " + e.Message
	}
	return http.StatusBadGateway, "agent_error", "the cluster agent failed: " + e.Message
}
