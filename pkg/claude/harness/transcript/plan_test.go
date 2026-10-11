package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sample() *Transcript {
	t := New(Source{Harness: "claude", ConvID: "c1", Model: "claude-x", Title: "demo"})
	t.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("list the files")}})
	t.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{
		{Kind: BlockThinking, Text: "hmm", Opaque: &Opaque{Vendor: "anthropic", Data: "sig"}},
		Text("Listing."),
		{Kind: BlockToolCall, CallID: "t1", Name: "Bash", Input: json.RawMessage(`{"command": "ls"}`)},
	}})
	t.Append(Entry{Role: RoleUser, Origin: OriginTool, Blocks: []Block{{Kind: BlockToolResult, CallID: "t1", Text: "a.go\nb.go"}}})
	t.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("Two files."), {Kind: BlockToolCall, CallID: "t2", Name: "Read"}}})
	return t
}

func TestPlanFlattensAndAlternates(t *testing.T) {
	p := NewPlan(sample(), Options{Target: "codex", TargetModel: "gpt-x"})
	require.NotEmpty(t, p.Entries)
	assert.Equal(t, RoleUser, p.Entries[0].Role)
	for i := 1; i < len(p.Entries); i++ {
		assert.NotEqual(t, p.Entries[i-1].Role, p.Entries[i].Role, "entries alternate")
	}
	var all strings.Builder
	for _, e := range p.Entries {
		for _, b := range e.Blocks {
			require.Equal(t, BlockText, b.Kind, "only text survives without inline media")
			all.WriteString(b.Text + "\n")
		}
	}
	s := all.String()
	assert.Contains(t, s, "⟦tool call · Bash⟧\n{\"command\":\"ls\"}")
	assert.Contains(t, s, "⟦tool result · Bash · ok⟧\na.go\nb.go")
	assert.Contains(t, s, "no result was recorded")
	assert.NotContains(t, s, "hmm", "reasoning is not carried")
	last := p.Entries[len(p.Entries)-1]
	assert.Equal(t, RoleUser, last.Role)
	assert.Contains(t, planTextOf(last), "carried over from claude (claude-x) to codex (gpt-x)")

	r := p.Report
	assert.Equal(t, 4, r.SourceEntries)
	assert.Equal(t, 1, r.Count(LossReasoningDropped))
	assert.Equal(t, 2, r.Count(LossToolCallsFlattened))
	assert.Equal(t, 1, r.Count(LossInterruptedCall))
	assert.True(t, r.Warnings())
	assert.Contains(t, r.Summary(), "History claude -> codex: carried")
	assert.NotContains(t, r.Summary(), "a.go", "summary holds counts only")
}

func planTextOf(e Entry) string {
	var s []string
	for _, b := range e.Blocks {
		s = append(s, b.Text)
	}
	return strings.Join(s, "\n")
}

func TestPlanClipsLongToolOutput(t *testing.T) {
	tr := New(Source{Harness: "codex"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("go")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{{Kind: BlockToolCall, CallID: "x", Name: "sh", Freeform: true, Text: "cat big"}}})
	big := "HEAD" + strings.Repeat("é", 5000) + "TAIL"
	tr.Append(Entry{Role: RoleUser, Origin: OriginTool, Blocks: []Block{{Kind: BlockToolResult, CallID: "x", Text: big}}})
	p := NewPlan(tr, Options{Target: "claude", ToolResultClipBytes: 1000, TranscriptPath: "/m/transcript.md"})
	var result string
	for _, e := range p.Entries {
		if strings.Contains(planTextOf(e), "tool result") {
			result = planTextOf(e)
		}
	}
	assert.Contains(t, result, "HEAD")
	assert.Contains(t, result, "TAIL")
	assert.Contains(t, result, "bytes clipped; full text in /m/transcript.md")
	assert.Less(t, len(result), 1400)
	assert.True(t, strings.ToValidUTF8(result, "?") == result)
	assert.Equal(t, 1, p.Report.Count(LossToolOutputClipped))
}

func TestPlanBudgetKeepsNewestTurnsAndSummary(t *testing.T) {
	tr := New(Source{Harness: "claude"})
	tr.ResetToSummary("SUMMARY-TEXT", zeroTime())
	for i := 0; i < 50; i++ {
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(strings.Repeat("u", 4000))}})
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("a", 4000))}})
	}
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("NEWEST")}})
	p := NewPlan(tr, Options{Target: "codex", ContextWindowTokens: 20000, BudgetPercent: 50, TranscriptPath: "/x.md"})
	first := planTextOf(p.Entries[0])
	assert.Contains(t, first, "SUMMARY-TEXT", "summary stays pinned")
	assert.Contains(t, first, "earlier entries were omitted to fit the context budget; read them in /x.md")
	assert.Contains(t, planTextOf(p.Entries[len(p.Entries)-1]), "NEWEST")
	omitted := p.Report.Count(LossTruncatedBudget)
	assert.Greater(t, omitted, 50)
	assert.Equal(t, 101+1, p.Report.Carried+omitted, "every source entry is either carried or reported omitted")
	var tokens int64
	for _, e := range p.Entries {
		tokens += estimateTokens(e)
	}
	assert.LessOrEqual(t, tokens, int64(10000+200))
}

func TestPlanMediaInlineExternalizeDrop(t *testing.T) {
	img := Media{MediaType: "image/png", Data: []byte("\x89PNG-fake")}
	tr := New(Source{Harness: "claude"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("look"), {Kind: BlockMedia, Media: []Media{img}}}})

	inline := NewPlan(tr, Options{Target: "codex", InlineMedia: true})
	assert.Equal(t, BlockMedia, inline.Entries[0].Blocks[1].Kind)
	assert.Zero(t, inline.Report.Count(LossMediaExternalized))

	dir := t.TempDir()
	ext := NewPlan(tr, Options{Target: "copilot", MediaDir: dir})
	assert.Contains(t, planTextOf(ext.Entries[0]), filepath.Join(dir, "media-001.png"))
	assert.Equal(t, 1, ext.Report.Count(LossMediaExternalized))
	_, err := os.Stat(filepath.Join(dir, "media-001.png"))
	assert.True(t, os.IsNotExist(err), "a plan writes nothing until applied")
	require.NoError(t, ext.WriteMedia())
	raw, err := os.ReadFile(filepath.Join(dir, "media-001.png"))
	require.NoError(t, err)
	assert.Equal(t, img.Data, raw)

	drop := NewPlan(tr, Options{Target: "copilot"})
	assert.Equal(t, 1, drop.Report.Count(LossMediaDropped))
}

func TestWriteMarkdownIsComplete(t *testing.T) {
	tr := sample()
	var buf bytes.Buffer
	require.NoError(t, WriteMarkdown(tr, &buf))
	s := buf.String()
	assert.Contains(t, s, "list the files")
	assert.Contains(t, s, "hmm", "the transcript file keeps readable reasoning")
	assert.Contains(t, s, "**Tool call `Bash`** (t1)")
	assert.Contains(t, s, "a.go\nb.go")
	p := NewPlan(tr, Options{Target: "gemini"})
	assert.Contains(t, p.Briefing(), "--- user ---\nlist the files")
}

func TestHumanTurnsStaySeparateInTranscript(t *testing.T) {
	tr := New(Source{Harness: "codex"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("one")}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("two")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("a")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("b")}})
	require.Len(t, tr.Entries, 3)
	assert.Len(t, tr.Entries[2].Blocks, 2)
}

func zeroTime() (t0 time.Time) { return }
