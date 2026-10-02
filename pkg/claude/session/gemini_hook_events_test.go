package session

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeGeminiHookForTest(t *testing.T, raw string) HookCallbackInput {
	t.Helper()
	var input HookCallbackInput
	require.NoError(t, json.NewDecoder(bytes.NewReader([]byte(raw))).Decode(&input))
	normalizeGeminiHookEvent(&input, []byte(raw))
	return input
}

func TestNormalizeGeminiHookEvent(t *testing.T) {
	in := decodeGeminiHookForTest(t, `{"session_id":"s","cwd":"/w","hook_event_name":"BeforeAgent","prompt":"fix it"}`)
	assert.Equal(t, "UserPromptSubmit", in.HookEventName)
	assert.Equal(t, "fix it", in.Prompt)

	in = decodeGeminiHookForTest(t, `{"session_id":"s","hook_event_name":"AfterAgent","prompt":"p","prompt_response":"done","stop_hook_active":false}`)
	assert.Equal(t, "Stop", in.HookEventName)
	assert.Equal(t, "done", in.LastAssistantMessage)

	in = decodeGeminiHookForTest(t, `{"session_id":"s","hook_event_name":"AfterTool","tool_name":"run_shell_command","tool_input":{"command":"ls"}}`)
	assert.Equal(t, "PostToolUse", in.HookEventName)
	assert.Equal(t, "run_shell_command", in.ToolName)

	in = decodeGeminiHookForTest(t, `{"session_id":"s","hook_event_name":"Notification","notification_type":"ToolPermission","message":"Allow shell?","details":{}}`)
	assert.Equal(t, "Notification", in.HookEventName)
	assert.Equal(t, "permission_prompt", in.NotificationType)

	// Shared names and every other harness's events pass through untouched.
	for _, raw := range []string{
		`{"session_id":"s","hook_event_name":"SessionStart","source":"startup"}`,
		`{"session_id":"s","hook_event_name":"SessionEnd","reason":"exit"}`,
		`{"session_id":"s","hook_event_name":"Stop"}`,
		`{"session_id":"s","hook_event_name":"Notification","notification_type":"idle_prompt"}`,
		`{"session_id":"s","hook_event_name":"PreCompact","trigger":"auto"}`,
	} {
		var want HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(raw), &want))
		assert.Equal(t, want, decodeGeminiHookForTest(t, raw), raw)
	}
}

func TestNormalizeGeminiBeforeTool(t *testing.T) {
	in := decodeGeminiHookForTest(t, `{"session_id":"s","hook_event_name":"BeforeTool","tool_name":"write_file"}`)
	assert.Equal(t, "PreToolUse", in.HookEventName)
	assert.Equal(t, "write_file", in.ToolName)
}
