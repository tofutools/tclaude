package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

func TestAccountUsageResetlessReadingExpiresAtKnownReset(t *testing.T) {
	setupTestDB(t)
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	usageapi.UpdateFromStatusLine(nil, &usageapi.CachedBucket{Pct: 20, ResetsAt: reset}, nil)
	// The new percentage lacks a reset; production borrows the prior boundary.
	usageapi.UpdateFromStatusLine(nil, &usageapi.CachedBucket{Pct: 25}, nil)
	out, err := collectAccountUsage(reset.Add(-time.Minute), usageStaleAfter)
	require.NoError(t, err)
	require.Len(t, out.Windows, 1)
	assert.True(t, out.Windows[0].Available)
	assert.Equal(t, reset.Format(time.RFC3339Nano), out.Windows[0].ResetsAt)
	out, err = collectAccountUsage(reset.Add(time.Minute), usageStaleAfter)
	require.NoError(t, err)
	require.Len(t, out.Windows, 1)
	assert.False(t, out.Windows[0].Available)
	assert.Equal(t, "reset", out.Windows[0].Status)
	assert.Equal(t, 25.0, out.Windows[0].Pct)
}
