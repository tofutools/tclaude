package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A transition into the "error" status must be notify-worthy by
// default — the human wants a desktop notification when an agent's turn
// ends in an API/auth/billing error (Claude Code's StopFailure hook).
func TestDefaultConfig_NotifiesOnErrorTransition(t *testing.T) {
	n := DefaultConfig().Notifications
	n.Enabled = true // MatchesTransition short-circuits on a disabled config

	assert.True(t, n.MatchesTransition("working", "error"),
		"working→error must match a default notification rule")
	assert.True(t, n.MatchesTransition("idle", "error"),
		"the error rule uses a wildcard 'from', so any prior status matches")

	// Sanity: the pre-existing rules still match.
	assert.True(t, n.MatchesTransition("working", "idle"))
	assert.True(t, n.MatchesTransition("working", "exited"))

	// And a transition with no matching rule still does not notify.
	assert.False(t, n.MatchesTransition("idle", "working"),
		"a transition with no matching rule must not notify")
}
