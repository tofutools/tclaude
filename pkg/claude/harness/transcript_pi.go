package harness

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// piTranscript decodes and encodes pi session files (see pi_session.go). A
// decode carries what pi would send the model on resume: the current branch,
// from the newest compaction on, with context edits applied; the encode writes
// a linear branch pi resumes as is.
type piTranscript struct{}

func (piTranscript) InlineMedia() bool { return true }

// pi's own framing of the summary messages it derives (core/messages.js).
const (
	piBranchSummaryPrefix = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	piBranchSummarySuffix = "</summary>"
)

func (piTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	session, err := decodePiSession(r)
	if err != nil {
		return nil, err
	}
	if session.Header.ID != sourceID {
		return nil, errors.New("pi history has no matching session header")
	}
	t := transcript.New(transcript.Source{
		Harness: PiName, Format: piHistory{}.Format(), ConvID: sourceID,
		Cwd: session.Header.Cwd, Title: session.Name(),
	})

	onBranch := map[string]bool{}
	for _, e := range session.Branch() {
		onBranch[e.ID] = true
	}
	abandoned, harnessContext := 0, 0
	for _, e := range session.Entries {
		switch {
		case !onBranch[e.ID] && e.Type == "message":
			abandoned++
		case onBranch[e.ID] && e.Type == "message" && e.Message != nil && e.Message.Role == "system":
			// pi's system prompt and later instructions: the target
			// harness brings its own.
			harnessContext++
		}
	}
	t.Losses.Add(transcript.LossInactiveBranch, abandoned, "")
	t.Losses.Add(transcript.LossHarnessContext, harnessContext, "")

	entries, compacted := session.Context()
	t.Losses.Add(transcript.LossPreCompaction, compacted, "")
	edits := map[string]json.RawMessage{}
	for _, e := range entries {
		if e.Type == "context_edit" && e.TargetID != "" {
			edits[e.TargetID] = e.Replacement
		}
	}
	for i, e := range entries {
		at := parseTime(e.Timestamp)
		switch e.Type {
		case "compaction":
			if i > 0 {
				// An older compaction kept inside the newest one's range
				// contributes nothing, as in pi.
				continue
			}
			t.Append(transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginSystem, Time: at,
				Blocks: []transcript.Block{{Kind: transcript.BlockSummary, Text: e.Summary}}})
		case "branch_summary":
			if e.Summary != "" {
				t.Append(transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginSystem, Time: at,
					Blocks: []transcript.Block{transcript.Text(piBranchSummaryPrefix + e.Summary + piBranchSummarySuffix)}})
			}
		case "custom_message":
			content, keep := piEditedContent(e.Content, edits, e.ID)
			if keep {
				t.Append(transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginSystem, Time: at,
					Blocks: piInputBlocks(content)})
			}
		case "message":
			if e.Message == nil {
				continue
			}
			if e.Message.Role == "system" {
				continue
			}
			content, keep := piEditedContent(e.Message.Content, edits, e.ID)
			if !keep {
				continue
			}
			if entry, ok := piDecodeMessage(t, e, content); ok {
				t.Append(entry)
			}
		case "model_change", "thinking_level_change", "session_info", "label", "custom", "usage", "context_edit":
			// Session metadata, not conversation.
		default:
			t.Losses.Add(transcript.LossUnknownRecord, 1, "pi:"+e.Type)
		}
	}
	return t, nil
}

// piEditedContent applies a context_edit: a replacement's content stands in
// for the entry's, and a null replacement drops the entry.
func piEditedContent(content json.RawMessage, edits map[string]json.RawMessage, id string) (json.RawMessage, bool) {
	edit, ok := edits[id]
	if !ok {
		return content, true
	}
	if len(edit) == 0 || string(edit) == "null" {
		return nil, false
	}
	var replacement struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(edit, &replacement) != nil {
		return content, true
	}
	return replacement.Content, true
}

// piDecodeMessage maps one message onto a transcript entry.
func piDecodeMessage(t *transcript.Transcript, e *piEntry, content json.RawMessage) (transcript.Entry, bool) {
	m := e.Message
	at := parseTime(e.Timestamp)
	if at.IsZero() {
		at = msTime(m.Timestamp)
	}
	switch m.Role {
	case "user":
		return transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at,
			Blocks: piInputBlocks(content)}, true
	case "assistant":
		// pi never replays an errored or aborted response (pi-ai
		// transformMessages), so neither does a translation.
		if m.StopReason == "error" || m.StopReason == "aborted" {
			return transcript.Entry{}, false
		}
		model := piModelRef(m.Provider, m.Model)
		if model != "" {
			t.Source.Model = model
		}
		entry := transcript.Entry{ID: e.ID, Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: at, Model: model}
		for _, b := range piBlocks(content) {
			switch b.Type {
			case "text":
				if b.Text != "" {
					entry.Blocks = append(entry.Blocks, transcript.Text(b.Text))
				}
			case "thinking":
				block := transcript.Block{Kind: transcript.BlockThinking, Text: b.Thinking}
				if b.ThinkingSignature != "" {
					block.Opaque = &transcript.Opaque{Vendor: PiName, Data: b.ThinkingSignature}
				}
				if block.Text != "" || block.Opaque != nil {
					entry.Blocks = append(entry.Blocks, block)
				}
			case "toolCall":
				entry.Blocks = append(entry.Blocks, transcript.Block{Kind: transcript.BlockToolCall, CallID: b.ID, Name: b.Name, Input: b.Arguments})
			case "image":
				if media, ok := piMedia(b); ok {
					entry.Blocks = append(entry.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{media}})
				}
			}
		}
		return entry, true
	case "toolResult":
		result := transcript.Block{Kind: transcript.BlockToolResult, CallID: m.ToolCallID, Name: m.ToolName, IsError: m.IsError}
		var texts []string
		for _, b := range piBlocks(content) {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "image":
				if media, ok := piMedia(b); ok {
					result.Media = append(result.Media, media)
				}
			}
		}
		result.Text = strings.Join(texts, "\n")
		return transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at,
			Blocks: []transcript.Block{result}}, true
	case "bashExecution":
		// A `!command` the operator ran; `!!` keeps it out of context.
		if m.ExcludeFromContext {
			return transcript.Entry{}, false
		}
		return transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at,
			Blocks: []transcript.Block{transcript.Text(piBashExecutionText(m))}}, true
	case "custom":
		return transcript.Entry{ID: e.ID, Role: transcript.RoleUser, Origin: transcript.OriginSystem, Time: at,
			Blocks: piInputBlocks(content)}, true
	default:
		t.Losses.Add(transcript.LossUnknownRecord, 1, "pi:"+m.Role)
		return transcript.Entry{}, false
	}
}

// piInputBlocks maps user-side content: text and images.
func piInputBlocks(content json.RawMessage) []transcript.Block {
	var blocks []transcript.Block
	for _, b := range piBlocks(content) {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, transcript.Text(b.Text))
			}
		case "image":
			if media, ok := piMedia(b); ok {
				blocks = append(blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{media}})
			}
		}
	}
	return blocks
}

func piMedia(b piBlock) (transcript.Media, bool) {
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil || len(data) == 0 {
		return transcript.Media{}, false
	}
	return transcript.Media{MediaType: b.MimeType, Data: data}, true
}

// piBashExecutionText is the text pi sends the model for a `!command`
// (core/messages.js bashExecutionToText).
func piBashExecutionText(m *piMessage) string {
	text := "Ran `" + m.Command + "`\n"
	if m.Output != "" {
		text += "```\n" + m.Output + "\n```"
	} else {
		text += "(no output)"
	}
	if m.Cancelled {
		text += "\n\n(command cancelled)"
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		text += "\n\nCommand exited with code " + strconv.Itoa(*m.ExitCode)
	}
	if m.Truncated && m.FullOutputPath != "" {
		text += "\n\n[Output truncated. Full output: " + m.FullOutputPath + "]"
	}
	return text
}

// Encode writes a plan as a new linear session: the header, the target model
// when the plan names one as `provider/model`, the session name, and one
// message per entry. Assistant messages carry zero usage, so pi's footer and
// cost totals count only what the new session spends.
func (piTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := uuid.NewString()
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	taken := map[string]*piEntry{}
	var parent any
	entry := func(typ string, at int, fields map[string]any) {
		eid := piNewEntryID(taken)
		taken[eid] = nil
		record := map[string]any{"type": typ, "id": eid, "parentId": parent, "timestamp": rfc3339ms(times[at])}
		for k, v := range fields {
			record[k] = v
		}
		out.write(record)
		parent = eid
	}
	out.write(map[string]any{"type": piSessionHeaderType, "version": piSessionVersion, "id": id,
		"timestamp": rfc3339ms(times[0]), "cwd": cwd})
	provider, model, routed := strings.Cut(p.TargetModel, "/")
	if !routed {
		provider, model = "", p.TargetModel
	}
	if provider != "" && model != "" {
		entry("model_change", 0, map[string]any{"provider": provider, "modelId": model})
	}
	for i, e := range p.Entries {
		var content []map[string]any
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockText:
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			case transcript.BlockMedia:
				if e.Role == transcript.RoleAssistant {
					continue
				}
				for _, m := range b.Media {
					if len(m.Data) > 0 {
						content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(m.Data), "mimeType": m.MediaType})
					}
				}
			}
		}
		if len(content) == 0 {
			continue
		}
		ms := times[i].UnixMilli()
		message := map[string]any{"role": "user", "content": content, "timestamp": ms}
		if e.Role == transcript.RoleAssistant {
			message = map[string]any{
				"role": "assistant", "content": content, "provider": provider, "model": model,
				"usage": map[string]any{
					"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
					"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
				},
				"stopReason": "stop", "timestamp": ms,
			}
		}
		entry("message", i, map[string]any{"message": message})
	}
	if p.Title != "" {
		entry("session_info", len(times)-1, map[string]any{"name": p.Title})
	}
	return id, out.err
}
