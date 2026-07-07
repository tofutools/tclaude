package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

func TestMaybeRefreshUsageSkipsAnthropicAPIByDefault(t *testing.T) {
	setupTestDB(t)

	calls := 0
	prev := usageGetCached
	usageGetCached = func() (*usageapi.CachedUsage, error) {
		calls++
		return nil, nil
	}
	t.Cleanup(func() { usageGetCached = prev })

	maybeRefreshUsage()
	require.Zero(t, calls)
}

func TestMaybeRefreshUsageCallsAnthropicAPIWhenOptedIn(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{
		Usage: &config.UsageConfig{PollAnthropicAPI: true},
	}))

	calls := 0
	prev := usageGetCached
	usageGetCached = func() (*usageapi.CachedUsage, error) {
		calls++
		return nil, nil
	}
	t.Cleanup(func() { usageGetCached = prev })

	maybeRefreshUsage()
	require.Equal(t, 1, calls)
}
