package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFreeFloatingAgentName(t *testing.T) {
	created := time.Date(2026, time.July, 28, 12, 17, 33, 0, time.FixedZone("CEST", 2*60*60))
	got := FreeFloatingAgentName(created, "agt_f3e10b1d99999999")

	assert.Equal(t, "20260728-101733-f3e10b1d", got)
	assert.True(t, IsFreeFloatingAgentName(got))
	assert.False(t, IsFreeFloatingAgentName("explicit-worker"))
	assert.False(t, IsFreeFloatingAgentName("session-20260728-101733-f3e10b1d"),
		"the superseded prefixed shape must not be treated as the current generated fallback")
}

func TestAutoNamePromptExcerptBoundsRunes(t *testing.T) {
	got := AutoNamePromptExcerpt("  " + strings.Repeat("å", autoNamePromptRunes+10) + "  ")
	assert.Len(t, []rune(got), autoNamePromptRunes)
	assert.NotContains(t, got, " ")
}
