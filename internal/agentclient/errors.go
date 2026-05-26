// Package agentclient is the HTTP client the live-boot agent uses to talk to
// the controller's /api/v1/agent/* endpoints. Pure stdlib, no third-party deps.
//
// The wire protocol is documented in docs/api.md §13. All control-plane calls
// carry the machine's SMBIOS UUID in either the query string (GETs) or JSON
// body (POSTs); the controller uses it for consistency checks, not auth (M2.5
// will layer a token in).
package agentclient

import (
	"errors"
	"fmt"
)

// APIError wraps a non-2xx response from the controller. Code is the HTTP
// status; Message is the controller's {"error": "..."} body (falls back to the
// raw response bytes on parse failure).
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("agentclient: http %d", e.Code)
	}
	return fmt.Sprintf("agentclient: http %d: %s", e.Code, e.Message)
}

// ErrNoCurrentJob is returned by Client.CurrentJob when the controller answers
// 404 — the machine has no in-flight job. Use errors.Is to detect it.
var ErrNoCurrentJob = errors.New("agentclient: no current job")
