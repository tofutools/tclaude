package transcript

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolTurns is a task, then n turns each with chat and a sizeable tool
// call and result, then the latest instruction.
func toolTurns(n int) *Transcript {
	tr := New(Source{Harness: "copilot"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("ORIGINAL TASK " + strings.Repeat("t", 3000))}})
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{
			Text(fmt.Sprintf("thinking aloud %d ", i) + strings.Repeat("chat ", 200)),
			{Kind: BlockToolCall, CallID: id, Name: "bash", Input: []byte(fmt.Sprintf(`{"cmd":"cat file%d %s"}`, i, strings.Repeat("a", 400)))},
		}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginTool, Blocks: []Block{{Kind: BlockToolResult, CallID: id, Text: fmt.Sprintf("OUTPUT%d ", i) + strings.Repeat("o", 3000)}}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(fmt.Sprintf("instruction %d", i))}})
	}
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("LATEST " + strings.Repeat("l", 3000))}})
	return tr
}

func usageOf(r *LossReport, c Category) CategoryUsage {
	for _, u := range r.Categories {
		if u.Category == c {
			return u
		}
	}
	return CategoryUsage{}
}

func TestCarryKeepPerCategoryShortensOlderRecordsAndTellsTheAgent(t *testing.T) {
	tr := toolTurns(20)
	full := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md", Keep: map[Category]int{CategoryToolOutput: 100, CategoryToolCalls: 100}})
	assert.Zero(t, usageOf(full.Report, CategoryToolOutput).Shortened, "100%% keeps every output whole, unlike the automatic last-three-turns rule")
	assert.Contains(t, full.Briefing(), "OUTPUT0 ")

	p := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md", Keep: map[Category]int{CategoryToolOutput: 25}})
	out := usageOf(p.Report, CategoryToolOutput)
	require.NotNil(t, out.KeepPercent)
	assert.Equal(t, 25, *out.KeepPercent)
	assert.Positive(t, out.Shortened)
	assert.Positive(t, out.ShortenedTokens)
	assert.Less(t, out.CarriedTokens, out.SourceTokens*40/100, "about a quarter of the output stays")
	assert.Equal(t, usageOf(p.Report, CategoryChat).SourceTokens, usageOf(full.Report, CategoryChat).SourceTokens)
	assert.Contains(t, p.Briefing(), "OUTPUT19 ", "the newest output stays whole")
	assert.NotContains(t, p.Briefing(), "OUTPUT0 "+strings.Repeat("o", 300), "the oldest output is a stub")
	assert.Contains(t, p.Briefing(), "Cut during conversion, on the operator's carry-over settings")
	assert.Contains(t, p.Briefing(), "older tool outputs")
	assert.Contains(t, p.Briefing(), "/m/transcript.md")
	assert.Zero(t, p.Report.Count(LossTruncatedBudget), "nothing is omitted")
}

func TestCarryChatKeepZeroNeverShortensTaskOrLatest(t *testing.T) {
	tr := toolTurns(6)
	p := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md", Keep: map[Category]int{CategoryChat: 0}})
	b := p.Briefing()
	assert.Contains(t, b, "ORIGINAL TASK "+strings.Repeat("t", 3000))
	assert.Contains(t, b, "LATEST "+strings.Repeat("l", 3000))
	assert.Contains(t, b, "earlier assistant message shortened")
	assert.Positive(t, usageOf(p.Report, CategoryChat).Shortened)
	assert.Contains(t, b, "older chat messages")
}

func TestCarryCutFirstShortensTheCategoryBeforeOmittingTurns(t *testing.T) {
	tr := toolTurns(30)
	base := Options{Target: "claude", ContextWindowTokens: 80_000, TranscriptPath: "/m/transcript.md",
		Keep: map[Category]int{CategoryToolOutput: 100, CategoryToolCalls: 100}}
	oldest := NewPlan(tr, base)
	require.Positive(t, oldest.Report.Count(LossTruncatedBudget), "the fixture overflows the budget")

	cutFirst := base
	cutFirst.Keep = map[Category]int{CategoryToolCalls: 100}
	cutFirst.CutFirst = CategoryToolOutput
	p := NewPlan(tr, cutFirst)
	assert.Zero(t, p.Report.Count(LossTruncatedBudget), "shortening tool output alone made it fit")
	out := usageOf(p.Report, CategoryToolOutput)
	require.NotNil(t, out.KeepPercent, "the chosen share is reported")
	assert.Less(t, *out.KeepPercent, 100)
	assert.Positive(t, out.Shortened)
	assert.LessOrEqual(t, p.Report.CarriedEstimatedTokens, p.Report.BudgetTokens)
	assert.Equal(t, CategoryToolOutput, p.Report.CutFirst)
	assert.Contains(t, p.Briefing(), "instruction 0", "whole turns survive")

	// An explicit keep share for the category wins over the search.
	cutFirst.Keep = map[Category]int{CategoryToolOutput: 100, CategoryToolCalls: 100}
	assert.Positive(t, NewPlan(tr, cutFirst).Report.Count(LossTruncatedBudget))
}

func TestCarryAllAndStatedBudget(t *testing.T) {
	tr := toolTurns(30)
	all := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 20_000, TranscriptPath: "/m/transcript.md", CarryAll: true})
	assert.Zero(t, all.Report.Count(LossTruncatedBudget))
	assert.False(t, all.Report.NeedsSummary)
	assert.True(t, all.Report.CarryAll)
	assert.Equal(t, "all", all.Report.BudgetSource)
	assert.True(t, all.Report.OverWindow, "carrying more than the window warns")
	assert.True(t, all.Report.OverHeadroom)
	assert.False(t, all.Report.OverBudget(), "carry-all has no budget to overflow, so native delivery stands")
	assert.Contains(t, all.Briefing(), "instruction 0")

	stated := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, BudgetTokens: 10_000, TranscriptPath: "/m/transcript.md"})
	assert.Equal(t, int64(10_000), stated.Report.BudgetTokens)
	assert.Equal(t, "stated", stated.Report.BudgetSource)
	assert.Positive(t, stated.Report.Count(LossTruncatedBudget))
	assert.LessOrEqual(t, stated.Report.CarriedEstimatedTokens, int64(10_000))

	auto := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md"})
	assert.Equal(t, "auto", auto.Report.BudgetSource)
	assert.Contains(t, auto.Briefing(), "Cut during conversion, automatically", "the automatic tool stubs are named too")
}

// A stub can outgrow a small record. Under a share such a record stays
// whole, so cutting tool calls first is never worse than the oldest-first cut.
func TestCarryCutFirstNeverWorseWhenStubsOutgrowRecords(t *testing.T) {
	tr := New(Source{Harness: "copilot"})
	tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text("TASK")}})
	for i := range 60 {
		id := fmt.Sprintf("c%d", i)
		tr.Append(Entry{Role: RoleAssistant, Origin: OriginAgent, Blocks: []Block{{Kind: BlockToolCall, CallID: id, Name: "ls", Input: []byte(`{}`)}}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginTool, Blocks: []Block{{Kind: BlockToolResult, CallID: id, Text: strings.Repeat("r", 230)}}})
		tr.Append(Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(fmt.Sprintf("go on %d", i))}})
	}
	base := Options{Target: "claude", ContextWindowTokens: 20_000, TranscriptPath: "/m/transcript.md"}
	oldest := NewPlan(tr, base)
	cut := base
	cut.CutFirst = CategoryToolCalls
	p := NewPlan(tr, cut)
	assert.LessOrEqual(t, p.Report.Count(LossTruncatedBudget), oldest.Report.Count(LossTruncatedBudget))
	u := usageOf(p.Report, CategoryToolCalls)
	assert.LessOrEqual(t, u.CarriedTokens, usageOf(oldest.Report, CategoryToolCalls).CarriedTokens, "cutting calls first never carries more of them")

	keep := base
	keep.Keep = map[Category]int{CategoryToolCalls: 0}
	k := usageOf(NewPlan(tr, keep).Report, CategoryToolCalls)
	assert.Zero(t, k.Shortened, "stubs bigger than the calls are not used")
}

func TestCarryAllKeepsEveryRecordWhole(t *testing.T) {
	tr := toolTurns(10)
	p := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md", CarryAll: true})
	for _, c := range Categories {
		assert.Zerof(t, usageOf(p.Report, c).Shortened, "%s is carried whole", c)
	}
	assert.Contains(t, p.Briefing(), "OUTPUT0 "+strings.Repeat("o", 300))
	shared := NewPlan(tr, Options{Target: "claude", ContextWindowTokens: 1_000_000, TranscriptPath: "/m/transcript.md", CarryAll: true, Keep: map[Category]int{CategoryToolOutput: 10}})
	assert.Positive(t, usageOf(shared.Report, CategoryToolOutput).Shortened, "a named share still applies")
}
