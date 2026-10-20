package agentcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"kubemap/pkg/protocol"
	"kubemap/portal/tunnel"
)

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		code   protocol.ErrorCode
		status int
		want   string
	}{
		{protocol.CodeUnsupported, 501, "unsupported"},
		{protocol.CodeNotFound, 404, "not_found"},
		{protocol.CodeBadRequest, 400, "bad_request"},
		{protocol.CodeTooLarge, 422, "too_large"},
		{protocol.CodeTimeout, 504, "timeout"},
		{protocol.CodeUnavailable, 503, "unavailable"},
		{protocol.CodeForbidden, 502, "agent_forbidden"},
		{protocol.CodeInternal, 502, "agent_error"},
	} {
		status, code, msg := Status(&protocol.Error{Code: tc.code, Message: "detail"}, "choose one namespace")
		if status != tc.status || code != tc.want {
			t.Errorf("%s: %d %s", tc.code, status, code)
		}
		if tc.code == protocol.CodeTooLarge && !strings.Contains(msg, "choose one namespace") {
			t.Errorf("too_large message %q has no hint", msg)
		}
	}
}

func TestTunnelError(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{tunnel.ErrNotConnected, 503, "agent_offline"},
		{fmt.Errorf("tunnel: request list: %w", context.DeadlineExceeded), 504, "timeout"},
		{errors.New("tunnel: send request: closed"), 502, "agent_error"},
	} {
		rec := httptest.NewRecorder()
		TunnelError(rec, tc.err)
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != tc.status || body["error"] != tc.code {
			t.Errorf("%v: %d %v", tc.err, rec.Code, body)
		}
	}
}
