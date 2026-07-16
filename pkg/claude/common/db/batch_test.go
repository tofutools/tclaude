package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentGroupBatchLoaders(t *testing.T) {
	setupTestDB(t)
	alpha, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	beta, err := CreateAgentGroup("beta", "")
	require.NoError(t, err)
	empty, err := CreateAgentGroup("empty", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: alpha, ConvID: "member-a", Role: "builder", Descr: "alpha member",
	}))
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{
		GroupID: beta, ConvID: "member-b", Role: "reviewer", Descr: "beta member",
	}))
	require.NoError(t, AddAgentGroupOwner(alpha, "owner-a", "test"))
	require.NoError(t, AddAgentGroupOwner(beta, "owner-b", "test"))

	members, err := ListAgentGroupMembersBatch([]int64{beta, empty, alpha})
	require.NoError(t, err)
	require.Len(t, members[alpha], 1)
	assert.Equal(t, "member-a", members[alpha][0].ConvID)
	require.Len(t, members[beta], 1)
	assert.Equal(t, "member-b", members[beta][0].ConvID)
	assert.Empty(t, members[empty])

	owners, err := ListAgentGroupOwnersBatch([]int64{beta, empty, alpha})
	require.NoError(t, err)
	require.Len(t, owners[alpha], 1)
	assert.Equal(t, "owner-a", owners[alpha][0].ConvID)
	require.Len(t, owners[beta], 1)
	assert.Equal(t, "owner-b", owners[beta][0].ConvID)
	assert.Empty(t, owners[empty])

	noMembers, err := ListAgentGroupMembersBatch(nil)
	require.NoError(t, err)
	assert.Empty(t, noMembers)
	noOwners, err := ListAgentGroupOwnersBatch(nil)
	require.NoError(t, err)
	assert.Empty(t, noOwners)
}

func TestAgentsByConvCarriesActorLifecycleState(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("old-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, LinkConvToAgent("current-conv", agentID, ConvRoleHead, "reincarnate"))
	moved, err := SetAgentCurrentConv(agentID, "old-conv", "current-conv")
	require.NoError(t, err)
	require.True(t, moved)

	rows, err := AgentsByConv([]string{"old-conv", "current-conv", "plain-conv"})
	require.NoError(t, err)
	assert.Equal(t, "current-conv", rows["old-conv"].CurrentConvID,
		"a predecessor resolves to the actor's current generation")
	assert.False(t, rows["old-conv"].Retired)
	assert.Equal(t, "current-conv", rows["current-conv"].CurrentConvID)
	assert.NotContains(t, rows, "plain-conv")

	ok, err := RetireAgentByID(agentID, "human", "done")
	require.NoError(t, err)
	require.True(t, ok)
	rows, err = AgentsByConv([]string{"old-conv", "current-conv"})
	require.NoError(t, err)
	assert.True(t, rows["old-conv"].Retired)
	assert.True(t, rows["current-conv"].Retired)
}

func TestAgentsByConvMarksSupersededOrphanActor(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, RecordConvSuccession("predecessor", "successor", "reincarnate"))
	_, err := AllocateAgent("predecessor", "legacy-backfill")
	require.NoError(t, err)

	rows, err := AgentsByConv([]string{"predecessor", "successor"})
	require.NoError(t, err)
	assert.True(t, rows["predecessor"].Superseded,
		"an old_conv_id remains superseded even if a legacy backfill gave it a separate actor")
	assert.False(t, rows["successor"].Superseded)
}

func TestLoadSessionsByIDs(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{
		ID:          "pending-alpha",
		TmuxSession: "tmux-alpha",
		Cwd:         "/tmp/alpha",
		Status:      "starting",
		Harness:     "codex",
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID:          "pending-beta",
		TmuxSession: "tmux-beta",
		Cwd:         "/tmp/beta",
		Status:      "idle",
		Harness:     "claude",
	}))

	rows, err := LoadSessionsByIDs([]string{"pending-beta", "missing", "pending-alpha"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "tmux-alpha", rows["pending-alpha"].TmuxSession)
	assert.Equal(t, "/tmp/alpha", rows["pending-alpha"].Cwd)
	assert.Equal(t, "codex", rows["pending-alpha"].Harness)
	assert.Equal(t, "idle", rows["pending-beta"].Status)
	assert.NotContains(t, rows, "missing")

	empty, err := LoadSessionsByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestCanonicalAgeTimestamp_PreservesPrecision pins the wire representation,
// which is deliberately ordinary UTC RFC3339Nano. Age consumers compare parsed
// instants rather than relying on the strings to have a sortable fixed width.
func TestCanonicalAgeTimestamp_PreservesPrecision(t *testing.T) {
	whole := CanonicalAgeTimestamp("2026-06-18T12:00:00Z")
	frac := CanonicalAgeTimestamp("2026-06-18T12:00:00.5Z")

	assert.Equal(t, "2026-06-18T12:00:00Z", whole)
	assert.Equal(t, "2026-06-18T12:00:00.5Z", frac,
		"full sub-second precision is preserved, never truncated to seconds")
}

// TestCanonicalAgeTimestamp_ZoneAndEdgeCases pins the zone canonicalisation
// (agents.created_at is written in the daemon's LOCAL zone) plus the empty and
// unparseable inputs.
func TestCanonicalAgeTimestamp_ZoneAndEdgeCases(t *testing.T) {
	// A non-UTC offset is normalised to UTC so values from different sources sort
	// in one zone.
	assert.Equal(t, "2026-06-18T10:00:00Z",
		CanonicalAgeTimestamp("2026-06-18T12:00:00+02:00"))

	assert.Equal(t, "", CanonicalAgeTimestamp(""), "empty stays empty")
	assert.Equal(t, "not-a-time", CanonicalAgeTimestamp("not-a-time"),
		"unparseable is returned unchanged, not blanked")
}

// TestCanonicalAgeTimestampFromTime pins that the time.Time formatter (the CLI
// actor path) produces exactly what CanonicalAgeTimestamp produces for the
// same instant, so the dashboard and CLI Age values are byte-identical.
func TestCanonicalAgeTimestampFromTime(t *testing.T) {
	assert.Equal(t, "", CanonicalAgeTimestampFromTime(time.Time{}), "zero time yields empty Age")

	instant := time.Date(2026, 6, 18, 12, 0, 0, 500_000_000, time.UTC)
	assert.Equal(t,
		CanonicalAgeTimestamp(instant.Format(time.RFC3339Nano)),
		CanonicalAgeTimestampFromTime(instant),
		"string and time.Time canonicalisers agree byte-for-byte")
}

func TestEarliestAgeTimestamp(t *testing.T) {
	assert.Equal(t, "2020-01-02T10:00:00.25Z", EarliestAgeTimestamp(
		"2026-07-14T12:00:00Z", // actor row stamped by a later backfill
		"2020-01-02T12:00:00.25+02:00",
	), "older conversation creation repairs a late actor enrollment time")

	assert.Equal(t, "2020-01-02T10:00:00Z", EarliestAgeTimestamp(
		"2020-01-02T10:00:00Z", // stable actor birth
		"2026-07-14T12:00:00Z", // later reincarnated conversation
	), "actor birth remains the Age across later conversation generations")

	assert.Equal(t, "2020-01-02T10:00:00Z",
		EarliestAgeTimestamp("bad-actor-time", "2020-01-02T10:00:00Z"),
		"a valid source wins over an unparseable one")
	assert.Equal(t, "bad-actor-time", EarliestAgeTimestamp("bad-actor-time", ""),
		"the first non-empty invalid value is preserved for diagnostics")
	assert.Empty(t, EarliestAgeTimestamp("", ""))
}
