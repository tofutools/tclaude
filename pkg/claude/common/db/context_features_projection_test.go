package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The startup-context trim map (TCL-597) lives on a PRUNABLE session row, so it
// only survives a relaunch by being projected onto the durable
// conversation/agent relaunch profiles. These pin that projection, including the
// decay case that would otherwise silently un-trim a long-running agent.

func TestSetSessionContextFeaturesProjectsToDurableIntent(t *testing.T) {
	setupTestDB(t)
	const convID = "context-features-conversation"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "context-features-session", ConvID: convID, Cwd: "/tmp/context-features",
		Harness: "claude", Status: "idle",
	}))

	trims := map[string]string{"bundled-skills": "off", "artifact": "on"}
	require.NoError(t, SetSessionContextFeatures("context-features-session", trims))

	conversation, err := ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	require.NotNil(t, conversation.FallbackRelaunch)
	require.NotNil(t, conversation.FallbackRelaunch.ContextFeatures)
	assert.Equal(t, trims, *conversation.FallbackRelaunch.ContextFeatures)

	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.ContextFeatures)
	assert.Equal(t, trims, *agent.ContextFeatures)

	// And the read path every relaunch uses agrees.
	resolved, err := ContextFeaturesForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, trims, resolved)
}

func TestContextFeaturesDoNotDecayAcrossSessionGenerations(t *testing.T) {
	setupTestDB(t)
	const convID = "context-features-decay-conversation"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "context-features-gen1", ConvID: convID, Cwd: "/tmp/context-features-decay",
		Harness: "claude", Status: "idle",
	}))
	trims := map[string]string{"workflows": "off"}
	require.NoError(t, SetSessionContextFeatures("context-features-gen1", trims))

	// A relaunch creates a NEW session row for the same conv, whose
	// context_features column starts empty. Saving that row must not roll the
	// durable trims back to "nothing": SaveSession's UPSERT deliberately does not
	// own the column, exactly like remote_control / auto_memory.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "context-features-gen2", ConvID: convID, Cwd: "/tmp/context-features-decay",
		Harness: "claude", Status: "idle",
	}))

	resolved, err := ContextFeaturesForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, trims, resolved,
		"a new session generation must not silently un-trim the agent")

	// The relaunched pane then records its own resolved posture, which is what
	// keeps the value alive for the generation after it.
	require.NoError(t, SetSessionContextFeatures("context-features-gen2", trims))
	resolved, err = ContextFeaturesForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, trims, resolved)
}

func TestSetSessionContextFeaturesRecordsAnExplicitEmptyMap(t *testing.T) {
	setupTestDB(t)
	const convID = "context-features-empty-conversation"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "context-features-empty-session", ConvID: convID, Cwd: "/tmp/context-features-empty",
		Harness: "claude", Status: "idle",
	}))

	// "This agent trims nothing" is KNOWN intent, distinct from a legacy row that
	// was never recorded. The projection must therefore store a non-nil pointer to
	// an empty map rather than leaving the field nil (unknown).
	require.NoError(t, SetSessionContextFeatures("context-features-empty-session", nil))

	agent, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.ContextFeatures, "an explicit 'no trims' is known intent, not unknown")
	assert.Empty(t, *agent.ContextFeatures)
}

func TestNonClaudeSessionProjectionDropsContextFeatures(t *testing.T) {
	setupTestDB(t)
	const convID = "context-features-codex-conversation"
	_, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "context-features-codex-session", ConvID: convID, Cwd: "/tmp/context-features-codex",
		Harness: "codex", Status: "idle",
	}))

	// Claude-only flags on a non-Claude row are process telemetry, not durable
	// intent — the same normalization remote_control / auto_memory / ask-timeout
	// get, so a stale or hand-edited row cannot arm a Claude-only feature if the
	// agent later relaunches.
	require.NoError(t, SetSessionContextFeatures("context-features-codex-session",
		map[string]string{"bundled-skills": "off"}))

	resolved, err := ContextFeaturesForConv(convID)
	require.NoError(t, err)
	assert.Empty(t, resolved)
}
