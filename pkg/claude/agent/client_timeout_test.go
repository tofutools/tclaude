package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestDaemonClientTimeoutCombinesApprovalAndEndpointBudgets is the regression
// for --ask-human silently discarding a caller's Timeout.
//
// The daemon resolves the approval popup in requirePermission and only THEN
// runs the endpoint, so the two waits happen in sequence. While this was a
// switch on AskHuman first, a caller that set both got only the approval
// budget — so `tclaude proxy github run log-failed <id> --ask-human 30s` gave
// the client 60s against a daemon that is allowed 180s for the log download
// alone. The read succeeds daemon-side and the agent is told nobody answered.
func TestDaemonClientTimeoutCombinesApprovalAndEndpointBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts DaemonOpts
		want time.Duration
	}{
		{"neither set falls back to the default", DaemonOpts{}, 0},
		{"timeout alone is honoured",
			DaemonOpts{Timeout: 210 * time.Second}, 210 * time.Second},
		{"approval alone gets its own headroom",
			DaemonOpts{AskHuman: 60 * time.Second}, 90 * time.Second},
		{"both add up rather than one winning",
			DaemonOpts{AskHuman: 30 * time.Second, Timeout: 210 * time.Second},
			270 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, daemonClientTimeout(tc.opts))
		})
	}

	// The property that actually matters, stated directly: adding --ask-human
	// must never shrink the budget a caller already asked for.
	slow := DaemonOpts{Timeout: 210 * time.Second}
	assert.GreaterOrEqual(t,
		daemonClientTimeout(DaemonOpts{AskHuman: 5 * time.Second, Timeout: slow.Timeout}),
		daemonClientTimeout(slow),
		"--ask-human must not cost the caller the endpoint budget it asked for")
}
