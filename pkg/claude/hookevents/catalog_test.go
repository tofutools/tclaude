package hookevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCatalogCoversHarnessesAndNormalizesSelectors(t *testing.T) {
	for _, harness := range []string{HarnessClaude, HarnessCodex, HarnessOpenCode} {
		assert.NotEmpty(t, BaselineEvents(harness), harness)
	}
	got := NormalizeSelectors([]Selector{
		{Harness: " Codex ", Event: "PostCompact"},
		{Harness: "codex", Event: "PostCompact"},
		{Harness: "CLAUDE", Event: "SessionStart"},
	})
	assert.Equal(t, []Selector{
		{Harness: "claude", Event: "SessionStart"},
		{Harness: "codex", Event: "PostCompact"},
	}, got)
}

func TestCatalogMarksOnlyKnownSelectorsValid(t *testing.T) {
	assert.True(t, Valid(Selector{Harness: HarnessOpenCode, Event: "session.compacted"}))
	assert.False(t, Valid(Selector{Harness: HarnessOpenCode, Event: "not.real"}))
	assert.True(t, SupportsSameContinuation(
		Selector{Harness: HarnessClaude, Event: "PostToolUse"}))
	assert.False(t, SupportsSameContinuation(
		Selector{Harness: HarnessClaude, Event: "SessionEnd"}))
}
