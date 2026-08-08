package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// saveCopilotUsageSession seeds one Copilot sessions row through the production
// write path and returns its created_at, which every generation-guarded write
// below has to present.
func saveCopilotUsageSession(t *testing.T, id, convID string) time.Time {
	t.Helper()
	require.NoError(t, SaveSession(&SessionRow{
		ID:          id,
		TmuxSession: "tmux-" + id,
		ConvID:      convID,
		Cwd:         "/tmp/" + id,
		Status:      "running",
		Harness:     "copilot",
	}), "SaveSession %s", id)
	rows, err := ListSessions()
	require.NoError(t, err)
	for _, row := range rows {
		if row.ID == id {
			return row.CreatedAt
		}
	}
	t.Fatalf("seeded session %s not found", id)
	return time.Time{}
}

func sampleCopilotUsageSnapshot(sessionID, convID string) CopilotUsageSnapshot {
	nanoAIU := int64(4200)
	multiplier := 1.0
	return CopilotUsageSnapshot{
		SessionID: sessionID, ConvID: convID,
		LastEventID: 12, LastTurnIndex: 3,
		Model: "gpt-5", ReasoningEffort: "medium", FinishReason: "stop",
		Requests: 2, InputTokens: 53839, OutputTokens: 1000,
		CacheReadTokens: 48151, CacheWriteTokens: 3611, ReasoningTokens: 64,
		TotalNanoAIU: &nanoAIU, RequestMultiplier: &multiplier,
		LastCallInputTokens: 28725, LastCallOutputTokens: 700,
		LastCallCacheReadTokens: 25111, LastCallCacheWriteTokens: 3611,
		LastDurationMs: 900, LastTimeToFirstTokenMs: 120, LastInterTokenLatencyMs: 8,
		LastCallStamp: "2026-08-04T12:00:00Z",
		ObservedAt:    time.Now().UTC().Truncate(time.Second),
	}
}

func TestCopilotUsageSnapshotRoundTrip(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")

	want := sampleCopilotUsageSnapshot("s-copilot", "conv-1")
	saved, err := SaveCopilotUsageSnapshot(want, createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, int64(12), got.LastEventID)
	assert.Equal(t, CopilotUsageFoldVersion, got.FoldVersion)
	assert.Equal(t, int64(3), got.LastTurnIndex)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "medium", got.ReasoningEffort)
	assert.Equal(t, "stop", got.FinishReason)
	assert.Equal(t, int64(2), got.Requests)
	assert.Equal(t, int64(53839), got.InputTokens)
	assert.Equal(t, int64(3611), got.CacheWriteTokens)
	require.NotNil(t, got.TotalNanoAIU)
	assert.Equal(t, int64(4200), *got.TotalNanoAIU)
	require.NotNil(t, got.RequestMultiplier)
	assert.InDelta(t, 1.0, *got.RequestMultiplier, 1e-9)

	// The cumulative and per-call figures must stay distinguishable: the
	// per-call one is the live context numerator and the cumulative one is
	// session accounting, and confusing them renders an occupancy several
	// times the window.
	assert.Equal(t, int64(28725), got.LastCallInputTokens)
	assert.NotEqual(t, got.InputTokens, got.LastCallInputTokens)
	assert.Equal(t, "2026-08-04T12:00:00Z", got.LastCallStamp)
	assert.WithinDuration(t, want.ObservedAt, got.ObservedAt, time.Second)
}

func TestCopilotUsageSnapshotAbsentIsNotAnError(t *testing.T) {
	setupTestDB(t)
	got, err := LoadCopilotUsageSnapshot("s-never-polled")
	require.NoError(t, err, "a session the sweep has never seen is not a fault")
	assert.Nil(t, got, "nil is how the caller learns to start from cursor 0")

	got, err = LoadCopilotUsageSnapshot("")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestCopilotUsageSnapshotAdvancesCursor covers the ordinary steady state: the
// sweep folds more rows and replaces the row, cursor and all.
func TestCopilotUsageSnapshotAdvancesCursor(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")

	first := sampleCopilotUsageSnapshot("s-copilot", "conv-1")
	saved, err := SaveCopilotUsageSnapshot(first, createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	next := first
	next.LastEventID = 30
	next.Requests = 5
	next.OutputTokens = 2500
	next.LastCallInputTokens = 41000
	saved, err = SaveCopilotUsageSnapshot(next, createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(30), got.LastEventID)
	assert.Equal(t, int64(5), got.Requests)
	assert.Equal(t, int64(41000), got.LastCallInputTokens)
}

// TestCopilotUsageSnapshotRefusesStaleGeneration is the guard that keeps a
// recreated session id from inheriting the previous conversation's cursor —
// which would make the new conversation skip every row Copilot had already
// written for it.
func TestCopilotUsageSnapshotRefusesStaleGeneration(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")

	stale := sampleCopilotUsageSnapshot("s-copilot", "conv-1")

	// Same session id, different conversation: the generation the writer
	// observed is gone.
	saved, err := SaveCopilotUsageSnapshot(stale, createdAt.Add(-time.Hour))
	require.NoError(t, err, "a refused write is a normal outcome, not an error")
	assert.False(t, saved, "a stale created_at must not land")

	wrongConv := stale
	wrongConv.ConvID = "conv-2"
	saved, err = SaveCopilotUsageSnapshot(wrongConv, createdAt)
	require.NoError(t, err)
	assert.False(t, saved, "a conv id the session row does not carry must not land")

	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	assert.Nil(t, got, "no refused write may leave a row behind")
}

func TestCopilotUsageSnapshotIgnoresIncompleteIdentity(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")

	for _, tc := range []struct {
		name              string
		sessionID, convID string
	}{
		{"no session id", "", "conv-1"},
		{"no conv id", "s-copilot", ""},
		{"blank conv id", "s-copilot", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := sampleCopilotUsageSnapshot(tc.sessionID, tc.convID)
			saved, err := SaveCopilotUsageSnapshot(snapshot, createdAt)
			require.NoError(t, err)
			assert.False(t, saved)
		})
	}
}

func TestCopilotUsageSnapshotRejectsNegativeCursor(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")
	snapshot := sampleCopilotUsageSnapshot("s-copilot", "conv-1")
	snapshot.LastEventID = -1
	_, err := SaveCopilotUsageSnapshot(snapshot, createdAt)
	require.Error(t, err, "a negative cursor is a bug, not a degraded reading")
}

func TestDeleteCopilotUsageSnapshot(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")
	saved, err := SaveCopilotUsageSnapshot(
		sampleCopilotUsageSnapshot("s-copilot", "conv-1"), createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	require.NoError(t, DeleteCopilotUsageSnapshot("s-copilot"))
	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	assert.Nil(t, got)

	require.NoError(t, DeleteCopilotUsageSnapshot("s-copilot"),
		"deleting an absent snapshot is idempotent")
	require.NoError(t, DeleteCopilotUsageSnapshot(""))
}

// TestCopilotUsageSnapshotNilCostStaysNil keeps "Copilot said nothing" apart
// from "Copilot reported zero". A BYOK or mock provider legitimately bills
// zero, and collapsing the two would render a real zero as unknown or an
// unknown as a real zero.
func TestCopilotUsageSnapshotNilCostStaysNil(t *testing.T) {
	setupTestDB(t)
	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")

	silent := sampleCopilotUsageSnapshot("s-copilot", "conv-1")
	silent.TotalNanoAIU = nil
	silent.RequestMultiplier = nil
	saved, err := SaveCopilotUsageSnapshot(silent, createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.TotalNanoAIU)
	assert.Nil(t, got.RequestMultiplier)

	zero := int64(0)
	reported := silent
	reported.TotalNanoAIU = &zero
	saved, err = SaveCopilotUsageSnapshot(reported, createdAt)
	require.NoError(t, err)
	require.True(t, saved)

	got, err = LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.TotalNanoAIU, "a reported zero must survive as a value")
	assert.Zero(t, *got.TotalNanoAIU)
}
