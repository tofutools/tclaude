package paf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainLosses(t *testing.T) {
	first := Losses{Source: "claude", Target: "codex", Items: []LossItem{}}
	second := ChainLosses(&first, Losses{Source: "codex", Target: "gemini", Items: []LossItem{}})
	require.Len(t, second.Prior, 1)
	assert.Equal(t, "claude", second.Prior[0].Source)

	third := ChainLosses(&second, Losses{Source: "gemini", Items: []LossItem{}})
	require.Len(t, third.Prior, 2, "the earlier chain is flattened, oldest first")
	assert.Equal(t, "claude", third.Prior[0].Source)
	assert.Equal(t, "codex", third.Prior[1].Source)
	assert.Empty(t, third.Prior[1].Prior)
	assert.Len(t, second.Prior, 1, "chaining leaves its input alone")

	assert.Empty(t, ChainLosses(nil, Losses{Source: "x"}).Prior)
	assert.Empty(t, ChainLosses(&Losses{}, Losses{Source: "x"}).Prior, "an empty report records nothing")
}

func TestChainLossesKeepsTheNewestReports(t *testing.T) {
	l := Losses{Source: "s0", Items: []LossItem{}}
	for range maxPriorLosses + 5 {
		l = ChainLosses(&l, Losses{Source: "next", Items: []LossItem{}})
	}
	assert.Len(t, l.Prior, maxPriorLosses)
	assert.Equal(t, "next", l.Prior[0].Source, "the oldest went first")
}

func TestCountsOnly(t *testing.T) {
	l := Losses{
		Source: "claude", Target: "codex", SourceEntries: 9, DroppedPercent: 20, ContextWindowSource: "text",
		Items:      []LossItem{{Kind: "unknown_record", Severity: "warn", Count: 2, Detail: "text"}},
		Prior:      []Losses{{Source: "pi", ContextWindowSource: "text", Items: []LossItem{{Kind: "redacted", Severity: "info", Count: 1, Detail: "text"}}}},
		Extensions: Extensions{"x": []byte(`"text"`)},
	}
	got := CountsOnly(l)
	assert.Equal(t, Losses{
		Source: "claude", Target: "codex", SourceEntries: 9, DroppedPercent: 20,
		Items: []LossItem{{Kind: "unknown_record", Severity: "warn", Count: 2}},
		Prior: []Losses{{Source: "pi", Items: []LossItem{{Kind: "redacted", Severity: "info", Count: 1}}}},
	}, got)
	assert.Equal(t, "text", l.Items[0].Detail, "the stored report is left alone")
	assert.Equal(t, "text", l.Prior[0].Items[0].Detail, "the stored report is left alone")
	assert.NotNil(t, CountsOnly(Losses{Source: "pi", Items: []LossItem{}}).Items, "the schema requires items")
}
