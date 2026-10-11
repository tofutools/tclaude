package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// codexTranscript decodes/encodes Codex rollout JSONL. The model context is
// the response_item stream; event_msg records duplicate it for the UI. A
// `compacted` record replaces everything before it with its
// replacement_history (or, in older rollouts, a plain summary message).
type codexTranscript struct{}

func (codexTranscript) InlineMedia() bool { return true }

type codexItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	ID        string          `json:"id"`
	Content   json.RawMessage `json:"content"`
	Summary   []codexContent  `json:"summary"`
	Encrypted string          `json:"encrypted_content"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Author    string          `json:"author"`
	Status    string          `json:"status"`
}

type codexContent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ImageURL  string `json:"image_url"`
	Encrypted string `json:"encrypted_content"`
}

var codexHarnessPrefixes = []string{
	"<environment_context>", "# AGENTS.md instructions", "<user_instructions>", "<permissions instructions>",
	"<collaboration_mode>", "<turn_aborted>", "<model_switch>", "<skills_instructions>",
}

func codexHarnessText(s string) bool {
	s = strings.TrimSpace(s)
	for _, p := range codexHarnessPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (codexTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	t := transcript.New(transcript.Source{Harness: CodexName, Format: "codex-rollout-jsonl", ConvID: sourceID})
	harness := 0
	found := false
	err := scanCodexTranscriptRecords(r, t.Losses, func(line int, raw []byte) error {
		var env codexEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("invalid Codex rollout JSON at line %d", line)
		}
		at := parseTime(env.Timestamp)
		switch env.Type {
		case "session_meta":
			var meta struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			_ = json.Unmarshal(env.Payload, &meta)
			if meta.ID == sourceID {
				found = true
				t.Source.Cwd = meta.Cwd
			}
		case "turn_context":
			var tc struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(env.Payload, &tc) == nil && tc.Model != "" {
				t.Source.Model = tc.Model
			}
		case "response_item":
			var item codexItem
			if err := json.Unmarshal(env.Payload, &item); err != nil {
				return fmt.Errorf("invalid Codex response item at line %d", line)
			}
			harness += codexDecodeItem(t, item, at)
		case "compacted":
			var c struct {
				Message     string      `json:"message"`
				Replacement []codexItem `json:"replacement_history"`
			}
			if err := json.Unmarshal(env.Payload, &c); err != nil {
				return fmt.Errorf("invalid Codex compaction at line %d", line)
			}
			if len(c.Replacement) == 0 {
				t.ResetToSummary(c.Message, at)
				return nil
			}
			// A local compaction's replacement history already ends with
			// the summary as a user message: mark it rather than adding it
			// a second time.
			t.ResetToSummary("", at)
			for _, item := range c.Replacement {
				harness += codexDecodeItem(t, item, at)
			}
			codexMarkSummary(t, c.Message)
		case "event_msg", "token_usage_record", "world_state", "inter_agent_communication_metadata":
		default:
			t.Losses.Add(transcript.LossUnknownRecord, 1, "codex:"+env.Type)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("codex rollout has no matching session_meta")
	}
	t.Losses.Add(transcript.LossHarnessContext, harness, "")
	return t, nil
}

// codexDecodeItem appends one response item; it returns how many harness
// scaffolding items it dropped.
func codexDecodeItem(t *transcript.Transcript, item codexItem, at time.Time) int {
	agent := func(blocks ...transcript.Block) {
		t.Append(transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: at, Model: t.Source.Model, Blocks: blocks})
	}
	tool := func(blocks ...transcript.Block) {
		t.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at, Blocks: blocks})
	}
	name := item.Name
	if item.Namespace != "" && name != "" {
		name = item.Namespace + "." + name
	}
	switch item.Type {
	case "message":
		var content []codexContent
		_ = json.Unmarshal(item.Content, &content)
		switch item.Role {
		case "user":
			e := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at}
			dropped := 0
			for _, c := range content {
				switch c.Type {
				case "input_text":
					if codexHarnessText(c.Text) {
						dropped++
						continue
					}
					e.Blocks = append(e.Blocks, transcript.Text(c.Text))
				case "input_image":
					e.Blocks = append(e.Blocks, codexImage(c.ImageURL))
				}
			}
			t.Append(e)
			return dropped
		case "assistant":
			var blocks []transcript.Block
			for _, c := range content {
				if c.Type == "output_text" || c.Type == "refusal" {
					blocks = append(blocks, transcript.Text(c.Text))
				}
			}
			agent(blocks...)
		default:
			return 1
		}
	case "reasoning":
		var texts []string
		for _, s := range item.Summary {
			texts = append(texts, s.Text)
		}
		agent(transcript.Block{Kind: transcript.BlockThinking, Text: strings.Join(texts, "\n"), Opaque: &transcript.Opaque{Vendor: "openai", Data: item.Encrypted}})
	case "function_call":
		b := transcript.Block{Kind: transcript.BlockToolCall, CallID: item.CallID, Name: name}
		if json.Valid([]byte(item.Arguments)) {
			b.Input = json.RawMessage(item.Arguments)
		} else {
			b.Freeform, b.Text = true, item.Arguments
		}
		agent(b)
	case "custom_tool_call":
		agent(transcript.Block{Kind: transcript.BlockToolCall, CallID: item.CallID, Name: name, Freeform: true, Text: item.Input})
	case "local_shell_call":
		agent(transcript.Block{Kind: transcript.BlockToolCall, CallID: item.CallID, Name: "local_shell", Input: item.Action})
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		tool(codexToolOutput(item))
	case "web_search_call":
		id := item.ID
		if id == "" {
			id = uuid.NewString()
		}
		agent(transcript.Block{Kind: transcript.BlockToolCall, CallID: id, Name: "web_search", Input: item.Action})
		tool(transcript.Block{Kind: transcript.BlockToolResult, CallID: id, Name: "web_search", Text: "(web search results are not recorded in the source history)"})
	case "agent_message":
		var content []codexContent
		_ = json.Unmarshal(item.Content, &content)
		e := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at, Author: item.Author}
		for _, c := range content {
			switch c.Type {
			case "input_text", "output_text":
				e.Blocks = append(e.Blocks, transcript.Text(c.Text))
			case "encrypted_content":
				t.Losses.Add(transcript.LossNotCarried, 1, "encrypted sub-agent message payload")
			}
		}
		t.Append(e)
	case "compaction":
		t.Losses.Add(transcript.LossEncryptedSummary, 1, "")
	case "ghost_snapshot":
	default:
		t.Losses.Add(transcript.LossUnknownRecord, 1, "codex-item:"+item.Type)
	}
	return 0
}

// codexMarkSummary turns the replacement history's summary turn into a
// summary block, or adds the summary first when the history lacks it.
func codexMarkSummary(t *transcript.Transcript, summary string) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}
	for i := range t.Entries {
		e := &t.Entries[i]
		if e.Role != transcript.RoleUser || e.Origin != transcript.OriginHuman {
			continue
		}
		for j := range e.Blocks {
			if e.Blocks[j].Kind == transcript.BlockText && strings.Contains(e.Blocks[j].Text, summary) {
				e.Blocks[j] = transcript.Block{Kind: transcript.BlockSummary, Text: e.Blocks[j].Text}
				return
			}
		}
	}
	t.Entries = append([]transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{{Kind: transcript.BlockSummary, Text: summary}}}}, t.Entries...)
}

func codexImage(url string) transcript.Block {
	if mediaType, data, ok := dataURL(url); ok {
		return transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{{MediaType: mediaType, Data: data}}}
	}
	return transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{{URL: url}}}
}

func codexToolOutput(item codexItem) transcript.Block {
	b := transcript.Block{Kind: transcript.BlockToolResult, CallID: item.CallID}
	var s string
	if json.Unmarshal(item.Output, &s) == nil {
		b.Text = s
		return b
	}
	var parts []codexContent
	if json.Unmarshal(item.Output, &parts) == nil {
		var texts []string
		for _, p := range parts {
			switch p.Type {
			case "input_text", "output_text":
				texts = append(texts, p.Text)
			case "input_image":
				img := codexImage(p.ImageURL)
				b.Media = append(b.Media, img.Media...)
			}
		}
		b.Text = strings.Join(texts, "\n")
		return b
	}
	// Older rollouts: {"content": "...", "success": bool}.
	var legacy struct {
		Content string `json:"content"`
		Success *bool  `json:"success"`
	}
	if json.Unmarshal(item.Output, &legacy) == nil && legacy.Content != "" {
		b.Text = legacy.Content
		b.IsError = legacy.Success != nil && !*legacy.Success
		return b
	}
	b.Text = string(item.Output)
	return b
}

// codexTranscriptCLIVersion is stamped into session_meta.cli_version, a
// required field; it names the rollout schema version the encoder follows.
const codexTranscriptCLIVersion = "0.139.0"

func (codexTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := uuid.NewString()
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	start := rfc3339ms(times[0])
	out.write(map[string]any{"timestamp": start, "type": "session_meta", "payload": map[string]any{
		"id": id, "timestamp": start, "cwd": cwd, "originator": "tclaude_morph",
		"cli_version": codexTranscriptCLIVersion, "source": "cli",
	}})
	for i, e := range p.Entries {
		ts := rfc3339ms(times[i])
		text := planText(e)
		if e.Role == transcript.RoleAssistant {
			out.write(map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{
				"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}}})
			out.write(map[string]any{"timestamp": ts, "type": "event_msg", "payload": map[string]any{"type": "agent_message", "message": text}})
			continue
		}
		var content []map[string]string
		var images []string
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockText:
				content = append(content, map[string]string{"type": "input_text", "text": b.Text})
			case transcript.BlockMedia:
				for _, m := range b.Media {
					url := toDataURL(m)
					images = append(images, url)
					content = append(content, map[string]string{"type": "input_image", "image_url": url})
				}
			}
		}
		out.write(map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user", "content": content}})
		if e.Origin == transcript.OriginTool {
			// Carried tool results are model context, not something the
			// operator typed: keep them out of the UI replay stream.
			continue
		}
		if images == nil {
			images = []string{}
		}
		out.write(map[string]any{"timestamp": ts, "type": "event_msg", "payload": map[string]any{
			"type": "user_message", "message": text, "images": images}})
	}
	return id, out.err
}
