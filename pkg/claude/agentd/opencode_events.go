package agentd

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// openCodeEventProjector translates OpenCode's directory-wide SSE vocabulary
// into the same hook events every other harness uses. It retains only enough
// state to suppress OpenCode's repeated busy and running-tool snapshots.
type openCodeEventProjector struct {
	convID            string
	cwd               string
	lastSessionStatus string
	activeToolStates  map[string]string
	deferredIdle      bool
	pendingAttention  bool
	seenEventIDs      map[string]struct{}
	seenEventOrder    []string
}

type openCodeSessionStatus struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
}

type openCodePermissionRequest struct {
	ID         string `json:"id,omitempty"`
	SessionID  string `json:"sessionID"`
	Permission string `json:"permission,omitempty"`
	Action     string `json:"action,omitempty"`
}

type openCodeQuestionRequest struct {
	ID        string `json:"id,omitempty"`
	SessionID string `json:"sessionID"`
	Questions []struct {
		Question string `json:"question"`
	} `json:"questions"`
}

type openCodeEventEnvelope struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Properties struct {
		ID         string                   `json:"id,omitempty"`
		SessionID  string                   `json:"sessionID"`
		Status     openCodeSessionStatus    `json:"status"`
		Permission string                   `json:"permission,omitempty"`
		Action     string                   `json:"action,omitempty"`
		Questions  []openCodeQuestionPrompt `json:"questions,omitempty"`
		Part       openCodeMessagePart      `json:"part"`
		Error      openCodeSessionError     `json:"error"`
	} `json:"properties"`
}

type openCodeQuestionPrompt struct {
	Question string `json:"question"`
}

type openCodeMessagePart struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Tool   string          `json:"tool"`
	CallID string          `json:"callID"`
	State  json.RawMessage `json:"state"`
}

type openCodeToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Error  string          `json:"error,omitempty"`
}

type openCodeSessionError struct {
	Name string `json:"name"`
	Data struct {
		Message     string `json:"message,omitempty"`
		StatusCode  int    `json:"statusCode,omitempty"`
		IsRetryable bool   `json:"isRetryable,omitempty"`
	} `json:"data"`
}

func newOpenCodeEventProjector(convID, cwd string) *openCodeEventProjector {
	return &openCodeEventProjector{
		convID:           convID,
		cwd:              cwd,
		activeToolStates: make(map[string]string),
		seenEventIDs:     make(map[string]struct{}),
	}
}

func (p *openCodeEventProjector) project(event json.RawMessage) ([]session.HookCallbackInput, error) {
	var envelope openCodeEventEnvelope
	if err := json.Unmarshal(event, &envelope); err != nil {
		return nil, fmt.Errorf("decode OpenCode event: %w", err)
	}
	if envelope.Type == "" {
		return nil, nil
	}
	if p.convID == "" || envelope.Properties.SessionID == "" ||
		envelope.Properties.SessionID != p.convID {
		// /event is directory-scoped, not session-scoped. A user's OpenCode
		// store can contain several conversations for the same worktree.
		return nil, nil
	}
	if p.seenEvent(envelope.ID) {
		return nil, nil
	}

	switch envelope.Type {
	case "session.created":
		return []session.HookCallbackInput{p.hook("SessionStart")}, nil
	case "session.status":
		return p.projectStatus(envelope.Properties.Status, false), nil
	case "session.idle":
		return p.projectStatus(openCodeSessionStatus{Type: "idle"}, false), nil
	case "message.part.updated":
		return p.projectToolPart(envelope.Properties.Part)
	case "question.asked", "question.v2.asked":
		return p.projectQuestion(openCodeQuestionRequest{
			ID:        envelope.Properties.ID,
			SessionID: p.convID,
			Questions: questionPrompts(envelope.Properties.Questions),
		}), nil
	case "question.replied", "question.rejected",
		"question.v2.replied", "question.v2.rejected",
		"permission.replied", "permission.v2.replied":
		// The human interaction is resolved and OpenCode resumes (or rejects)
		// the suspended tool call. Force the generic working transition even
		// though the last server status is normally still "busy".
		p.pendingAttention = false
		return p.projectStatus(openCodeSessionStatus{Type: "busy"}, true), nil
	case "permission.asked", "permission.v2.asked":
		return p.projectPermission(openCodePermissionRequest{
			ID:         envelope.Properties.ID,
			SessionID:  p.convID,
			Permission: envelope.Properties.Permission,
			Action:     envelope.Properties.Action,
		}), nil
	case "session.error":
		clear(p.activeToolStates)
		p.deferredIdle = false
		p.pendingAttention = false
		input := p.hook("StopFailure")
		input.ErrorType = openCodeHookErrorType(envelope.Properties.Error)
		input.ErrorMessage = boundedOpenCodeDetail(envelope.Properties.Error.Data.Message)
		return []session.HookCallbackInput{input}, nil
	default:
		// session.deleted is deliberately among the unmapped events. It
		// deletes conversation data but does not end the attached TUI or
		// managed server process, so treating it as SessionEnd would invent a
		// process exit. OpenCode process exit remains reaper-authoritative,
		// with a blank exit reason like Codex.
		return nil, nil
	}
}

func (p *openCodeEventProjector) projectStatus(status openCodeSessionStatus, force bool) []session.HookCallbackInput {
	switch status.Type {
	case "busy", "retry":
		if p.pendingAttention && !force {
			// OpenCode reports a session blocked on a permission/question as
			// busy. The richer attention event/snapshot remains authoritative
			// until a reply or a real turn boundary clears it.
			return nil
		}
		if p.deferredIdle {
			// A new turn started without a terminal event for a tool from the
			// prior turn (observed on abort-capable event streams). Bound the
			// deferral: close the prior turn, retire its stale calls, then
			// announce this fresh turn.
			clear(p.activeToolStates)
			p.deferredIdle = false
			p.lastSessionStatus = status.Type
			return []session.HookCallbackInput{
				p.hook("Stop"),
				p.hook("UserPromptSubmit"),
			}
		}
		if !force && p.lastSessionStatus == status.Type {
			return nil
		}
		p.lastSessionStatus = status.Type
		p.deferredIdle = false
		// UserPromptSubmit is the shared model's generic "a turn is active"
		// transition. OpenCode can emit several busy snapshots during a turn;
		// the projector suppresses repeats while tool events provide detail.
		return []session.HookCallbackInput{p.hook("UserPromptSubmit")}
	case "idle":
		p.pendingAttention = false
		if len(p.activeToolStates) > 0 {
			// The stream is ordered in OpenCode 1.18.4, but do not trust an
			// early/missed idle across reconnect to declare the turn complete
			// while a tool call we saw remains open. A terminal tool event
			// normally drains this deferred Stop. A second idle assertion is
			// the bounded fallback for aborts that omit a terminal tool event.
			if p.deferredIdle {
				clear(p.activeToolStates)
				p.deferredIdle = false
				p.lastSessionStatus = "idle"
				return []session.HookCallbackInput{p.hook("Stop")}
			}
			p.deferredIdle = true
			p.lastSessionStatus = "idle"
			return nil
		}
		if !force && p.lastSessionStatus == "idle" {
			return nil
		}
		p.lastSessionStatus = "idle"
		p.deferredIdle = false
		return []session.HookCallbackInput{p.hook("Stop")}
	default:
		return nil
	}
}

func (p *openCodeEventProjector) projectToolPart(part openCodeMessagePart) ([]session.HookCallbackInput, error) {
	if part.Type != "tool" || len(part.State) == 0 {
		return nil, nil
	}
	var state openCodeToolState
	if err := json.Unmarshal(part.State, &state); err != nil {
		return nil, fmt.Errorf("decode OpenCode tool state: %w", err)
	}
	key := part.CallID
	if key == "" {
		key = part.ID
	}
	switch state.Status {
	case "pending":
		if key != "" {
			p.activeToolStates[key] = state.Status
		}
		return nil, nil
	case "running":
		if key != "" && p.activeToolStates[key] == state.Status {
			return nil, nil
		}
		if key != "" {
			p.activeToolStates[key] = state.Status
		}
		input := p.hook("PreToolUse")
		input.ToolName = openCodeHookToolName(part.Tool)
		input.ToolInput = normalizeOpenCodeToolInput(input.ToolName, state.Input)
		return []session.HookCallbackInput{input}, nil
	case "completed", "error":
		if key != "" {
			delete(p.activeToolStates, key)
		}
		eventName := "PostToolUse"
		if state.Status == "error" {
			eventName = "PostToolUseFailure"
		}
		input := p.hook(eventName)
		input.ToolName = openCodeHookToolName(part.Tool)
		input.ToolInput = normalizeOpenCodeToolInput(input.ToolName, state.Input)
		projected := []session.HookCallbackInput{input}
		if p.deferredIdle && len(p.activeToolStates) == 0 {
			p.deferredIdle = false
			p.lastSessionStatus = "idle"
			projected = append(projected, p.hook("Stop"))
		}
		return projected, nil
	default:
		return nil, nil
	}
}

func (p *openCodeEventProjector) projectPermission(request openCodePermissionRequest) []session.HookCallbackInput {
	if p.convID == "" || request.SessionID == "" || request.SessionID != p.convID {
		return nil
	}
	detail := request.Permission
	if detail == "" {
		detail = request.Action
	}
	input := p.hook("PermissionRequest")
	input.ToolName = boundedOpenCodeDetail(detail)
	p.pendingAttention = true
	return []session.HookCallbackInput{input}
}

func (p *openCodeEventProjector) projectQuestion(request openCodeQuestionRequest) []session.HookCallbackInput {
	if p.convID == "" || request.SessionID == "" || request.SessionID != p.convID {
		return nil
	}
	var detail string
	if len(request.Questions) > 0 {
		detail = request.Questions[0].Question
	}
	input := p.hook("Notification")
	input.NotificationType = "elicitation_dialog"
	input.Message = boundedOpenCodeDetail(detail)
	p.pendingAttention = true
	return []session.HookCallbackInput{input}
}

func (p *openCodeEventProjector) seenEvent(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := p.seenEventIDs[id]; ok {
		return true
	}
	const maxSeenEvents = 2048
	p.seenEventIDs[id] = struct{}{}
	p.seenEventOrder = append(p.seenEventOrder, id)
	if len(p.seenEventOrder) > maxSeenEvents {
		evicted := p.seenEventOrder[0]
		p.seenEventOrder = p.seenEventOrder[1:]
		delete(p.seenEventIDs, evicted)
	}
	return false
}

func (p *openCodeEventProjector) resetToolsForSnapshot() {
	// A reconnect has no event replay cursor. Tool completions may have
	// happened while disconnected, so pre-disconnect call IDs cannot remain
	// authoritative after a complete status/attention snapshot. Events
	// buffered after the new stream opened rebuild active calls before any
	// subsequent idle boundary is consumed.
	clear(p.activeToolStates)
	p.deferredIdle = false
}

func (p *openCodeEventProjector) hook(eventName string) session.HookCallbackInput {
	input := session.HookCallbackInput{
		ConvID:        p.convID,
		Cwd:           p.cwd,
		HookEventName: eventName,
	}
	if eventName == "SessionStart" {
		input.Source = "startup"
	}
	return input
}

func questionPrompts(in []openCodeQuestionPrompt) []struct {
	Question string `json:"question"`
} {
	out := make([]struct {
		Question string `json:"question"`
	}, len(in))
	for i := range in {
		out[i].Question = in[i].Question
	}
	return out
}

func openCodeHookToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Tool"
	}
	first, size := utf8.DecodeRuneInString(name)
	return strings.ToUpper(string(first)) + name[size:]
}

func normalizeOpenCodeToolInput(toolName string, input json.RawMessage) json.RawMessage {
	if (toolName != "Edit" && toolName != "Write") || len(input) == 0 {
		return input
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil || len(fields["file_path"]) != 0 {
		return input
	}
	if filePath := fields["filePath"]; len(filePath) != 0 {
		fields["file_path"] = filePath
		if normalized, err := json.Marshal(fields); err == nil {
			return normalized
		}
	}
	return input
}

func openCodeHookErrorType(openCodeErr openCodeSessionError) string {
	switch openCodeErr.Name {
	case "ProviderAuthError":
		return "authentication_failed"
	case "MessageOutputLengthError":
		return "max_output_tokens"
	case "ContextOverflowError", "ContentFilterError", "StructuredOutputError":
		return "invalid_request"
	case "APIError":
		switch {
		case openCodeErr.Data.StatusCode == 429:
			return "rate_limit"
		case openCodeErr.Data.StatusCode == 402:
			return "billing_error"
		case openCodeErr.Data.IsRetryable || openCodeErr.Data.StatusCode >= 500:
			return "server_error"
		case openCodeErr.Data.StatusCode >= 400:
			return "invalid_request"
		}
	}
	return "unknown"
}

func boundedOpenCodeDetail(value string) string {
	const maxRunes = 240
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes-1]) + "…"
}
