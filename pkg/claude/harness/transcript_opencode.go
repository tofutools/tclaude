package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// openCodeTranscript decodes/encodes OpenCode's record-wise export
// projection ({"info":…} then {"message":{"info","parts"}} lines). OpenCode
// resumes from the last completed compaction: an assistant message with
// summary=true whose text is the summary.
type openCodeTranscript struct{}

func (openCodeTranscript) InlineMedia() bool { return true }

type openCodeMessageInfo struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Summary    bool   `json:"summary"`
	Time       struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type openCodePart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	State     struct {
		Status      string          `json:"status"`
		Input       json.RawMessage `json:"input"`
		Output      string          `json:"output"`
		Error       string          `json:"error"`
		Attachments []openCodePart  `json:"attachments"`
	} `json:"state"`
	Mime     string `json:"mime"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Prompt   string `json:"prompt"`
}

func openCodeMedia(p openCodePart) transcript.Media {
	m := transcript.Media{MediaType: p.Mime, Name: p.Filename}
	if mediaType, data, ok := dataURL(p.URL); ok {
		m.Data = data
		if m.MediaType == "" {
			m.MediaType = mediaType
		}
	} else if path, ok := strings.CutPrefix(p.URL, "file://"); ok {
		m.Path = path
	} else {
		m.URL = p.URL
	}
	return m
}

func (openCodeTranscript) Decode(r io.Reader, sourceID string) (*transcript.Transcript, error) {
	t := transcript.New(transcript.Source{Harness: OpenCodeName, Format: "opencode-session-jsonl", ConvID: sourceID})
	header := false
	harness := 0
	err := scanHistoryRecords(r, func(line int, raw []byte) error {
		var rec struct {
			Info    *json.RawMessage `json:"info"`
			Message *struct {
				Info  openCodeMessageInfo `json:"info"`
				Parts []openCodePart      `json:"parts"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("invalid OpenCode history record at line %d", line)
		}
		if rec.Info != nil {
			var info struct {
				ID        string `json:"id"`
				Directory string `json:"directory"`
				Title     string `json:"title"`
			}
			_ = json.Unmarshal(*rec.Info, &info)
			if info.ID != sourceID {
				return errors.New("OpenCode session identity mismatch")
			}
			header = true
			t.Source.Cwd, t.Source.Title = info.Directory, info.Title
			return nil
		}
		if rec.Message == nil || !header {
			return errors.New("OpenCode history lacks session header")
		}
		info, parts := rec.Message.Info, rec.Message.Parts
		at := msTime(info.Time.Created)
		if info.Role == "assistant" {
			model := info.ModelID
			if info.ProviderID != "" && model != "" {
				model = info.ProviderID + "/" + model
			}
			if model != "" {
				t.Source.Model = model
			}
			if info.Summary {
				var texts []string
				for _, p := range parts {
					if p.Type == "text" {
						texts = append(texts, p.Text)
					}
				}
				t.ResetToSummary(strings.Join(texts, "\n"), at)
				return nil
			}
			for _, p := range parts {
				agent := func(b transcript.Block) {
					t.Append(transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Time: at, Model: model, Blocks: []transcript.Block{b}})
				}
				switch p.Type {
				case "text":
					if p.Synthetic || p.Ignored {
						harness++
						continue
					}
					agent(transcript.Text(p.Text))
				case "reasoning":
					agent(transcript.Block{Kind: transcript.BlockThinking, Text: p.Text})
				case "tool":
					agent(transcript.Block{Kind: transcript.BlockToolCall, CallID: p.CallID, Name: p.Tool, Input: p.State.Input})
					switch p.State.Status {
					case "completed", "error":
						res := transcript.Block{Kind: transcript.BlockToolResult, CallID: p.CallID, Name: p.Tool, Text: p.State.Output, IsError: p.State.Status == "error"}
						if res.IsError {
							res.Text = p.State.Error
						}
						for _, a := range p.State.Attachments {
							res.Media = append(res.Media, openCodeMedia(a))
						}
						t.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginTool, Time: at, Blocks: []transcript.Block{res}})
					}
				case "file":
					agent(transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{openCodeMedia(p)}})
				case "subtask", "agent":
					t.Losses.Add(transcript.LossSidechain, 1, "")
				case "step-start", "step-finish", "snapshot", "patch", "retry", "compaction":
				default:
					t.Losses.Add(transcript.LossUnknownRecord, 1, "opencode-part:"+p.Type)
				}
			}
			return nil
		}
		e := transcript.Entry{ID: info.ID, Role: transcript.RoleUser, Origin: transcript.OriginHuman, Time: at}
		for _, p := range parts {
			switch p.Type {
			case "text":
				if p.Synthetic || p.Ignored {
					harness++
					continue
				}
				e.Blocks = append(e.Blocks, transcript.Text(p.Text))
			case "file":
				e.Blocks = append(e.Blocks, transcript.Block{Kind: transcript.BlockMedia, Media: []transcript.Media{openCodeMedia(p)}})
			case "subtask", "agent":
				t.Losses.Add(transcript.LossSidechain, 1, "")
			case "compaction", "step-start", "step-finish", "snapshot", "patch":
			default:
				t.Losses.Add(transcript.LossUnknownRecord, 1, "opencode-part:"+p.Type)
			}
		}
		t.Append(e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !header {
		return nil, errors.New("OpenCode history lacks session header")
	}
	t.Losses.Add(transcript.LossHarnessContext, harness, "")
	return t, nil
}

// openCodeTranscriptVersion is the OpenCode export schema the encoder
// follows (verified with `opencode import`/`export` 1.18).
const openCodeTranscriptVersion = "1.18.35"

// openCodeModel splits a qualified "provider/model". OpenCode's import
// requires a model on every user message and resumes with the last one's, so
// a placeholder would break the next prompt: the target must be qualified.
func openCodeModel(target string) (provider, model string, err error) {
	if provider, model, ok := strings.Cut(target, "/"); ok && provider != "" && model != "" {
		return provider, model, nil
	}
	return "", "", fmt.Errorf("an OpenCode target needs a provider/model, got %q", target)
}

func (openCodeTranscript) Encode(p *transcript.Plan, cwd string, w io.Writer) (string, error) {
	if len(p.Entries) == 0 {
		return "", errors.New("nothing to encode")
	}
	id := openCodeID("ses_")
	out := newJSONLWriter(w)
	times := encodeTimes(p)
	provider, model, err := openCodeModel(p.TargetModel)
	if err != nil {
		return "", err
	}
	title := p.Title
	if title == "" {
		title = "Morphed from " + p.Source.Harness
	}
	created, updated := times[0].UnixMilli(), times[len(times)-1].UnixMilli()
	out.write(map[string]any{"info": map[string]any{
		"id": id, "slug": "morphed", "projectID": "global", "directory": cwd, "title": title,
		"version": openCodeTranscriptVersion, "time": map[string]int64{"created": created, "updated": updated},
	}})
	lastUser := ""
	for i, e := range p.Entries {
		msgID := openCodeID("msg_")
		at := times[i].UnixMilli()
		var parts []map[string]any
		for _, b := range e.Blocks {
			part := map[string]any{"id": openCodeID("prt_"), "sessionID": id, "messageID": msgID}
			switch b.Kind {
			case transcript.BlockText:
				part["type"], part["text"] = "text", b.Text
				parts = append(parts, part)
			case transcript.BlockMedia:
				for j, m := range b.Media {
					if j > 0 {
						part = map[string]any{"id": openCodeID("prt_"), "sessionID": id, "messageID": msgID}
					}
					name := m.Name
					if name == "" {
						name = "image" + mediaExtFor(m.MediaType)
					}
					part["type"], part["mime"], part["url"], part["filename"] = "file", m.MediaType, toDataURL(m), name
					parts = append(parts, part)
				}
			}
		}
		info := map[string]any{"id": msgID, "sessionID": id, "role": string(e.Role), "time": map[string]int64{"created": at}, "agent": "build"}
		if e.Role == transcript.RoleUser {
			info["model"] = map[string]string{"providerID": provider, "modelID": model}
			lastUser = msgID
		} else {
			info["parentID"] = lastUser
			info["mode"] = "build"
			info["path"] = map[string]string{"cwd": cwd, "root": cwd}
			info["cost"] = 0
			info["tokens"] = map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]int{"read": 0, "write": 0}}
			info["modelID"], info["providerID"] = model, provider
			info["time"] = map[string]int64{"created": at, "completed": at}
			info["finish"] = "stop"
		}
		out.write(map[string]any{"message": map[string]any{"info": info, "parts": parts}})
	}
	return id, out.err
}

func mediaExtFor(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}
