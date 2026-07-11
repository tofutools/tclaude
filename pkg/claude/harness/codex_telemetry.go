package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// Context-window telemetry for Codex. Claude Code surfaces "how full is the
// context" through its command-statusline (tclaude's status-bar reads the
// context_window block off stdin and persists it onto the sessions row).
// Codex has no command-backed status line (openai/codex#17827), so the same
// figures have to be lifted from the rollout instead: Codex emits a
// `token_count` event_msg after each model response carrying the per-turn
// and cumulative token usage plus the model context window. JOH-170 reads
// the latest such event and persists it onto the SAME sessions-row columns
// CC populates, so the dashboard / `agent context-info` render context% for
// Codex sessions through one unchanged read path.

// ContextTelemetry is the per-turn context-window snapshot for a
// conversation — the cross-harness analog of the figures Claude Code's
// statusline carries (used %, input/output tokens, window size). All token
// fields come from the LAST turn (not the cumulative session total), so Pct
// reflects current window occupancy rather than lifetime spend.
type ContextTelemetry struct {
	// Pct is context-window occupancy in 0..100. 0 when the window size is
	// unknown (no model_context_window on the event).
	Pct float64
	// TokensInput is the last turn's input tokens — the full prompt Codex
	// re-sent, i.e. the whole prior conversation resident in the window.
	TokensInput int64
	// TokensOutput is the last turn's output tokens (includes reasoning).
	TokensOutput int64
	// WindowSize is the model's context window (model_context_window).
	WindowSize int64
}

// CodexRuntimeSnapshot is the live, rollout-derived state that has no direct
// Codex statusline equivalent. Context is populated from token_count events.
// InterruptedSubagents is the authoritative set of collaboration-child IDs
// whose latest recorded lifecycle state is terminal interrupt, even when the
// SubagentStop hook was lost. A later started/interacted event clears that ID
// because Codex can resume the same child thread. Normal completion still
// belongs to the hook ledger: the rollout has no equivalent terminal
// "completed" activity kind, so rollout activity must never be treated as a
// complete active-set reconstruction.
type CodexRuntimeSnapshot struct {
	Context              ContextTelemetry
	HasContext           bool
	InterruptedSubagents map[string]struct{}
}

// codexTokenUsage mirrors a token-usage block inside a token_count event.
// Both total_token_usage (cumulative) and last_token_usage (this turn) use
// this shape. cached_input_tokens is a subset of input_tokens (a cache hit),
// not additive, so it never enters the occupancy math.
type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// codexTokenCountInfo is the `info` block of a token_count event_msg.
type codexTokenCountInfo struct {
	TotalTokenUsage    codexTokenUsage `json:"total_token_usage"`
	LastTokenUsage     codexTokenUsage `json:"last_token_usage"`
	ModelContextWindow int64           `json:"model_context_window"`
}

// codexTokenCountEvent is the `event_msg` payload of a token_count line. The
// outer envelope's type is event_msg; this inner type selects token_count.
// RateLimits is a sibling of Info carrying the account-wide subscription
// limits; codex_usage.go reads it while the telemetry path here reads Info.
type codexTokenCountEvent struct {
	Type       string              `json:"type"`
	Info       codexTokenCountInfo `json:"info"`
	RateLimits *codexRateLimits    `json:"rate_limits"`
}

// codexSubagentActivityEvent mirrors Codex's sub_agent_activity event_msg.
// Codex 0.144 exposes exactly three kinds: started, interacted, and interrupted.
// Only interrupted is a terminal fact; agent_thread_id is the stable key shared
// with the corresponding Subagent* hook payloads.
type codexSubagentActivityEvent struct {
	Type          string `json:"type"`
	EventID       string `json:"event_id"`
	AgentThreadID string `json:"agent_thread_id"`
	Kind          string `json:"kind"`
}

// codexFunctionCall identifies the collaboration tool call that produced a
// sub_agent_activity event. event_id on the activity equals call_id here. This
// matters for "interacted": followup_task resumes/triggers the child, whereas
// send_message only queues text and is not live evidence.
type codexFunctionCall struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	CallID string `json:"call_id"`
}

// CodexRuntimeTelemetry locates convID's rollout and derives all runtime state
// that tclaude reads from it in one scan. A missing rollout is a normal zero
// snapshot, not an error.
func CodexRuntimeTelemetry(home, convID string) (CodexRuntimeSnapshot, error) {
	path, err := findCodexRollout(home, convID)
	if err != nil {
		return CodexRuntimeSnapshot{}, err
	}
	if path == "" {
		return CodexRuntimeSnapshot{}, nil
	}
	return CodexRuntimeTelemetryFromRollout(path)
}

// CodexContextTelemetry locates convID's rollout under home and returns the
// latest token_count snapshot. ok is false (with a nil error) when there is
// no rollout for convID or it carries no token_count event yet — both are
// the normal "nothing to persist" state of a just-started session, not
// failures. A non-nil error is an I/O / scan fault the caller should log.
func CodexContextTelemetry(home, convID string) (ContextTelemetry, bool, error) {
	snap, err := CodexRuntimeTelemetry(home, convID)
	if err != nil {
		return ContextTelemetry{}, false, err
	}
	return snap.Context, snap.HasContext, nil
}

// CodexTelemetryFromRollout reads rolloutPath (transparently decompressing
// `.zst`) and returns the LAST token_count event's snapshot. A Codex turn
// can emit several token_count events (one per model response, including
// tool-call rounds); the last one reflects the window after the most recent
// response, so the scan keeps the last seen rather than stopping early. A
// malformed line is skipped; only an I/O / scanner error is returned. ok is
// false when no token_count event is present.
func CodexTelemetryFromRollout(rolloutPath string) (ContextTelemetry, bool, error) {
	snap, err := CodexRuntimeTelemetryFromRollout(rolloutPath)
	if err != nil {
		return ContextTelemetry{}, false, err
	}
	return snap.Context, snap.HasContext, nil
}

// CodexRuntimeTelemetryFromRollout scans one rollout once for both context and
// sub-agent lifecycle state. Harvesting terminal interrupted facts from the
// sub_agent_activity stream makes the dashboard self-heal immediately when
// Codex did not invoke the configured SubagentStop hook.
func CodexRuntimeTelemetryFromRollout(rolloutPath string) (CodexRuntimeSnapshot, error) {
	rc, err := openCodexRollout(rolloutPath)
	if err != nil {
		return CodexRuntimeSnapshot{}, err
	}
	defer func() { _ = rc.Close() }()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCodexRolloutLineBytes)

	var latest *codexTokenCountInfo
	interruptedSubagents := map[string]struct{}{}
	followupCallIDs := map[string]struct{}{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.Type == "response_item" {
			var call codexFunctionCall
			if json.Unmarshal(env.Payload, &call) == nil && call.Type == "function_call" &&
				call.Name == "followup_task" && call.CallID != "" {
				followupCallIDs[call.CallID] = struct{}{}
			}
			continue
		}
		if env.Type != "event_msg" {
			continue
		}
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(env.Payload, &kind) != nil {
			continue
		}
		switch kind.Type {
		case "token_count":
			var ev codexTokenCountEvent
			if json.Unmarshal(env.Payload, &ev) != nil {
				continue
			}
			info := ev.Info
			latest = &info
		case "sub_agent_activity":
			var ev codexSubagentActivityEvent
			if json.Unmarshal(env.Payload, &ev) != nil || ev.AgentThreadID == "" {
				continue
			}
			switch ev.Kind {
			case "started":
				// A previously interrupted child can be resumed under the same
				// thread id. A fresh start is live evidence, so its old terminal
				// fact no longer suppresses the hook-ledger entry.
				delete(interruptedSubagents, ev.AgentThreadID)
			case "interacted":
				// Both followup_task and queue-only send_message emit
				// "interacted". Only followup_task triggers a child turn; recover
				// that distinction through event_id → function-call call_id.
				if _, resumes := followupCallIDs[ev.EventID]; resumes {
					delete(interruptedSubagents, ev.AgentThreadID)
				}
				delete(followupCallIDs, ev.EventID)
			case "interrupted":
				interruptedSubagents[ev.AgentThreadID] = struct{}{}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return CodexRuntimeSnapshot{}, fmt.Errorf("scan codex rollout %s: %w", rolloutPath, err)
	}
	result := CodexRuntimeSnapshot{InterruptedSubagents: interruptedSubagents}
	if latest == nil {
		return result, nil
	}
	snap := contextTelemetryFromTokenCount(*latest)
	// A token_count carrying no actual usage (all-zero last_token_usage —
	// e.g. an event emitted before the first real response) has no occupancy
	// signal. Report it as "nothing to persist" so it can't overwrite a good
	// snapshot with a window-only row: db.UpdateContextSnapshot's all-zero
	// guard would NOT catch that, because WindowSize is non-zero.
	if snap.TokensInput == 0 && snap.TokensOutput == 0 {
		return result, nil
	}
	result.Context = snap
	result.HasContext = true
	return result, nil
}

// contextTelemetryFromTokenCount turns a token_count info block into a
// ContextTelemetry. It uses last_token_usage (this turn), NOT
// total_token_usage: total is cumulative across every turn and would far
// exceed the window, whereas Codex re-sends the entire conversation as each
// turn's input — so last_token_usage.total_tokens (input + output) is
// exactly the number of tokens resident in the window after the last
// response, the right numerator for occupancy. Pct guards an absent window.
func contextTelemetryFromTokenCount(info codexTokenCountInfo) ContextTelemetry {
	u := info.LastTokenUsage
	snap := ContextTelemetry{
		TokensInput:  u.InputTokens,
		TokensOutput: u.OutputTokens,
		WindowSize:   info.ModelContextWindow,
	}
	if info.ModelContextWindow > 0 {
		used := u.TotalTokens
		if used == 0 {
			// total_tokens absent on the event — reconstruct it as
			// input+output. output_tokens already includes
			// reasoning_output_tokens (OpenAI usage semantics) and
			// input_tokens already includes the cached prefix, so this is
			// the full occupancy with nothing dropped or double-counted.
			used = u.InputTokens + u.OutputTokens
		}
		snap.Pct = float64(used) / float64(info.ModelContextWindow) * 100
	}
	return snap
}
