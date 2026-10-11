package transcript

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"strings"
	"testing"
)

func TestOverBudgetPinsTaskLatestAndSummarizesMiddle(t *testing.T) {
	tr := New(Source{Harness: "claude"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("ORIGINAL TASK: fix the checkout")}})
	for i := 0; i < 50; i++ {
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("old detail ", 200))}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(fmt.Sprintf("older instruction %d", i))}})
	}
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("understood")}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("LATEST: preserve the API")}})
	opts := Options{Target: "codex", ContextWindowTokens: 4096, TranscriptPath: "/m/transcript.md"}
	p := NewPlan(tr, opts)
	assert.Contains(t, p.Briefing(), "ORIGINAL TASK: fix the checkout")
	assert.Contains(t, p.Briefing(), "LATEST: preserve the API")
	assert.True(t, p.Report.NeedsSummary)
	assert.True(t, p.Report.BudgetWarning)
	assert.GreaterOrEqual(t, p.Report.DroppedPercent, 25)
	assert.Equal(t, 100, p.Report.KeptPercent+p.Report.DroppedPercent)
	opts.Summary = "SUMMARY: the earlier work fixed the parser; preserve compatibility."
	p = NewPlan(tr, opts)
	assert.Contains(t, p.Briefing(), opts.Summary)
	assert.Contains(t, p.Briefing(), "ORIGINAL TASK")
	assert.Contains(t, p.Briefing(), "LATEST")
	assert.LessOrEqual(t, p.Report.CarriedEstimatedTokens, int64(4096*60/100))
}

func TestOverBudgetGiantPinnedTurnsStayInFile(t *testing.T) {
	tr := New(Source{Harness: "claude"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(strings.Repeat("ORIGINAL", 20000))}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text("done")}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(strings.Repeat("LATEST", 20000))}})
	p := NewPlan(tr, Options{Target: "codex", ContextWindowTokens: 4096, TranscriptPath: "/m/transcript.md", Summary: strings.Repeat("summary", 20000)})
	assert.True(t, p.Report.PinnedExternalized)
	assert.True(t, p.Report.NeedsSummary)
	assert.Contains(t, p.Briefing(), "original task")
	assert.Contains(t, p.Briefing(), "latest user instructions")
	assert.Contains(t, p.Briefing(), "/m/summary.md")
	assert.LessOrEqual(t, p.Report.CarriedEstimatedTokens, int64(4096*60/100))
	var full strings.Builder
	assert.NoError(t, WriteMarkdown(tr, &full))
	assert.Contains(t, full.String(), strings.Repeat("ORIGINAL", 20000))
	assert.Contains(t, full.String(), strings.Repeat("LATEST", 20000))
}

func TestOverBudgetDaemonNudgeDoesNotReplaceLatestInstruction(t *testing.T) {
	tr := New(Source{Harness: "claude"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("ORIGINAL: repair checkout")}})
	for i := 0; i < 40; i++ {
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("detail ", 400))}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(fmt.Sprintf("instruction %d", i))}})
	}
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("work ", 400))}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("[system: new agent message #122 from the human operator; delivery: inline; subject: Preserve the API] LATEST: preserve backward compatibility")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("huge last work ", 2000))}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("[system: new agent message #123; delivery: inline; subject: Morph handoff requested] Write the summary now.")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("writing summary ", 2000))}})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("[system: new agent message #124 for you. fetch with: tclaude agent inbox read 124; subject: Morph handoff requested]")}})
	tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{Text(strings.Repeat("retrieved summary request ", 2000))}})
	p := NewPlan(tr, Options{Target: "codex", ContextWindowTokens: 4096, TranscriptPath: "/m/transcript.md", Summary: "Earlier parser work is done."})
	assert.Contains(t, p.Briefing(), "ORIGINAL: repair checkout")
	assert.Contains(t, p.Briefing(), "LATEST: preserve backward compatibility")
	assert.LessOrEqual(t, p.Report.CarriedEstimatedTokens, p.Report.BudgetTokens)
}
