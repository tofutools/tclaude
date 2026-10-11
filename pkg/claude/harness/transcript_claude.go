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

// claudeTranscript decodes/encodes Claude Code JSONL.
//
// Decode follows the parentUuid chain back from the newest message, the
// branch Claude Code itself resumes; the chain ends at a compact boundary,
// so only post-compaction context is carried. Files without uuids (older or
// synthetic transcripts) are read in file order.
type claudeTranscript struct{}

func (claudeTranscript) InlineMedia() bool { return true }

type claudeRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	ParentUUID       *string         `json:"parentUuid"`
	SessionID        string          `json:"sessionId"`
	Timestamp        string          `json:"timestamp"`
	Cwd              string          `json:"cwd"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsVisibleOnly    bool            `json:"isVisibleInTranscriptOnly"`
	IsAPIError       bool            `json:"isApiErrorMessage"`
	CustomTitle      string          `json:"customTitle"`
	Message          json.RawMessage `json:"message"`
	CompactMetadata  *struct {
		PreservedSegment *struct {
			HeadUUID   string `json:"headUuid"`
			AnchorUUID string `json:"anchorUuid"`
			TailUUID   string `json:"tailUuid"`
		} `json:"preservedSegment"`
		PreservedMessages *struct {
			AnchorUUID string   `json:"anchorUuid"`
			UUIDs      []string `json:"uuids"`
		} `json:"preservedMessages"`
	} `json:"compactMetadata"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// claudeHarnessText marks user text Claude Code injects itself.
var claudeHarnessPrefixes = []string{
	"<command-name>", "<command-message>", "<command-args>", "<local-command-stdout>",
	"<local-command-stderr>", "<local-command-caveat>", "<system-reminder>",
	"<bash-input>", "<bash-stdout>", "<bash-stderr>", "Caveat: The messages below were generated",
}

func claudeHarnessText(s string) bool {
	s = strings.TrimSpace(s)
	for _, p := range claudeHarnessPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (claudeTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	t := transcript.New(transcript.Source{Harness: DefaultName, Format: "claude-jsonl", ConvID: sourceID})
	var records []claudeRecord
	byUUID := map[string]int{}
	err := scanHistoryRecords(r, func(line int, raw []byte) error {
		var rec claudeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("invalid Claude history JSON at line %d", line)
		}
		if rec.SessionID != "" && rec.SessionID != sourceID {
			if rec.Type == "user" || rec.Type == "assistant" {
				t.Losses.Add(transcript.LossInactiveBranch, 1, "different session")
			}
			return nil
		}
		switch rec.Type {
		case "user", "assistant":
		case "custom-title":
			if rec.CustomTitle != "" {
				t.Source.Title = rec.CustomTitle
			}
		case "system", "summary", "attachment", "file-history-snapshot", "file-history-delta", "queue-operation",
			"mode", "permission-mode", "pr-link", "cost-state", "agent-name", "last-prompt",
			"atis-latch", "bridge-session", "progress":
		default:
			t.Losses.Add(transcript.LossUnknownRecord, 1, "claude:"+rec.Type)
		}
		if rec.Cwd != "" {
			t.Source.Cwd = rec.Cwd
		}
		if rec.UUID != "" {
			byUUID[rec.UUID] = len(records)
		}
		records = append(records, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}

	isMessage := func(rec claudeRecord) bool {
		return (rec.Type == "user" || rec.Type == "assistant") && !rec.IsSidechain
	}
	leaf := -1
	sidechain := 0
	mainMessages := 0
	for i, rec := range records {
		if (rec.Type == "user" || rec.Type == "assistant") && rec.IsSidechain {
			sidechain++
		}
		if isMessage(rec) {
			mainMessages++
			leaf = i
		}
	}
	t.Losses.Add(transcript.LossSidechain, sidechain, "")
	if leaf < 0 {
		return t, nil
	}
	var chain []int
	compacted := false
	harnessDropped := 0
	scaffold := func(rec claudeRecord) bool {
		return rec.Type == "attachment" || (rec.Type == "system" && rec.Subtype != "compact_boundary")
	}
	if records[leaf].UUID == "" {
		for i, rec := range records {
			if isMessage(rec) {
				chain = append(chain, i)
			} else if scaffold(rec) {
				harnessDropped++
			}
		}
	} else {
		seen := map[int]bool{}
		boundary := -1
		for at, ok := leaf, true; ok && !seen[at]; {
			seen[at] = true
			rec := records[at]
			if rec.Type == "system" && rec.Subtype == "compact_boundary" {
				compacted = true
				boundary = at
				break
			}
			if isMessage(rec) {
				chain = append(chain, at)
			} else if scaffold(rec) && !rec.IsSidechain {
				harnessDropped++
			}
			if rec.ParentUUID == nil || *rec.ParentUUID == "" {
				break
			}
			at, ok = byUUID[*rec.ParentUUID]
		}
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		if boundary >= 0 {
			chain = claudeSplicePreserved(records, byUUID, boundary, chain, isMessage)
		}
		chain = claudeRecoverParallelResults(records, chain, isMessage)
	}
	if off := mainMessages - len(chain); off > 0 {
		if compacted {
			t.Losses.Add(transcript.LossPreCompaction, off, "")
		} else {
			t.Losses.Add(transcript.LossInactiveBranch, off, "")
		}
	}

	for _, at := range chain {
		rec := records[at]
		// The compact summary is transcript-only in the TUI but is model
		// context; other transcript-only records are not.
		if (rec.IsVisibleOnly && !rec.IsCompactSummary) || rec.IsAPIError {
			harnessDropped++
			continue
		}
		var msg claudeMessage
		if err := json.Unmarshal(rec.Message, &msg); err != nil {
			return nil, fmt.Errorf("invalid Claude message in record %s", rec.UUID)
		}
		when := parseTime(rec.Timestamp)
		var blocks []claudeBlock
		var plain string
		if json.Unmarshal(msg.Content, &plain) != nil {
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				return nil, fmt.Errorf("invalid Claude message content in record %s", rec.UUID)
			}
		} else {
			blocks = []claudeBlock{{Type: "text", Text: plain}}
		}
		if rec.Type == "assistant" {
			if msg.Model != "" && msg.Model != "<synthetic>" {
				t.Source.Model = msg.Model
			}
			e := transcript.Entry{ID: msg.ID, Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: when, Model: msg.Model}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					e.Blocks = append(e.Blocks, transcript.Text(b.Text))
				case "thinking":
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockThinking, Text: b.Thinking, Opaque: &transcript.Opaque{Vendor: "anthropic", Data: b.Signature}})
				case "redacted_thinking":
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockThinking, Opaque: &transcript.Opaque{Vendor: "anthropic", Data: b.Data}})
				case "tool_use", "server_tool_use", "mcp_tool_use":
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockToolCall, CallID: b.ID, Name: b.Name, Input: b.Input})
				case "web_search_tool_result", "web_fetch_tool_result", "mcp_tool_result", "code_execution_tool_result":
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockToolResult, CallID: b.ToolUseID, Text: rawJSONString(b.Content)})
				default:
					t.Losses.Add(transcript.LossUnknownRecord, 1, "claude-block:"+b.Type)
				}
			}
			t.Append(e)
			continue
		}
		if rec.IsMeta {
			harnessDropped++
			continue
		}
		human := transcript.Entry{ID: rec.UUID, Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: when}
		tool := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: when}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				switch {
				case rec.IsCompactSummary:
					human.Blocks = append(human.Blocks, transcript.Block{Kind: transcript.BlockSummary, Text: b.Text})
				case claudeHarnessText(b.Text):
					harnessDropped++
				default:
					human.Blocks = append(human.Blocks, transcript.Text(b.Text))
				}
			case "image", "document":
				if m, ok := claudeMedia(b); ok {
					human.Blocks = append(human.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{m}})
				}
			case "tool_result":
				res := transcript.Block{Kind: transcript.BlockToolResult, CallID: b.ToolUseID, IsError: b.IsError}
				var s string
				if json.Unmarshal(b.Content, &s) == nil {
					res.Text = s
				} else {
					var parts []claudeBlock
					_ = json.Unmarshal(b.Content, &parts)
					var texts []string
					for _, part := range parts {
						switch part.Type {
						case "text":
							texts = append(texts, part.Text)
						case "image", "document":
							if m, ok := claudeMedia(part); ok {
								res.Media = append(res.Media, m)
							}
						}
					}
					res.Text = strings.Join(texts, "\n")
				}
				tool.Blocks = append(tool.Blocks, res)
			default:
				t.Losses.Add(transcript.LossUnknownRecord, 1, "claude-block:"+b.Type)
			}
		}
		// A tool result and a user interjection can share one record;
		// results come first so they stay paired with their calls.
		t.Append(tool)
		t.Append(human)
	}
	t.Losses.Add(transcript.LossHarnessContext, harnessDropped, "")
	return t, nil
}

// claudeRecoverParallelResults re-attaches tool results that sit on a side
// branch. Claude Code records parallel tool calls as a chain of assistant
// records, and each result's parent is its own call, so a walk from the leaf
// sees only the last call's result; Claude Code pairs the others back in on
// load, and so does this.
func claudeRecoverParallelResults(records []claudeRecord, chain []int, isMessage func(claudeRecord) bool) []int {
	onChain := map[int]bool{}
	calls := map[string]int{} // tool_use id -> chain position
	have := map[string]bool{}
	ids := func(rec claudeRecord, kind string) []string {
		var msg claudeMessage
		var blocks []claudeBlock
		if json.Unmarshal(rec.Message, &msg) != nil || json.Unmarshal(msg.Content, &blocks) != nil {
			return nil
		}
		var out []string
		for _, b := range blocks {
			if b.Type == kind {
				if kind == "tool_use" {
					out = append(out, b.ID)
				} else {
					out = append(out, b.ToolUseID)
				}
			}
		}
		return out
	}
	for pos, at := range chain {
		onChain[at] = true
		switch records[at].Type {
		case "assistant":
			for _, id := range ids(records[at], "tool_use") {
				calls[id] = pos
			}
		case "user":
			for _, id := range ids(records[at], "tool_result") {
				have[id] = true
			}
		}
	}
	insertAt := map[int][]int{} // chain position -> orphan records to insert before it
	for at, rec := range records {
		if onChain[at] || rec.Type != "user" || !isMessage(rec) {
			continue
		}
		results := ids(rec, "tool_result")
		if len(results) == 0 {
			continue
		}
		pos, ok := -1, true
		for _, id := range results {
			p, found := calls[id]
			if !found || have[id] {
				ok = false
				break
			}
			pos = max(pos, p)
		}
		if !ok {
			continue
		}
		for _, id := range results {
			have[id] = true
		}
		// Group with the next results on the chain, after the last call.
		before := len(chain)
		for p := pos + 1; p < len(chain); p++ {
			if records[chain[p]].Type == "user" {
				before = p
				break
			}
		}
		insertAt[before] = append(insertAt[before], at)
	}
	if len(insertAt) == 0 {
		return chain
	}
	out := make([]int, 0, len(chain)+len(insertAt))
	for p := 0; p <= len(chain); p++ {
		out = append(out, insertAt[p]...)
		if p < len(chain) {
			out = append(out, chain[p])
		}
	}
	return out
}

// claudeSplicePreserved re-inserts the messages a partial compaction kept
// verbatim. Claude Code records them before the boundary and anchors them
// after the compact summary (preservedMessages / preservedSegment), so a
// plain parentUuid walk from the leaf stops short of them.
func claudeSplicePreserved(records []claudeRecord, byUUID map[string]int, boundary int, chain []int, isMessage func(claudeRecord) bool) []int {
	meta := records[boundary].CompactMetadata
	if meta == nil {
		return chain
	}
	var anchor string
	var uuids []string
	switch {
	case meta.PreservedMessages != nil && len(meta.PreservedMessages.UUIDs) > 0:
		anchor, uuids = meta.PreservedMessages.AnchorUUID, meta.PreservedMessages.UUIDs
	case meta.PreservedSegment != nil && meta.PreservedSegment.HeadUUID != "":
		// Older boundaries name only the segment: follow it forward from
		// the head through the pre-boundary records.
		anchor = meta.PreservedSegment.AnchorUUID
		children := map[string]string{}
		for i := 0; i < boundary; i++ {
			if p := records[i].ParentUUID; p != nil && *p != "" && records[i].UUID != "" {
				children[*p] = records[i].UUID
			}
		}
		for id, n := meta.PreservedSegment.HeadUUID, 0; id != "" && n < len(records); n++ {
			uuids = append(uuids, id)
			if id == meta.PreservedSegment.TailUUID {
				break
			}
			id = children[id]
		}
	default:
		return chain
	}
	onChain := map[int]bool{}
	for _, at := range chain {
		onChain[at] = true
	}
	var preserved []int
	for _, id := range uuids {
		if at, ok := byUUID[id]; ok && !onChain[at] && isMessage(records[at]) {
			preserved = append(preserved, at)
			onChain[at] = true
		}
	}
	if len(preserved) == 0 {
		return chain
	}
	insert := 0
	if at, ok := byUUID[anchor]; ok {
		for i, c := range chain {
			if c == at {
				insert = i + 1
				break
			}
		}
	}
	out := append([]int{}, chain[:insert]...)
	out = append(out, preserved...)
	return append(out, chain[insert:]...)
}

func claudeMedia(b claudeBlock) (transcript.Media, bool) {
	if b.Source == nil {
		return transcript.Media{}, false
	}
	switch b.Source.Type {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			return transcript.Media{}, false
		}
		return transcript.Media{MediaType: b.Source.MediaType, Data: data}, true
	case "url":
		return transcript.Media{MediaType: b.Source.MediaType, URL: b.Source.URL}, true
	}
	return transcript.Media{}, false
}

// claudeTranscriptVersion is stamped on encoded records. Claude Code reads
// older-version transcripts; this only marks the records' provenance shape.
const claudeTranscriptVersion = "2.0.0"

func (claudeTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := uuid.NewString()
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	var parent any
	for i, e := range p.Entries {
		recID := uuid.NewString()
		rec := map[string]any{
			"parentUuid":  parent,
			"isSidechain": false,
			"userType":    "external",
			"cwd":         cwd,
			"sessionId":   id,
			"version":     claudeTranscriptVersion,
			"uuid":        recID,
			"timestamp":   rfc3339ms(times[i]),
		}
		if e.Role == transcript.RoleAssistant {
			var content []map[string]any
			for _, b := range e.Blocks {
				if b.Kind == transcript.BlockText {
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				}
			}
			rec["type"] = "assistant"
			rec["message"] = map[string]any{
				"id": "msg_morph_" + strings.ReplaceAll(recID, "-", "")[:24], "type": "message", "role": "assistant",
				"model": "<synthetic>", "content": content, "stop_reason": "end_turn", "stop_sequence": nil,
				"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
			}
		} else {
			var content []map[string]any
			for _, b := range e.Blocks {
				switch b.Kind {
				case transcript.BlockText:
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				case transcript.BlockMedia:
					for _, m := range b.Media {
						content = append(content, map[string]any{"type": "image", "source": map[string]any{
							"type": "base64", "media_type": m.MediaType, "data": base64.StdEncoding.EncodeToString(m.Data)}})
					}
				}
			}
			rec["type"] = "user"
			rec["message"] = map[string]any{"role": "user", "content": content}
		}
		out.write(rec)
		parent = recID
	}
	if p.Title != "" {
		out.write(map[string]any{"type": "custom-title", "customTitle": p.Title, "sessionId": id})
	}
	return id, out.err
}
