package harness

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A cloned rollout carries its source's token usage and sub-agent spawns.
// Everything observed at or before the inherited-history boundary is the
// source's spend; only what follows is the clone's.
func TestCodexTelemetryFollower_ExcludesInheritedHistoryCost(t *testing.T) {
	home := t.TempDir()
	const (
		rootID  = "019ec004-4250-79b1-9ade-ebaea4135501"
		childID = "019ec004-4250-79b1-9ade-ebaea4135502"
	)
	root := newFollowerTestRollout(t, home, rootID)
	appendRolloutEnvelope(t, root, "turn_context", map[string]any{"model": "gpt-5.6-terra"})
	appendTokenCount(t, root, 1000, 100, 1100)
	appendSubagentActivity(t, root, childID, "started", "")
	child := followerTestRolloutPath(t, home, childID)
	appendRolloutEnvelope(t, child, "session_meta", map[string]any{"id": childID})
	appendRolloutEnvelope(t, child, "turn_context", map[string]any{"model": "gpt-5.6-terra"})
	appendTokenCount(t, child, 1000, 100, 1100)

	var plain CodexTelemetryFollower
	whole, err := plain.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	require.True(t, whole.HasCost)
	perTurn := whole.Cost.CostUSD / 2 // the root's turn plus its child's

	boundary := time.Now()
	time.Sleep(time.Millisecond)

	follower := &CodexTelemetryFollower{}
	follower.SetCostInheritedUntil(boundary)
	inherited, err := follower.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	assert.False(t, inherited.HasCost, "nothing after the boundary yet")
	assert.Empty(t, inherited.CostHistory)
	assert.NotContains(t, inherited.DiscoveredSubagents, childID,
		"a sub-agent the source spawned is the source's")

	appendTokenCount(t, root, 1000, 100, 1100)
	own, err := follower.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	require.True(t, own.HasCost)
	assert.InDelta(t, perTurn, own.Cost.CostUSD, 1e-12, "only the clone's own turn")
	require.Len(t, own.CostHistory, 1)
	assert.InDelta(t, perTurn, own.CostHistory[0].CostUSD, 1e-12)

	// The boundary travels with the durable checkpoint.
	data, ok, err := follower.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	restored := &CodexTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(data))
	restored.SetCostInheritedUntil(boundary)
	appendTokenCount(t, root, 1000, 100, 1100)
	again, err := restored.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	assert.InDelta(t, 2*perTurn, again.Cost.CostUSD, 1e-12)
}

// Legacy token_count events carry only the cumulative total, so a later
// event's figure still includes the inherited spend; the clone reports the
// difference.
func TestCodexTelemetryFollower_ExcludesInheritedLegacyCumulativeCost(t *testing.T) {
	home := t.TempDir()
	const rootID = "019ec004-4250-79b1-9ade-ebaea4135503"
	legacy := func(path string, total int64) {
		appendRolloutEnvelope(t, path, "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage":    map[string]any{"input_tokens": total, "total_tokens": total},
				"model_context_window": 200000,
			},
		})
	}
	root := newFollowerTestRollout(t, home, rootID)
	appendRolloutEnvelope(t, root, "turn_context", map[string]any{"model": "gpt-5.6-terra"})
	legacy(root, 1000)
	boundary := time.Now()
	time.Sleep(time.Millisecond)
	legacy(root, 3000)

	var plain CodexTelemetryFollower
	whole, err := plain.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	follower := &CodexTelemetryFollower{}
	follower.SetCostInheritedUntil(boundary)
	own, err := follower.RuntimeTelemetry(home, rootID)
	require.NoError(t, err)
	require.True(t, own.HasCost)
	assert.InDelta(t, whole.Cost.CostUSD*2/3, own.Cost.CostUSD, 1e-12,
		"2000 of the 3000 cumulative tokens are the clone's")
}
