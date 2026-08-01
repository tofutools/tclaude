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
		dbTime(now.Add(-OpenCodeUsageActivityRetention-time.Hour)))
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

func TestOpenCodePricingStepRemovalFollowsConversationClearsActivityAndExpires(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, UpsertOpenCodeUsageActivity(OpenCodeUsageActivity{
		SessionID: "spawn", MessageID: "msg-removed", ConvID: "ses-resume",
		ProviderID: "openai", ModelID: "gpt-a", ObservedAt: now,
	}))
	require.NoError(t, MarkOpenCodePricingStepsRemoved(
		"ses-resume", "resume", "msg-removed", now,
	))

	activity, err := OpenCodeUsageActivityBetween(now.Add(-time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, activity, "final-step removal clears provider Usage coverage across local resumes")
	removed, err := OpenCodePricingStepsRemoved("ses-resume", now)
	require.NoError(t, err)
	assert.True(t, removed["msg-removed"], "conversation-keyed removal survives a local session ID change")

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO opencode_usage_step_removals
		(conv_id, message_id, removed_at) VALUES (?, ?, ?)`,
		"ses-resume", "msg-expired",
		dbTime(now.Add(-OpenCodeUsageActivityRetention-time.Hour)))
	require.NoError(t, err)
	removed, err = OpenCodePricingStepsRemoved("ses-resume", now)
	require.NoError(t, err)
	assert.False(t, removed["msg-expired"], "removal markers share the 90-day activity boundary")

	require.NoError(t, ClearOpenCodePricingStepsRemoved("ses-resume", "msg-removed"))
	removed, err = OpenCodePricingStepsRemoved("ses-resume", now)
	require.NoError(t, err)
	assert.Empty(t, removed, "a later eligible step clears the final-step marker")
}
