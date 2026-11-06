package agent

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A refusal exits 5 (rcAuth), apart from an I/O failure (4), whatever code
// the daemon gives it: "permission" (the common one), "auth", or any other
// 403. Before tcl-cezwhu only "auth" exited 5, so a permission denial looked
// like agentd being down; `agent seance` alone mapped it, in its own copy.
func TestMapDaemonErrorToRCExitsFiveOnARefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"permission", &DaemonError{Status: http.StatusForbidden, Code: "permission"}, rcAuth},
		{"auth", &DaemonError{Status: http.StatusUnauthorized, Code: "auth"}, rcAuth},
		{"specific 403", &DaemonError{Status: http.StatusForbidden, Code: "operator_only"}, rcAuth},
		{"federation 403", &DaemonError{Status: http.StatusForbidden, Code: "permission_denied"}, rcAuth},
		{"not found", &DaemonError{Status: http.StatusNotFound, Code: "not_found"}, rcNotFound},
		{"invalid arg", &DaemonError{Status: http.StatusBadRequest, Code: "invalid_arg"}, rcInvalidArg},
		{"daemon failure", &DaemonError{Status: http.StatusInternalServerError, Code: "io"}, rcIOFailure},
		{"unreachable", errors.New("dial unix: connection refused"), rcIOFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MapDaemonErrorToRC(tc.err))
		})
	}
	// The séance command agrees for the shared codes, and keeps its own.
	assert.Equal(t, rcAuth, mapSeanceDaemonErrorToRC(&DaemonError{Status: http.StatusForbidden, Code: "permission"}))
	assert.Equal(t, rcInvalidArg, mapSeanceDaemonErrorToRC(&DaemonError{Status: http.StatusBadRequest, Code: "unsupported_harness"}))
}
