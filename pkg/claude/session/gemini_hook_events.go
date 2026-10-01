package session

import "encoding/json"

// geminiHookEventNames maps Gemini CLI's hook event names onto the Claude Code
// vocabulary the rest of the callback speaks. Gemini's payload is otherwise
// the same snake_case shape (session_id, transcript_path, cwd,
// hook_event_name, prompt, tool_name, tool_input, tool_response,
// stop_hook_active, source, reason, message, notification_type), so the event
// name is the only translation most events need.
//
// The source names are unique to Gemini — no other harness emits BeforeAgent,
// AfterAgent, BeforeTool or AfterTool — so the mapping is decided from the payload alone
// and needs no per-harness flag on the installed command.
var geminiHookEventNames = map[string]string{
	"BeforeAgent": "UserPromptSubmit",
	"BeforeTool":  "PreToolUse",
	"AfterAgent":  "Stop",
	"AfterTool":   "PostToolUse",
}

// geminiToolPermissionNotification is Gemini's notification_type for a tool
// confirmation dialog — the one Notification it emits. Claude Code spells the
// same signal "permission_prompt", and Claude's own types are lowercase, so
// the spelling alone identifies the source.
const geminiToolPermissionNotification = "ToolPermission"

// normalizeGeminiHookEvent rewrites a Gemini hook payload into tclaude's
// vocabulary in place. raw is the undecoded payload, for the one field whose
// name differs (AfterAgent's prompt_response). Payloads from every other
// harness pass through untouched.
func normalizeGeminiHookEvent(input *HookCallbackInput, raw []byte) {
	if input == nil {
		return
	}
	if input.HookEventName == "Notification" && input.NotificationType == geminiToolPermissionNotification {
		input.NotificationType = "permission_prompt"
		return
	}
	mapped, ok := geminiHookEventNames[input.HookEventName]
	if !ok {
		return
	}
	if input.HookEventName == "AfterAgent" && input.LastAssistantMessage == "" {
		var extra struct {
			PromptResponse string `json:"prompt_response"`
		}
		if json.Unmarshal(raw, &extra) == nil {
			input.LastAssistantMessage = extra.PromptResponse
		}
	}
	input.HookEventName = mapped
}
