package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditLog_InsertListPrune(t *testing.T) {
	setupTestDB(t)

	// Insert a spread of rows: two successes and one denial, across the
	// two surfaces. At is set explicitly so the prune cutoff is
	// deterministic (old rows fall away, recent ones stay).
	old := time.Now().Add(-90 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	_, err := InsertAuditLog(AuditLogEntry{
		At: old, ActorKind: AuditActorHuman, ActorLabel: "human",
		Verb: "spawn", TargetLabel: "worker-1", GroupName: "crew",
		Detail: "worker-1", Method: "POST", Path: "/v1/groups/crew/spawn",
		Status: 200, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		At: recent, ActorKind: AuditActorAgent, ActorConv: "conv-aaaa", ActorLabel: "po",
		Verb: "message", TargetConv: "conv-bbbb", TargetLabel: "worker-1",
		Detail: "rebasing now…", Method: "POST", Path: "/v1/messages",
		Status: 200, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		At: recent, ActorKind: AuditActorAgent, ActorConv: "conv-cccc", ActorLabel: "worker-2",
		Verb: "retire", TargetConv: "conv-bbbb", TargetLabel: "worker-1",
		Method: "POST", Path: "/v1/agent/worker-1/retire",
		Status: 403, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	// List all — newest first by id.
	all, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "retire", all[0].Verb, "newest row (highest id) comes first")
	assert.Equal(t, "spawn", all[2].Verb, "oldest insert is last")
	// Round-trip a denormalized field + status.
	assert.Equal(t, "worker-1", all[2].TargetLabel)
	assert.Equal(t, 403, all[0].Status)

	// Verb filter.
	msgs, err := ListAuditLog(AuditLogFilter{Verb: "message"})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "rebasing now…", msgs[0].Detail)

	// Outcome filter: failures only.
	fails, err := ListAuditLog(AuditLogFilter{Outcome: "failure"})
	require.NoError(t, err)
	require.Len(t, fails, 1)
	assert.Equal(t, "retire", fails[0].Verb)

	// Outcome filter: successes only.
	oks, err := ListAuditLog(AuditLogFilter{Outcome: "success"})
	require.NoError(t, err)
	assert.Len(t, oks, 2)

	// Limit.
	one, err := ListAuditLog(AuditLogFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "retire", one[0].Verb)

	// Prune everything older than 30 days: the old spawn row goes, the
	// two recent rows remain.
	removed, err := PruneAuditLog(time.Now().Add(-30 * 24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	n, err := CountAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	remaining, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	for _, e := range remaining {
		assert.NotEqual(t, "spawn", e.Verb, "the old spawn row should have been pruned")
	}
}

// TestAuditLog_AgentColumnsSurfaced verifies the read surfaces the PR4
// dual-written actor_agent / target_agent columns (PR3c-web) — so the
// dashboard Audit tab can render the actor/target by the stable, rotation-
// immune agent_id, and the human can search the trail by it.
func TestAuditLog_AgentColumnsSurfaced(t *testing.T) {
	setupTestDB(t)

	const actorConv = "actor-conv-1111"
	const targetConv = "target-conv-2222"
	actorAgent, _, err := EnsureAgentForConv(actorConv, "spawn")
	require.NoError(t, err, "mint actor")
	targetAgent, _, err := EnsureAgentForConv(targetConv, "spawn")
	require.NoError(t, err, "mint target")
	require.NotEmpty(t, actorAgent)
	require.NotEmpty(t, targetAgent)

	_, err = InsertAuditLog(AuditLogEntry{
		At: time.Now(), ActorKind: AuditActorAgent, ActorConv: actorConv, ActorLabel: "po",
		Verb: "message", TargetConv: targetConv, TargetLabel: "worker",
		Method: "POST", Path: "/v1/messages", Status: 200, Source: AuditSourceCLI,
	})
	require.NoError(t, err, "InsertAuditLog")

	rows, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err, "ListAuditLog")
	require.Len(t, rows, 1)
	assert.Equal(t, actorAgent, rows[0].ActorAgent, "actor_agent dual-write surfaced by the read")
	assert.Equal(t, targetAgent, rows[0].TargetAgent, "target_agent dual-write surfaced by the read")

	// The trail is searchable by the stable id.
	hit, err := ListAuditLog(AuditLogFilter{Search: actorAgent})
	require.NoError(t, err, "search by agent_id")
	require.Len(t, hit, 1, "audit search matches the stable agent_id")
}

func TestAuditLog_ExplicitAgentIdentityPrecedesConvLookup(t *testing.T) {
	setupTestDB(t)

	derivedActor, _, err := EnsureAgentForConv("actor-conv", "test")
	require.NoError(t, err)
	derivedTarget, _, err := EnsureAgentForConv("target-conv", "test")
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		ActorKind:   AuditActorAgent,
		ActorConv:   "actor-conv",
		ActorAgent:  "agt_captured_actor",
		Verb:        "approval.approve-always",
		TargetConv:  "target-conv",
		TargetAgent: "agt_captured_target",
		Source:      AuditSourcePopup,
	})
	require.NoError(t, err)

	rows, err := ListAuditLog(AuditLogFilter{Verb: "approval.approve-always"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "agt_captured_actor", rows[0].ActorAgent)
	assert.Equal(t, "agt_captured_target", rows[0].TargetAgent)
	assert.NotEqual(t, derivedActor, rows[0].ActorAgent, "explicit actor must not be replaced from conv metadata")
	assert.NotEqual(t, derivedTarget, rows[0].TargetAgent, "explicit target must not be replaced from conv metadata")
}

// TestAuditLog_SearchSortPaginate exercises the server-side query knobs
// the dashboard relies on: substring search, whitelisted sort, and
// limit/offset pagination.
func TestAuditLog_SearchSortPaginate(t *testing.T) {
	setupTestDB(t)

	// Insert in a known order so id (= insert order) is deterministic.
	rows := []AuditLogEntry{
		{ActorLabel: "po", Verb: "spawn", TargetLabel: "alpha", GroupName: "crew", Status: 200, Source: AuditSourceCLI},
		{ActorLabel: "po", Verb: "message", TargetLabel: "beta", Detail: "ship it", Status: 200, Source: AuditSourceCLI},
		{ActorLabel: "operator", Verb: "retire", TargetLabel: "beta", Status: 403, Source: AuditSourceDashboard},
		{ActorLabel: "worker", Verb: "rename", TargetLabel: "gamma", Detail: "→ delta", Status: 200, Source: AuditSourceCLI},
	}
	for _, e := range rows {
		_, err := InsertAuditLog(e)
		require.NoError(t, err)
	}

	// Search matches across columns (target label "beta") — 2 rows.
	beta, err := ListAuditLog(AuditLogFilter{Search: "beta"})
	require.NoError(t, err)
	assert.Len(t, beta, 2)
	n, err := CountAuditLog(AuditLogFilter{Search: "beta"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Search matches detail text too.
	shipit, err := ListAuditLog(AuditLogFilter{Search: "ship"})
	require.NoError(t, err)
	require.Len(t, shipit, 1)
	assert.Equal(t, "message", shipit[0].Verb)

	// Sort by verb ascending: message, rename, retire, spawn.
	byVerb, err := ListAuditLog(AuditLogFilter{SortBy: AuditSortVerb, Asc: true})
	require.NoError(t, err)
	require.Len(t, byVerb, 4)
	assert.Equal(t, []string{"message", "rename", "retire", "spawn"},
		[]string{byVerb[0].Verb, byVerb[1].Verb, byVerb[2].Verb, byVerb[3].Verb})

	// Pagination: page size 2, offset 2 → the 3rd+4th newest rows.
	pageDesc, err := ListAuditLog(AuditLogFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Len(t, pageDesc, 2)
	assert.Equal(t, "rename", pageDesc[0].Verb, "newest first by id")
	page2, err := ListAuditLog(AuditLogFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Equal(t, "spawn", page2[1].Verb, "last page ends on the oldest row")

	// A search wildcard char is matched literally, not as a LIKE wildcard.
	pct, err := CountAuditLog(AuditLogFilter{Search: "%"})
	require.NoError(t, err)
	assert.Equal(t, 0, pct, "a literal %% must not match every row")
}

// TestAuditLog_AtDefaultsToNow verifies a zero At is stamped at insert
// time rather than written as the zero value.
func TestAuditLog_AtDefaultsToNow(t *testing.T) {
	setupTestDB(t)

	before := time.Now().Add(-time.Second)
	_, err := InsertAuditLog(AuditLogEntry{Verb: "spawn", Source: AuditSourceCLI})
	require.NoError(t, err)

	rows, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].At.After(before), "zero At should be stamped to now at insert")
}
