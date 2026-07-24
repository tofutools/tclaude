package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeUsageActivityReplayReplaceAndRange(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	row := OpenCodeUsageActivity{
		SessionID: "oc-s1", MessageID: "msg-1", ConvID: "ses-1",
		ProviderID: "openai", ModelID: "gpt-old", ObservedAt: now.Add(-time.Hour),
	}
	require.NoError(t, UpsertOpenCodeUsageActivity(row))
	row.ModelID = "gpt-new"
	require.NoError(t, UpsertOpenCodeUsageActivity(row), "replay replaces the message")

	got, err := OpenCodeUsageActivityBetween(now.Add(-2*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "gpt-new", got[0].ModelID)

	require.NoError(t, ReplaceOpenCodeUsageActivity("oc-s1", []OpenCodeUsageActivity{{
		SessionID: "oc-s1", MessageID: "msg-2", ConvID: "ses-1",
		ProviderID: "anthropic", ModelID: "claude-new", ObservedAt: now.Add(-30 * time.Minute),
	}}, now))
	got, err = OpenCodeUsageActivityBetween(now.Add(-2*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "msg-2", got[0].MessageID)
	assert.Equal(t, "anthropic", got[0].ProviderID)

	have, err := HasOpenCodeUsageActivitySince(now.Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, have)
}
