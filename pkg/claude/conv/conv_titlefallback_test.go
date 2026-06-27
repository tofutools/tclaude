package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// JOH-216: conv listings fall back to the agent's spawn-time pending name
// when a conv has no real custom title — so a not-yet-renamed agent (e.g. a
// Codex agent whose out-of-band title write hasn't landed) shows its
// designated name instead of its raw first prompt.
func TestConvDisplayTitle_PendingNameFallback(t *testing.T) {
	pending := map[string]string{"codexconv": "codex-worker"}

	cases := []struct {
		name string
		e    SessionEntry
		want string
	}{
		{
			"real custom title wins over pending",
			SessionEntry{SessionID: "codexconv", CustomTitle: "Real Title", FirstPrompt: "hi"},
			"[Real Title]: hi",
		},
		{
			"empty custom falls back to pending name",
			SessionEntry{SessionID: "codexconv", FirstPrompt: "[tclaude] You are being started"},
			"[codex-worker]: [tclaude] You are being started",
		},
		{
			"empty custom, no pending → plain first prompt",
			SessionEntry{SessionID: "other", FirstPrompt: "just a prompt"},
			"just a prompt",
		},
		{
			"summary still used when no custom and no pending",
			SessionEntry{SessionID: "other", Summary: "a summary", FirstPrompt: "p"},
			"[a summary]: p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, convDisplayTitle(tc.e, pending))
		})
	}

	// nil map is safe and behaves as "no fallback".
	assert.Equal(t, "just a prompt",
		convDisplayTitle(SessionEntry{SessionID: "x", FirstPrompt: "just a prompt"}, nil))
}

// PendingNamesByConv returns only actors that recorded a non-empty spawn-time
// name, keyed by their current conv id.
func TestPendingNamesByConv_RoundTrip(t *testing.T) {
	setupHarnessTestHome(t) // temp HOME + fresh DB

	const named = "11111111-1111-1111-1111-111111111111"
	const unnamed = "22222222-2222-2222-2222-222222222222"
	namedAgent, _, err := db.EnsureAgentForConv(named, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentPendingName(namedAgent, "codex-worker"))
	_, _, err = db.EnsureAgentForConv(unnamed, "test") // no pending name
	require.NoError(t, err)

	got, err := db.PendingNamesByConv()
	require.NoError(t, err)
	assert.Equal(t, "codex-worker", got[named], "named actor is returned")
	_, ok := got[unnamed]
	assert.False(t, ok, "an actor with no pending name is absent from the map")
}
