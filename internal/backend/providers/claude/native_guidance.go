package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type nativeHookInput struct {
	SessionID        string `json:"session_id"`
	HookEventName    string `json:"hook_event_name"`
	AgentID          string `json:"agent_id"`
	NotificationType string `json:"notification_type"`
}

func claudeNativeNormalizer(nativeID string) func(ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error) {
	return func(raw ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error) {
		var input nativeHookInput
		if len(raw.Body) == 0 || len(raw.Body) > 1<<20 || !json.Valid(raw.Body) || json.Unmarshal(raw.Body, &input) != nil {
			return ports.NormalizedNativeEvent{}, "", fmt.Errorf("invalid Claude native hook payload")
		}
		if input.SessionID != nativeID || input.AgentID != "" {
			return ports.NormalizedNativeEvent{}, "", fmt.Errorf("claude native hook session does not match execution")
		}
		kind := ""
		switch input.HookEventName {
		case "SessionStart":
			kind = "session_start"
		case "UserPromptSubmit":
			kind = "user_prompt"
		case "PreToolUse", "PostToolUse":
			kind = "activity_active"
		case "Stop":
			// A Stop hook may still be blocked by another hook. It proves no
			// waiting/idle state; only a later native notification can do that.
			kind = "activity_unknown"
		case "Notification":
			switch input.NotificationType {
			case "idle_prompt":
				kind = "activity_idle"
			case "permission_prompt", "elicitation_dialog":
				kind = "activity_awaiting_input"
			default:
				return ports.NormalizedNativeEvent{}, "", fmt.Errorf("unsupported Claude activity notification")
			}
		default:
			return ports.NormalizedNativeEvent{}, "", fmt.Errorf("unsupported Claude native hook event %q", input.HookEventName)
		}
		digest := sha256.Sum256(append([]byte(fmt.Sprintf("%d\x00", raw.ReceivedAt.UnixNano())), raw.Body...))
		return ports.NormalizedNativeEvent{
			EventID: "claude:" + hex.EncodeToString(digest[:]), Kind: kind,
			OccurredAt: raw.ReceivedAt, ObservedAt: raw.ReceivedAt,
			NativeCorrelation: input.SessionID, Payload: append(json.RawMessage(nil), raw.Body...),
			Timing: model.StandingOrderSameContinuation,
		}, input.HookEventName, nil
	}
}

func encodeClaudeGuidance(nativeKind, guidance string) ([]byte, error) {
	if strings.TrimSpace(guidance) == "" {
		return nil, fmt.Errorf("claude native guidance is empty")
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": nativeKind, "additionalContext": guidance,
	}})
}
