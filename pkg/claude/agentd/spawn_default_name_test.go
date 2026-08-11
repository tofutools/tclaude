package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestDerivedGroupSpawnName(t *testing.T) {
	now := time.Date(2026, 8, 10, 20, 4, 0, 0, time.FixedZone("test", 2*60*60))
	got := derivedGroupSpawnName("review-team", now, "a1b2c3")
	assert.Equal(t, "review-team-20260810-2004-a1b2", got)
	assert.True(t, isValidSpawnName(got))

	long := derivedGroupSpawnName(strings.Repeat("x", 100), now, "abcdef")
	require.LessOrEqual(t, len(long), agent.MaxSpawnNameLen)
	assert.True(t, isValidSpawnName(long))
}
