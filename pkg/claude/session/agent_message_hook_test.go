package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestApplyHookRegularMessageProcessingAcrossHarnesses(t *testing.T) {
	for _, harnessName := range []string{"claude", "codex"} {
		t.Run(harnessName, func(t *testing.T) {
			convID := "hook-message-" + harnessName
			apply := ledgerWorld(t, "hook-session-"+harnessName, convID,
				&SessionState{Status: StatusIdle, Harness: harnessName})

			inlineID, _, err := db.InsertAgentMessageBounded(&db.AgentMessage{ToConv: convID, Body: "inline body"}, 10)
			require.NoError(t, err)
			apply(HookCallbackInput{
				HookEventName: "UserPromptSubmit",
				Prompt:        fmt.Sprintf("harness wrapper\n[system: new agent message #%d; delivery: inline] inline body", inlineID),
			})
			inline, err := db.GetAgentMessage(inlineID)
			require.NoError(t, err)
			assert.False(t, inline.StartedAt.IsZero())
			assert.False(t, inline.ReadAt.IsZero())
			assert.True(t, inline.ProcessedAt.IsZero(), "submit alone is not terminal processing")

			apply(HookCallbackInput{HookEventName: "Stop"})
			inline, err = db.GetAgentMessage(inlineID)
			require.NoError(t, err)
			assert.False(t, inline.ProcessedAt.IsZero(), "terminal turn hook acknowledges inline processing")

			pointerID, _, err := db.InsertAgentMessageBounded(&db.AgentMessage{ToConv: convID, Body: "pointer body"}, 10)
			require.NoError(t, err)
			apply(HookCallbackInput{
				HookEventName: "UserPromptSubmit",
				Prompt:        fmt.Sprintf("[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]", pointerID, pointerID),
			})
			apply(HookCallbackInput{HookEventName: "StopFailure", ErrorType: "rate_limit"})
			pointer, err := db.GetAgentMessage(pointerID)
			require.NoError(t, err)
			assert.False(t, pointer.StartedAt.IsZero())
			assert.True(t, pointer.ReadAt.IsZero())
			assert.True(t, pointer.ProcessedAt.IsZero(), "terminal pointer turn cannot claim the unread body")

			require.NoError(t, db.MarkAgentMessageRead(pointerID))
			pointer, err = db.GetAgentMessage(pointerID)
			require.NoError(t, err)
			assert.False(t, pointer.ProcessedAt.IsZero(), "explicit inbox read frees capacity")
		})
	}
}

func TestAgentMessagePromptFindsWrappedServerMarker(t *testing.T) {
	id, inline, ok := agentMessagePrompt("prefix added by harness\n[system: new agent message #42; delivery: inline; subject: review] body")
	assert.True(t, ok)
	assert.Equal(t, int64(42), id)
	assert.True(t, inline)

	_, _, ok = agentMessagePrompt("ordinary user prompt")
	assert.False(t, ok)
}
