package harness

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// copilotTranscript decodes/encodes Copilot CLI events.jsonl, whose resume
// rebuilds the conversation from user/assistant/tool events. A successful
// session.compaction_complete replaces everything before it with its
// summary.
type copilotTranscript struct{}

// Copilot carries images as attachments whose model-side replay is not part
// of the documented event schema, so the plan references them by path.
func (copilotTranscript) InlineMedia() bool { return false }

// copilotIgnoredEvents are UI, telemetry and lifecycle families that never
// carry model context.
var copilotIgnoredEvents = []string{
	"session.", "hook.", "assistant.turn_", "assistant.usage", "assistant.intent", "assistant.reasoning",
	"assistant.message_delta", "assistant.streaming", "model.", "skill.", "tool.execution_start",
	"tool.execution_partial", "tool.execution_progress", "tool.user_requested", "abort", "subagent.",
	"permission.", "elicitation.", "user_input.", "pending_messages.", "command.", "external_tool.",
	"exit_plan_mode.", "system.notification", "capabilities.", "mcp.",
}

func (copilotTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	t := transcript.New(transcript.Source{Harness: CopilotName, Format: "copilot-events-jsonl", ConvID: sourceID})
	found := false
	harness := 0
	sidechain := 0
	truncated := false
	err := scanCopilotTranscriptRecords(r, t.Losses, func(line int, raw []byte) error {
		var ev struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Data      json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("invalid Copilot event at line %d", line)
		}
		at := parseTime(ev.Timestamp)
		switch ev.Type {
		case "session.start":
			var d struct {
				SessionID     string `json:"sessionId"`
				SelectedModel string `json:"selectedModel"`
				Context       struct {
					Cwd string `json:"cwd"`
				} `json:"context"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if d.SessionID != sourceID {
				return errors.New("copilot history session identity mismatch")
			}
			found = true
			t.Source.Cwd, t.Source.Model = d.Context.Cwd, d.SelectedModel
		case "session.model_change":
			var d struct {
				NewModel string `json:"newModel"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.NewModel != "" {
				t.Source.Model = d.NewModel
			}
		case "session.compaction_complete":
			var d struct {
				Success        bool   `json:"success"`
				SummaryContent string `json:"summaryContent"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Success {
				t.ResetToSummary(d.SummaryContent, at)
				truncated = false
			}
		case "session.context_cleared":
			var d struct {
				InitialMessage string `json:"initialMessage"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("invalid Copilot clear at line %d", line)
			}
			t.Losses.Add(transcript.LossClearedHistory, len(t.Entries), "")
			t.Entries = nil
			truncated = false
			if d.InitialMessage != "" {
				t.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at, Blocks: []transcript.Block{transcript.Text(d.InitialMessage)}})
			}
		case "session.truncation":
			// This event records statistics, not the identities of retained
			// messages. Guessing from counts can resurrect discarded context.
			truncated = true
		case "user.message":
			var d struct {
				Content          string `json:"content"`
				ParentAgentTask  string `json:"parentAgentTaskId"`
				AgentModeSubtask bool   `json:"isSubagent"`
				Attachments      []struct {
					Type        string `json:"type"`
					Path        string `json:"path"`
					DisplayName string `json:"displayName"`
					MimeType    string `json:"mimeType"`
					Data        string `json:"data"`
				} `json:"attachments"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("invalid Copilot user message at line %d", line)
			}
			e := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at}
			if d.Content != "" {
				e.Blocks = append(e.Blocks, transcript.Text(d.Content))
			}
			for _, a := range d.Attachments {
				m := transcript.Media{MediaType: a.MimeType, Name: a.DisplayName}
				switch a.Type {
				case "file":
					m.Path = a.Path
				case "blob":
					data, err := base64.StdEncoding.DecodeString(a.Data)
					if err != nil || len(data) == 0 {
						t.Losses.Add(transcript.LossMediaDropped, 1, "")
						continue
					}
					m.Data = data
				default:
					t.Losses.Add(transcript.LossNotCarried, 1, "copilot "+a.Type+" attachment")
					continue
				}
				e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{m}})
			}
			t.Append(e)
		case "assistant.message":
			var d struct {
				Content       string `json:"content"`
				Model         string `json:"model"`
				ReasoningText string `json:"reasoningText"`
				Opaque        string `json:"reasoningOpaque"`
				Encrypted     string `json:"encryptedContent"`
				ParentToolID  string `json:"parentToolCallId"`
				ToolRequests  []struct {
					ToolCallID string          `json:"toolCallId"`
					Name       string          `json:"name"`
					Arguments  json.RawMessage `json:"arguments"`
				} `json:"toolRequests"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("invalid Copilot assistant message at line %d", line)
			}
			if d.ParentToolID != "" {
				sidechain++
				return nil
			}
			e := transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: at, Model: d.Model}
			if d.ReasoningText != "" || d.Opaque != "" || d.Encrypted != "" {
				opaque := d.Opaque
				if opaque == "" {
					opaque = d.Encrypted
				}
				e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockThinking, Text: d.ReasoningText, Opaque: &transcript.Opaque{Vendor: "copilot", Data: opaque}})
			}
			if d.Content != "" {
				e.Blocks = append(e.Blocks, transcript.Text(d.Content))
			}
			for _, tr := range d.ToolRequests {
				input := tr.Arguments
				b := transcript.Block{Kind: transcript.BlockToolCall, CallID: tr.ToolCallID, Name: tr.Name, Input: input}
				var s string
				if json.Unmarshal(input, &s) == nil {
					b.Input, b.Freeform, b.Text = nil, true, s
				}
				e.Blocks = append(e.Blocks, b)
			}
			if d.Model != "" {
				t.Source.Model = d.Model
			}
			t.Append(e)
		case "tool.execution_complete":
			var d struct {
				ToolCallID   string `json:"toolCallId"`
				Success      bool   `json:"success"`
				ParentToolID string `json:"parentToolCallId"`
				Result       *struct {
					Content string `json:"content"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("invalid Copilot tool result at line %d", line)
			}
			if d.ParentToolID != "" {
				sidechain++
				return nil
			}
			b := transcript.Block{Kind: transcript.BlockToolResult, CallID: d.ToolCallID, IsError: !d.Success}
			if d.Result != nil {
				b.Text = d.Result.Content
			}
			if d.Error != nil && d.Error.Message != "" {
				b.Text = strings.TrimSpace(b.Text + "\n" + d.Error.Message)
			}
			t.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at, Blocks: []transcript.Block{b}})
		case "system.message":
			harness++
		default:
			for _, p := range copilotIgnoredEvents {
				if strings.HasPrefix(ev.Type, p) {
					return nil
				}
			}
			t.Losses.Add(transcript.LossUnknownRecord, 1, "copilot:"+ev.Type)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("copilot truncation does not identify retained messages; use the agent-written handoff")
	}
	if !found {
		return nil, errors.New("copilot history has no matching session.start")
	}
	t.Losses.Add(transcript.LossHarnessContext, harness, "")
	t.Losses.Add(transcript.LossSidechain, sidechain, "")
	return t, nil
}

// copilotTranscriptVersion is the Copilot CLI event schema the encoder
// follows (session.start.copilotVersion is required).
const copilotTranscriptVersion = "1.0.95"

func (copilotTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := uuid.NewString()
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	parent := ""
	emit := func(at int, kind string, data map[string]any) {
		ev := map[string]any{"type": kind, "data": data, "id": uuid.NewString(), "timestamp": rfc3339ms(times[at])}
		if parent != "" {
			ev["parentId"] = parent
		} else {
			ev["parentId"] = nil
		}
		parent = ev["id"].(string)
		out.write(ev)
	}
	emit(0, "session.start", map[string]any{
		"sessionId": id, "version": 1, "producer": "copilot-agent", "copilotVersion": copilotTranscriptVersion,
		"startTime": rfc3339ms(times[0]), "context": map[string]string{"cwd": cwd},
	})
	turn := 0
	for i, e := range p.Entries {
		text := planText(e)
		if e.Role == transcript.RoleUser {
			emit(i, "user.message", map[string]any{"content": text, "attachments": []any{}})
			continue
		}
		turnID := fmt.Sprint(turn)
		turn++
		emit(i, "assistant.turn_start", map[string]any{"turnId": turnID})
		emit(i, "assistant.message", map[string]any{"messageId": uuid.NewString(), "content": text, "toolRequests": []any{}, "turnId": turnID})
		emit(i, "assistant.turn_end", map[string]any{"turnId": turnID})
	}
	return id, out.err
}
