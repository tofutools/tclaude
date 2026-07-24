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

	require.NoError(t, ReplaceOpenCodeUsageActivity("oc-s1", "ses-1", []OpenCodeUsageActivity{{
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

func TestOpenCodeUsageActivityFollowsConversationAcrossResumeAndPrunes(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, UpsertOpenCodeUsageActivity(OpenCodeUsageActivity{
		SessionID: "spawn", MessageID: "msg-old", ConvID: "ses-resume",
		ProviderID: "openai", ModelID: "gpt-a", ObservedAt: now.Add(-time.Hour),
	}))
	require.NoError(t, ReplaceOpenCodeUsageActivity("resume", "ses-resume", []OpenCodeUsageActivity{{
		SessionID: "resume", MessageID: "msg-old", ConvID: "ses-resume",
		ProviderID: "openai", ModelID: "gpt-b", ObservedAt: now.Add(-time.Hour),
	}}, now))

	got, err := OpenCodeUsageActivityBetween(now.Add(-2*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, got, 1, "resume replacement removes the spawn-session duplicate")
	assert.Equal(t, "resume", got[0].SessionID)
	assert.Equal(t, "gpt-b", got[0].ModelID)

	require.NoError(t, DeleteOpenCodeUsageActivity("ses-resume", "resume", "msg-old"))
	got, err = OpenCodeUsageActivityBetween(now.Add(-2*time.Hour), now)
	require.NoError(t, err)
	assert.Empty(t, got, "conversation-scoped removal clears activity from every local session ID")

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO opencode_usage_activity
		(session_id, message_id, conv_id, provider_id, model_id, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"spawn", "msg-expired", "ses-other", "openai", "gpt-a",
		now.Add(-OpenCodeUsageActivityRetention-time.Hour).Format(time.RFC3339Nano))
	require.NoError(t, err)
	require.NoError(t, UpsertOpenCodeUsageActivity(OpenCodeUsageActivity{
		SessionID: "live", MessageID: "msg-new", ConvID: "ses-live",
		ProviderID: "openai", ModelID: "gpt-a", ObservedAt: now,
	}))
	var expired int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM opencode_usage_activity
		WHERE message_id = 'msg-expired'`).Scan(&expired))
	assert.Zero(t, expired, "live upserts enforce the 90-day retention bound")
}
