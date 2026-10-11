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

// geminiTranscript decodes/encodes Gemini CLI session JSONL: a metadata
// record, then message records keyed by id (a repeated id replaces the
// earlier record in place), $set metadata updates (a $set.messages
// checkpoint replaces all messages) and $rewindTo, which drops the named
// message and everything after it, as gemini-cli's loadConversationRecord.
type geminiTranscript struct{}

func (geminiTranscript) InlineMedia() bool { return true }

type geminiPart struct {
	Text       string `json:"text"`
	Thought    bool   `json:"thought"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FileData *struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	} `json:"fileData"`
	FunctionCall *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
}

type geminiRecord struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Model     string          `json:"model"`
	Thoughts  []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	} `json:"thoughts"`
	ToolCalls []struct {
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Args   json.RawMessage `json:"args"`
		Result json.RawMessage `json:"result"`
		Status string          `json:"status"`
	} `json:"toolCalls"`
}

// geminiParts normalizes a PartListUnion (string, part, or list).
func geminiParts(raw json.RawMessage) []geminiPart {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []geminiPart{{Text: s}}
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []geminiPart
		for _, item := range list {
			out = append(out, geminiParts(item)...)
		}
		return out
	}
	var p geminiPart
	if json.Unmarshal(raw, &p) == nil {
		return []geminiPart{p}
	}
	return nil
}

func geminiMedia(p geminiPart) (transcript.Media, bool) {
	switch {
	case p.InlineData != nil:
		data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			return transcript.Media{}, false
		}
		return transcript.Media{MediaType: p.InlineData.MimeType, Data: data}, true
	case p.FileData != nil:
		m := transcript.Media{MediaType: p.FileData.MimeType, URL: p.FileData.FileURI}
		if path, ok := strings.CutPrefix(m.URL, "file://"); ok {
			m.URL, m.Path = "", path
		}
		return m, true
	}
	return transcript.Media{}, false
}

// geminiResponseText renders a functionResponse.response object.
func geminiResponseText(raw json.RawMessage) (string, bool) {
	var resp struct {
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &resp) == nil {
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			return rawJSONString(resp.Error), true
		}
		if len(resp.Output) > 0 {
			return rawJSONString(resp.Output), false
		}
	}
	return compactJSONText(raw), false
}

func compactJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return ""
	}
	return string(raw)
}

func (geminiTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	t := transcript.New(transcript.Source{Harness: GeminiName, Format: "gemini-session-jsonl", ConvID: sourceID})
	var order []string
	byID := map[string]geminiRecord{}
	put := func(rec geminiRecord) {
		if _, ok := byID[rec.ID]; !ok {
			order = append(order, rec.ID)
		}
		byID[rec.ID] = rec
	}
	recordRemoved := func(previous []string) {
		kind := transcript.LossInactiveBranch
		if len(order) == 0 {
			kind = transcript.LossClearedHistory
		}
		for _, id := range order {
			if geminiCompactionSummary(byID[id]) {
				kind = transcript.LossPreCompaction
				break
			}
		}
		for _, id := range previous {
			if _, ok := byID[id]; !ok {
				t.Losses.Add(kind, 1, "")
			}
		}
	}
	putAll := func(raw json.RawMessage) error {
		var msgs []geminiRecord
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return errors.New("invalid Gemini messages checkpoint")
		}
		previous := append([]string(nil), order...)
		order, byID = nil, map[string]geminiRecord{}
		for _, m := range msgs {
			if m.ID != "" {
				put(m)
			}
		}
		recordRemoved(previous)
		return nil
	}
	found := false
	rewound := 0
	err := scanHistoryRecords(r, func(line int, raw []byte) error {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("invalid Gemini history record at line %d", line)
		}
		meta := func(m map[string]json.RawMessage) error {
			if title := geminiHistoryRawString(m, "summary"); title != "" {
				t.Source.Title = title
			}
			if msgs, ok := m["messages"]; ok {
				return putAll(msgs)
			}
			return nil
		}
		switch {
		case !found:
			if geminiHistoryRawString(rec, "sessionId") != sourceID {
				return errors.New("gemini history has no matching session metadata")
			}
			found = true
			return meta(rec)
		case geminiHistoryRawString(rec, "$rewindTo") != "":
			id := geminiHistoryRawString(rec, "$rewindTo")
			at := -1
			for i, o := range order {
				if o == id {
					at = i
					break
				}
			}
			if at < 0 {
				at = 0
			}
			for _, o := range order[at:] {
				delete(byID, o)
			}
			rewound += len(order) - at
			order = order[:at]
		case rec["$patch"] != nil:
			var patch struct {
				ID        string          `json:"id"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID     string          `json:"id"`
					Result json.RawMessage `json:"result"`
				} `json:"toolCalls"`
				Updates   []json.RawMessage `json:"updates"`
				RemoveIDs []string          `json:"removeIds"`
				OrderIDs  []string          `json:"orderIds"`
			}
			if err := json.Unmarshal(rec["$patch"], &patch); err != nil {
				return errors.New("invalid Gemini message patch")
			}
			previous := append([]string(nil), order...)
			apply := func(raw json.RawMessage) error {
				var update struct {
					ID        string          `json:"id"`
					Content   json.RawMessage `json:"content"`
					ToolCalls []struct {
						ID     string          `json:"id"`
						Result json.RawMessage `json:"result"`
					} `json:"toolCalls"`
				}
				if err := json.Unmarshal(raw, &update); err != nil {
					return errors.New("invalid Gemini message patch update")
				}
				msg, ok := byID[update.ID]
				if !ok {
					return nil
				}
				if len(update.Content) > 0 {
					msg.Content = update.Content
				}
				for _, p := range update.ToolCalls {
					for i := range msg.ToolCalls {
						if msg.ToolCalls[i].ID == p.ID && len(p.Result) > 0 {
							msg.ToolCalls[i].Result = p.Result
						}
					}
				}
				byID[update.ID] = msg
				return nil
			}
			if patch.ID != "" {
				if err := apply(rec["$patch"]); err != nil {
					return err
				}
			}
			for _, u := range patch.Updates {
				if err := apply(u); err != nil {
					return err
				}
			}
			for _, id := range patch.RemoveIDs {
				delete(byID, id)
			}
			kept := order[:0]
			for _, id := range order {
				if _, ok := byID[id]; ok {
					kept = append(kept, id)
				}
			}
			order = kept
			if patch.OrderIDs != nil {
				selected := map[string]bool{}
				var suffix []string
				for _, id := range patch.OrderIDs {
					if _, ok := byID[id]; ok && !selected[id] {
						selected[id] = true
						suffix = append(suffix, id)
					}
				}
				var prefix []string
				for _, id := range order {
					if !selected[id] {
						prefix = append(prefix, id)
					}
				}
				order = append(prefix, suffix...)
			}
			recordRemoved(previous)
		case rec["$set"] != nil:
			var m map[string]json.RawMessage
			_ = json.Unmarshal(rec["$set"], &m)
			return meta(m)
		case geminiHistoryRawString(rec, "id") != "":
			var msg geminiRecord
			if err := json.Unmarshal(raw, &msg); err != nil {
				return fmt.Errorf("invalid Gemini message at line %d", line)
			}
			put(msg)
		default:
			return meta(rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("gemini history has no session metadata")
	}
	t.Losses.Add(transcript.LossInactiveBranch, rewound, "")
	harness := 0
	for _, id := range order {
		msg := byID[id]
		at := parseTime(msg.Timestamp)
		parts := geminiParts(msg.Content)
		switch msg.Type {
		case "user":
			if geminiIgnoredUserRecord(parts) {
				harness++
				continue
			}
			e := transcript.Entry{ID: msg.ID, Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at}
			tool := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at}
			for _, p := range parts {
				switch {
				case p.FunctionResponse != nil:
					text, isErr := geminiResponseText(p.FunctionResponse.Response)
					tool.Blocks = append(tool.Blocks, transcript.Block{Kind: transcript.BlockToolResult, CallID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Text: text, IsError: isErr})
				case p.Text != "":
					kind := transcript.BlockText
					if geminiCompactionSummary(msg) {
						kind = transcript.BlockSummary
					}
					e.Blocks = append(e.Blocks, transcript.Block{Kind: kind, Text: p.Text})
				default:
					if m, ok := geminiMedia(p); ok {
						e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{m}})
					}
				}
			}
			t.Append(tool)
			t.Append(e)
		case "gemini":
			if msg.Model != "" {
				t.Source.Model = msg.Model
			}
			e := transcript.Entry{ID: msg.ID, Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: at, Model: msg.Model}
			modern := false
			for _, p := range parts {
				if p.FunctionCall != nil || p.Thought {
					modern = true
				}
			}
			if !modern {
				for _, th := range msg.Thoughts {
					text := th.Description
					if th.Subject != "" {
						text = "**" + th.Subject + "** " + text
					}
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockThinking, Text: text})
				}
			}
			for _, p := range parts {
				switch {
				case p.Thought:
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockThinking, Text: p.Text})
				case p.FunctionCall != nil:
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockToolCall, CallID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args})
				case p.Text != "":
					e.Blocks = append(e.Blocks, transcript.Text(p.Text))
				default:
					if m, ok := geminiMedia(p); ok {
						e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{m}})
					}
				}
			}
			if !modern {
				for _, tc := range msg.ToolCalls {
					e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockToolCall, CallID: tc.ID, Name: tc.Name, Input: tc.Args})
				}
			}
			t.Append(e)
			tool := transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at}
			for _, tc := range msg.ToolCalls {
				if len(tc.Result) == 0 || string(tc.Result) == "null" {
					continue
				}
				res := transcript.Block{Kind: transcript.BlockToolResult, CallID: tc.ID, Name: tc.Name, IsError: tc.Status == "error" || tc.Status == "cancelled"}
				var texts []string
				for _, p := range geminiParts(tc.Result) {
					switch {
					case p.FunctionResponse != nil:
						text, isErr := geminiResponseText(p.FunctionResponse.Response)
						texts = append(texts, text)
						res.IsError = res.IsError || isErr
					case p.Text != "":
						texts = append(texts, p.Text)
					default:
						if m, ok := geminiMedia(p); ok {
							res.Media = append(res.Media, m)
						}
					}
				}
				res.Text = strings.Join(texts, "\n")
				tool.Blocks = append(tool.Blocks, res)
			}
			t.Append(tool)
		case "info", "error", "warning":
			harness++
		default:
			t.Losses.Add(transcript.LossUnknownRecord, 1, "gemini:"+msg.Type)
		}
	}
	t.Losses.Add(transcript.LossHarnessContext, harness, "")
	return t, nil
}

// geminiIgnoredUserRecord matches the scaffolding gemini-cli itself skips on
// resume: an empty prompt, a slash or "?" command, and injected
// session/hook context. Only text-only records qualify, so a record carrying
// a function response or a pasted image is never dropped, and a prompt that
// merely starts with a path ("/src/x.go fails") is kept.
func geminiIgnoredUserRecord(parts []geminiPart) bool {
	var text strings.Builder
	for _, p := range parts {
		if p.FunctionResponse != nil || p.FunctionCall != nil || p.InlineData != nil || p.FileData != nil {
			return false
		}
		text.WriteString(p.Text)
	}
	trimmed := strings.TrimSpace(text.String())
	if trimmed == "" || strings.HasPrefix(trimmed, "<session_context>") || strings.HasPrefix(trimmed, "<hook_context>") {
		return true
	}
	first, _, _ := strings.Cut(trimmed, " ")
	if cmd, ok := strings.CutPrefix(first, "/"); ok {
		return cmd != "" && !strings.ContainsAny(cmd, "/.")
	}
	return first == "?"
}

func (geminiTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := uuid.NewString()
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	meta := map[string]any{
		"sessionId": id, "projectHash": geminiProjectHash(cwd), "startTime": rfc3339ms(times[0]),
		"lastUpdated": rfc3339ms(times[len(times)-1]), "kind": "main",
	}
	if p.Title != "" {
		meta["summary"] = p.Title
	}
	out.write(meta)
	for i, e := range p.Entries {
		var parts []map[string]any
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockText:
				parts = append(parts, map[string]any{"text": b.Text})
			case transcript.BlockMedia:
				for _, m := range b.Media {
					parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": m.MediaType, "data": base64.StdEncoding.EncodeToString(m.Data)}})
				}
			}
		}
		kind := "user"
		if e.Role == transcript.RoleAssistant {
			kind = "gemini"
		}
		out.write(map[string]any{"id": uuid.NewString(), "timestamp": rfc3339ms(times[i]), "type": kind, "content": parts})
	}
	return id, out.err
}

// Compression emits a synthetic user state_snapshot. Metadata.summary is only
// the conversation title and must never be substituted for model context.
func geminiCompactionSummary(rec geminiRecord) bool {
	if rec.Type != "user" {
		return false
	}
	for _, p := range geminiParts(rec.Content) {
		if strings.HasPrefix(strings.TrimSpace(p.Text), "<state_snapshot>") {
			return true
		}
	}
	return false
}
