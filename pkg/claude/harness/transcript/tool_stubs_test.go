package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolHeavyTranscript() *Transcript {
	tr := New(Source{Harness: "claude"})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("call-%d", i)
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(fmt.Sprintf("Task %d", i))}})
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("Checking the build."), {Kind: BlockToolCall, Name: "shell", CallID: id, Input: json.RawMessage(`{"command":"go test ./pkg/..."}`)}}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginTool, Blocks: []Block{{Kind: BlockToolResult, CallID: id, Text: fmt.Sprintf("RESULT-%d\nsecond line\n", i) + strings.Repeat("build output é\n", 1000) + "TAIL", IsError: i == 0}}})
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("Continue with the next task.")}})
	}
	return tr
}

func TestToolStubsPreserveTasksRecentOutputAndFullTranscript(t *testing.T) {
	tr := toolHeavyTranscript()
	before := NewPlan(tr, Options{Target: "codex"})
	after := NewPlan(tr, Options{Target: "codex", TranscriptPath: "/m/transcript.md"})
	text := after.Briefing()
	assert.Contains(t, text, "tool · shell · {\"command\":\"go test ./pkg/...\"} · error · RESULT-0 second line; full: /m/transcript.md#entry-2")
	assert.Contains(t, text, "full: /m/transcript.md#entry-3")
	assert.NotContains(t, text, "RESULT-0\nsecond line\nbuild output")
	assert.Contains(t, text, "RESULT-17\nsecond line\nbuild output", "last three human turns keep fuller output")
	assert.Contains(t, text, "TAIL", "recent clipping preserves the tail")
	for i := 0; i < 20; i++ {
		assert.Contains(t, text, fmt.Sprintf("Task %d", i))
	}
	assert.Equal(t, 34, after.Report.Count(LossToolRecordsStubbed))
	assert.Zero(t, after.Report.Count(LossTruncatedBudget))
	assert.Less(t, after.Report.CarriedEstimatedTokens, before.Report.CarriedEstimatedTokens/4)
	assert.True(t, utf8.ValidString(text))
	var full bytes.Buffer
	require.NoError(t, WriteMarkdown(tr, &full))
	assert.Contains(t, full.String(), "<a id=\"entry-3\"></a>")
	assert.Contains(t, full.String(), tr.Entries[2].Blocks[0].Text)
	assert.Equal(t, before.Report.SourceEstimatedTokens, after.Report.SourceEstimatedTokens)
	t.Logf("tool-heavy fixture: estimated tokens before=%d after=%d source=%d", before.Report.CarriedEstimatedTokens, after.Report.CarriedEstimatedTokens, after.Report.SourceEstimatedTokens)
}

func TestToolStubsInterruptedCallsAndMedia(t *testing.T) {
	tr := toolHeavyTranscript()
	tr.Entries[1].Blocks = append(tr.Entries[1].Blocks, Block{Kind: BlockToolCall, Name: "unfinished", CallID: "no-result"})
	tr.Entries[2].Blocks[0].Media = []Media{{MediaType: "image/png", Data: []byte("image")}}
	p := NewPlan(tr, Options{Target: "codex", TranscriptPath: "/m/transcript.md", InlineMedia: true})
	assert.Contains(t, p.Briefing(), "unfinished ·  · interrupted")
	assert.Equal(t, 1, p.Report.Count(LossInterruptedCall))
	found := false
	for _, e := range p.Entries {
		for _, b := range e.Blocks {
			if b.Kind == BlockMedia {
				found = true
				assert.Equal(t, []byte("image"), b.Media[0].Data)
			}
		}
	}
	assert.True(t, found, "stubbing does not discard result media")
	noFile := NewPlan(tr, Options{Target: "codex"})
	assert.Zero(t, noFile.Report.Count(LossToolRecordsStubbed), "no inaccessible transcript pointers")
	assert.Contains(t, noFile.Briefing(), "RESULT-0\nsecond line\nbuild output")
}
