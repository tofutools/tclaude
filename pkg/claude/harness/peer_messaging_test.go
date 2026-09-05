package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeerMessagingSettings_OffWritesAllThreeControls(t *testing.T) {
	keys, deny := PeerMessagingSettings(false)

	assert.Equal(t, PeerMessagingInboundRefuse, keys[PeerMessagingInboundKey],
		"inbound must be hard off: a refused message is never delivered to Claude")
	assert.Equal(t, true, keys[PeerMessagingIsolateKey],
		"a send that leaves the machine must require the operator's approval")
	assert.Equal(t, []string{PeerMessagingDenyTool}, deny,
		"the deny removes peer DISCOVERY only")
}

// The load-bearing constraint of the whole feature: tclaude relies on in-harness
// subagents (the cold-review fallback), and Claude Code's docs are explicit that
// "Denying SendMessage also removes messaging to subagents and agent-team
// teammates, since the same tool serves both". Denying ListAgents does not,
// because messaging a subagent addresses it by the ID the Agent tool returned.
// If anyone ever adds SendMessage to this deny list, subagent messaging breaks
// fleet-wide and no other test in the repo would notice.
func TestPeerMessagingSettings_NeverDeniesSendMessage(t *testing.T) {
	_, deny := PeerMessagingSettings(false)
	assert.NotContains(t, deny, "SendMessage",
		"denying SendMessage would also break messaging to in-harness subagents and teammates")
}

// Opting in must be SILENT rather than force the feature on: the absent
// crossSessionInbound default is Claude Code's permission-mode-aware decision,
// which is more careful than a blunt "accept", and neither isolatePeerMachines
// nor a deny rule can be turned off from a lower-precedence scope anyway.
func TestPeerMessagingSettings_OnWritesNothing(t *testing.T) {
	keys, deny := PeerMessagingSettings(true)
	assert.Empty(t, keys, "opting in must leave Claude Code's own default in charge")
	assert.Empty(t, deny)
}

func TestClaudeSettingsCarriesPeerMessagingRefusal(t *testing.T) {
	payload := claudeSettingsJSON(SpawnSpec{})
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &got))

	assert.Equal(t, "refuse", got["crossSessionInbound"])
	assert.Equal(t, true, got["isolatePeerMachines"])
	perms, ok := got["permissions"].(map[string]any)
	require.True(t, ok, "payload must carry a permissions block: %s", payload)
	assert.Equal(t, []any{"ListAgents"}, perms["deny"])

	// Opting in leaves an otherwise-bare spec with no payload at all, so the
	// agent runs entirely on the operator's own settings.json.
	assert.Empty(t, claudeSettingsJSON(SpawnSpec{PeerMessaging: true}))
}

// The peer-messaging deny must MERGE with the sandbox-derived tool denies rather
// than replace them — both feed the single `permissions.deny` array in the one
// `--settings` payload the spawner emits.
func TestClaudeSettingsMergesPeerMessagingDenyWithSandboxDenies(t *testing.T) {
	payload := claudeSettingsJSON(SpawnSpec{
		HarnessBuiltinMode: ClaudeSandboxOn,
		SandboxDenyDirs:    []string{"/home/op/.ssh"},
	})
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &got))

	perms, ok := got["permissions"].(map[string]any)
	require.True(t, ok, "payload must carry a permissions block: %s", payload)
	deny, ok := perms["deny"].([]any)
	require.True(t, ok)

	assert.Contains(t, deny, "ListAgents", "the peer-messaging deny must survive the merge")
	var sawSSH bool
	for _, rule := range deny {
		if s, _ := rule.(string); strings.Contains(s, ".ssh") {
			sawSSH = true
		}
	}
	assert.True(t, sawSSH, "the sandbox-derived denies must survive the merge: %v", deny)
}

func TestResolvePeerMessaging(t *testing.T) {
	claude := &Harness{Name: DefaultName}
	codex := &Harness{Name: "codex"}

	// Unset resolves to OFF — the whole point of the default.
	got, err := ResolvePeerMessaging(claude, nil)
	require.NoError(t, err)
	assert.False(t, got, "an unset field must resolve to peer messaging OFF")

	got, err = ResolvePeerMessaging(claude, boolp(true))
	require.NoError(t, err)
	assert.True(t, got)

	// Opting in on a harness with no such system is an error, not a silent
	// drop, so the mistake surfaces at the spawn boundary.
	_, err = ResolvePeerMessaging(codex, boolp(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-session messaging")

	// Explicitly asking for OFF on such a harness is fine: nothing is emitted.
	got, err = ResolvePeerMessaging(codex, boolp(false))
	require.NoError(t, err)
	assert.False(t, got)

	// Unset is fine everywhere, including a nil harness.
	got, err = ResolvePeerMessaging(nil, nil)
	require.NoError(t, err)
	assert.False(t, got)
}

func TestSupportsPeerMessagingIsClaudeCodeOnly(t *testing.T) {
	assert.True(t, (&Harness{Name: DefaultName}).SupportsPeerMessaging())
	for _, name := range []string{"codex", "opencode", "copilot"} {
		assert.False(t, (&Harness{Name: name}).SupportsPeerMessaging(),
			"%s has no cross-session messaging system to steer", name)
	}
	assert.False(t, (*Harness)(nil).SupportsPeerMessaging())
}

func boolp(b bool) *bool { return &b }
