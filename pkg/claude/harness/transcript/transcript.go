// Package transcript is the harness-neutral conversation form used to move an
// agent's history between harnesses (cross-harness morph). A harness codec
// decodes its native history into a Transcript, a Plan applies the target
// policy (what crosses, what is flattened, what fits), and the target codec
// encodes the Plan into its own native history.
//
// A Transcript is the effective model context, not the whole native log:
// only the active branch, from the last compaction onward, without harness
// scaffolding the target re-injects itself. Everything left behind is counted
// in the LossReport so a morph can say exactly what did not carry over.
package transcript

import (
	"encoding/json"
	"strings"
	"time"
)

// Role is the conversational side of an entry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Origin says who produced an entry. Tool results travel on the user side of
// every native format, so they are user-role entries with OriginTool; so do
// system text and other agents' messages.
//
// The set is open: a reader treats an origin it does not know as untrusted
// input that is not the operator's.
type Origin string

const (
	// OriginHuman is text the operator typed, and nothing else.
	OriginHuman Origin = "human"
	OriginAgent Origin = "agent"
	OriginTool  Origin = "tool"
	// OriginSystem is text a harness or orchestrator injected: compaction
	// summaries, tclaude notices and the like.
	OriginSystem Origin = "system"
	// OriginPeer is another agent's message; Author names it.
	OriginPeer Origin = "peer"
)

// Known reports whether o is an origin this package defines.
func (o Origin) Known() bool {
	switch o {
	case OriginHuman, OriginAgent, OriginTool, OriginSystem, OriginPeer:
		return true
	}
	return false
}

// ClassifyUserText sorts user-side text a harness recorded as the user's:
// a tclaude agent message is a peer's, or the operator's when the operator
// sent it, or the system's when it names no sender; other tclaude notices
// are the system's, and the rest is the operator's.
func ClassifyUserText(text string) (Origin, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[system:") {
		return OriginHuman, ""
	}
	header, _, _ := strings.Cut(text, "]")
	if strings.HasPrefix(header, "[system: new agent message") {
		if _, from, ok := strings.Cut(header, " from "); ok {
			// The operator's own messages come through the same inbox.
			if strings.HasPrefix(from, "the human operator") {
				return OriginHuman, ""
			}
			name, _, _ := strings.Cut(from, ";")
			name, _, _ = strings.Cut(name, " (")
			name, _, _ = strings.Cut(name, " for you")
			return OriginPeer, strings.TrimSpace(name)
		}
	}
	return OriginSystem, ""
}

// classifyHumanEntry narrows an entry a codec recorded as the operator's:
// a compaction summary is the system's, and tclaude's agent messages and
// notices are a peer's or the system's.
func classifyHumanEntry(e Entry) (Origin, string) {
	for _, b := range e.Blocks {
		if b.Kind == BlockSummary {
			return OriginSystem, e.Author
		}
	}
	for _, b := range e.Blocks {
		if b.Kind != BlockText {
			continue
		}
		o, author := ClassifyUserText(b.Text)
		if author == "" {
			author = e.Author
		}
		return o, author
	}
	return OriginHuman, e.Author
}

// Heading is how a reader labels an entry: its role, or what produced it
// when that is not the operator or the agent itself.
func (e Entry) Heading() string {
	h := string(e.Role)
	switch {
	case e.Origin == OriginTool:
		h = "tool results"
	case e.Origin == OriginSystem:
		h = "system"
	case e.Origin == OriginPeer:
		h = "peer agent"
	case e.Origin != "" && !e.Origin.Known():
		h = string(e.Role) + " (" + string(e.Origin) + ", untrusted)"
	}
	if e.Author != "" {
		h += " — " + e.Author
	}
	return h
}

// BlockKind discriminates Block.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolCall   BlockKind = "tool_call"
	BlockToolResult BlockKind = "tool_result"
	BlockMedia      BlockKind = "media"
	// BlockSummary is a compaction summary: the stand-in for everything the
	// source harness compacted away.
	BlockSummary BlockKind = "summary"
)

// Media is an image or document. Exactly one of Data, Path or URL is set.
type Media struct {
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Path      string `json:"path,omitempty"`
	URL       string `json:"url,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Opaque is provider-bound reasoning state (a thinking signature, encrypted
// reasoning content). It is only meaningful to the vendor that minted it.
type Opaque struct {
	Vendor string `json:"vendor"`
	Data   string `json:"data"`
}

// Block is one piece of an entry; Kind selects which fields are meaningful.
type Block struct {
	Kind BlockKind `json:"kind"`
	// Text is the text, thinking text, summary, tool-result output, or a
	// freeform tool call's input.
	Text   string  `json:"text,omitempty"`
	Opaque *Opaque `json:"opaque,omitempty"`
	// CallID pairs a tool_call with its tool_result; Name is the tool.
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Input is a structured tool call's arguments (JSON). A Freeform call
	// carries its raw input in Text instead.
	Input    json.RawMessage `json:"input,omitempty"`
	Freeform bool            `json:"freeform,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
	// Media is the media of a media block, or attachments of a tool result.
	Media []Media `json:"media,omitempty"`
}

// Entry is one turn-side message.
type Entry struct {
	ID     string    `json:"id,omitempty"`
	Role   Role      `json:"role"`
	Origin Origin    `json:"origin"`
	Time   time.Time `json:"time,omitzero"`
	Model  string    `json:"model,omitempty"`
	// Author names a non-primary speaker (a Codex sub-agent message).
	Author string  `json:"author,omitempty"`
	Blocks []Block `json:"blocks"`
}

// Source describes where a transcript came from.
type Source struct {
	Harness string `json:"harness"`
	Format  string `json:"format"`
	ConvID  string `json:"conv_id"`
	Model   string `json:"model,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Title   string `json:"title,omitempty"`
}

// Transcript is a decoded conversation plus what the decode left behind.
type Transcript struct {
	Source  Source      `json:"source"`
	Entries []Entry     `json:"entries"`
	Losses  *LossReport `json:"losses"`
}

// New returns an empty transcript for a source.
func New(src Source) *Transcript {
	return &Transcript{Source: src, Losses: &LossReport{Source: src.Harness}}
}

// Append adds an entry, merging consecutive agent entries (native formats
// split one model response across several records: Claude per content block,
// Codex per response item) and consecutive tool-result entries. Human turns
// stay separate.
func (t *Transcript) Append(e Entry) {
	if len(e.Blocks) == 0 {
		return
	}
	if e.Origin == OriginHuman {
		e.Origin, e.Author = classifyHumanEntry(e)
	}
	if n := len(t.Entries); n > 0 {
		last := &t.Entries[n-1]
		if last.Role == e.Role && last.Origin == e.Origin && last.Author == e.Author && e.Origin != OriginHuman {
			last.Blocks = append(last.Blocks, e.Blocks...)
			if last.Model == "" {
				last.Model = e.Model
			}
			return
		}
	}
	t.Entries = append(t.Entries, e)
}

// ResetToSummary discards everything decoded so far: a compaction replaced
// it. A non-empty summary becomes the new first entry.
func (t *Transcript) ResetToSummary(summary string, at time.Time) {
	if n := countEntries(t.Entries); n > 0 {
		t.Losses.Add(LossPreCompaction, n, "")
	}
	t.Entries = nil
	if summary != "" {
		t.Entries = append(t.Entries, Entry{Role: RoleUser, Origin: OriginHuman, Time: at, Blocks: []Block{{Kind: BlockSummary, Text: summary}}})
	}
}

func countEntries(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if len(e.Blocks) == 1 && e.Blocks[0].Kind == BlockSummary {
			continue
		}
		n++
	}
	return n
}

// Text is a convenience text block.
func Text(s string) Block { return Block{Kind: BlockText, Text: s} }
