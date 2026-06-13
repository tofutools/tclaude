package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookCallbackInput_ParsesCodexPayloads verifies the existing,
// harness-agnostic hook callback parses OpenAI Codex's hook stdin payloads
// (JOH-158). The payloads below mirror Codex v0.139's generated input
// schemas (openai/codex codex-rs/hooks/schema/generated/*.command.input.schema.json
// at tag rust-v0.139.0): the field names tclaude reads — session_id, cwd,
// hook_event_name, permission_mode, source, tool_name, tool_input, prompt,
// last_assistant_message, stop_hook_active, agent_id/agent_type — match
// Claude Code's field-for-field, so the same HookCallbackInput struct
// decodes both. Codex-only extras (model, turn_id, tool_use_id,
// tool_response) are simply ignored.
func TestHookCallbackInput_ParsesCodexPayloads(t *testing.T) {
	t.Run("SessionStart", func(t *testing.T) {
		// transcript_path is a nullable string in Codex's schema; null must
		// decode to "" without error.
		const payload = `{
			"session_id": "abc-123",
			"cwd": "/home/u/proj",
			"hook_event_name": "SessionStart",
			"model": "gpt-5-codex",
			"permission_mode": "default",
			"source": "startup",
			"transcript_path": null
		}`
		var in HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.Equal(t, "abc-123", in.ConvID)
		assert.Equal(t, "/home/u/proj", in.Cwd)
		assert.Equal(t, "SessionStart", in.HookEventName)
		assert.Equal(t, "startup", in.Source)
		assert.Equal(t, "", in.TranscriptPath, "null transcript_path decodes to empty")
	})

	t.Run("PreToolUse", func(t *testing.T) {
		const payload = `{
			"session_id": "abc-123",
			"cwd": "/home/u/proj",
			"hook_event_name": "PreToolUse",
			"model": "gpt-5-codex",
			"permission_mode": "dontAsk",
			"tool_name": "shell",
			"tool_input": {"command": "ls -la"},
			"tool_use_id": "call_1",
			"turn_id": "turn_1",
			"transcript_path": "/x/rollout.jsonl"
		}`
		var in HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.Equal(t, "PreToolUse", in.HookEventName)
		assert.Equal(t, "shell", in.ToolName)
		assert.Equal(t, "dontAsk", in.PermissionMode, "Codex's dontAsk mode parses (PermissionMode is a free string)")
		assert.JSONEq(t, `{"command": "ls -la"}`, string(in.ToolInput))
	})

	t.Run("Stop", func(t *testing.T) {
		const payload = `{
			"session_id": "abc-123",
			"cwd": "/home/u/proj",
			"hook_event_name": "Stop",
			"model": "gpt-5-codex",
			"permission_mode": "default",
			"last_assistant_message": "done",
			"stop_hook_active": true,
			"turn_id": "turn_1",
			"transcript_path": "/x/rollout.jsonl"
		}`
		var in HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.Equal(t, "Stop", in.HookEventName)
		assert.Equal(t, "done", in.LastAssistantMessage)
		assert.True(t, in.StopHookActive)
	})

	t.Run("UserPromptSubmit", func(t *testing.T) {
		const payload = `{
			"session_id": "abc-123",
			"cwd": "/home/u/proj",
			"hook_event_name": "UserPromptSubmit",
			"model": "gpt-5-codex",
			"permission_mode": "default",
			"prompt": "fix the bug",
			"agent_id": "ag1",
			"agent_type": "subagent",
			"turn_id": "turn_1",
			"transcript_path": "/x/rollout.jsonl"
		}`
		var in HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.Equal(t, "UserPromptSubmit", in.HookEventName)
		assert.Equal(t, "fix the bug", in.Prompt)
		assert.Equal(t, "ag1", in.AgentID)
		assert.Equal(t, "subagent", in.AgentType)
	})

	t.Run("PermissionRequest", func(t *testing.T) {
		const payload = `{
			"session_id": "abc-123",
			"cwd": "/home/u/proj",
			"hook_event_name": "PermissionRequest",
			"model": "gpt-5-codex",
			"permission_mode": "default",
			"tool_name": "apply_patch",
			"tool_input": {"path": "main.go"},
			"transcript_path": "/x/rollout.jsonl"
		}`
		var in HookCallbackInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.Equal(t, "PermissionRequest", in.HookEventName)
		assert.Equal(t, "apply_patch", in.ToolName)
	})
}
