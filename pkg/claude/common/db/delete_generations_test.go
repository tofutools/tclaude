package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteAgentByConvID_PredecessorKeepsLiveActor guards the JOH-26 delete
// semantics: identity is actor-level, so deleting a PAST conversation
// generation (a reincarnate / Claude Code /clear leaves the old conv around)
// must NOT wipe the live actor's memberships, permissions or actor row — only
// the live generation's delete tears the actor down.
func TestDeleteAgentByConvID_PredecessorKeepsLiveActor(t *testing.T) {
	setupTestDB(t)

	groupID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err, "CreateAgentGroup")

	// One actor, two generations: old → new (new is the live head).
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	// Actor-level identity: a membership and a permission override.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "new", Role: "lead"}))
	require.NoError(t, GrantAgentPermission("new", "self.compact", "test"))

	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// --- Delete the PREDECESSOR generation. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.GroupMembers, "predecessor delete must not touch actor-level memberships")
	assert.Zero(t, counts.Permissions, "predecessor delete must not touch actor-level permissions")

	// The actor and its identity survive; only the old generation is unlinked.
	still, err := GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, still, "live actor survives a predecessor delete")
	assert.Equal(t, "new", still.CurrentConvID, "live conv pointer is unchanged")

	oldA, err := AgentIDForConv("old")
	require.NoError(t, err)
	assert.Empty(t, oldA, "the predecessor generation is unlinked")
	newA, err := AgentIDForConv("new")
	require.NoError(t, err)
	assert.Equal(t, actor, newA, "the live generation still resolves to the actor")

	members, err := ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "the actor keeps its membership after a predecessor delete")

	// --- Delete the LIVE generation → the actor is fully torn down. ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.GroupMembers, "live-generation delete removes the actor's membership")

	gone, err := GetAgent(actor)
	require.NoError(t, err)
	assert.Nil(t, gone, "deleting the live generation removes the actor")

	members, err = ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Empty(t, members, "no memberships remain once the actor is gone")
}

// TestDeleteAgentByConvID_BridgesMiddleGenerationSuccession guards the routing
// invariant that deleting a MIDDLE predecessor generation must not strand an
// OLDER generation's stale-id forwarding. Stale ids forward through
// agent_conv_succession (not agent_conversations), so when generation B is
// removed from a chain A → B → C the deletion of the A→B and B→C edges has to be
// healed with a fresh A→C bridge — otherwise ResolveLatestConv(A) would return A
// instead of the live head C.
func TestDeleteAgentByConvID_BridgesMiddleGenerationSuccession(t *testing.T) {
	setupTestDB(t)

	// One actor, three generations: A → B → C (C the live head).
	_, _, err := EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("A", "B", "clear")
	require.NoError(t, err, "rotate A→B")
	_, err = RotateAgentConv("B", "C", "reincarnate")
	require.NoError(t, err, "rotate B→C")

	// Precondition: every generation's stale id forwards to the live head.
	require.Equal(t, "C", ResolveLatestConv("A"), "A forwards to head before any delete")
	require.Equal(t, "C", ResolveLatestConv("B"), "B forwards to head before any delete")

	actor, err := AgentIDForConv("C")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// --- Delete the MIDDLE generation B. ---
	_, err = DeleteAgentByConvID("B")
	require.NoError(t, err, "delete middle generation")

	// B is gone; A stays linked to the actor.
	bAgent, err := AgentIDForConv("B")
	require.NoError(t, err)
	assert.Empty(t, bAgent, "the middle generation is unlinked")
	aAgent, err := AgentIDForConv("A")
	require.NoError(t, err)
	assert.Equal(t, actor, aAgent, "the older generation stays linked to the actor")

	// The chain is healed: A still forwards to the live head C.
	assert.Equal(t, "C", ResolveLatestConv("A"), "A still forwards to the live head after the middle delete")
	succ, err := GetConvSuccessor("A")
	require.NoError(t, err)
	assert.Equal(t, "C", succ, "an A→C bridge edge was recorded")

	// No dangling edges reference the deleted middle generation.
	bs, err := GetConvSuccessor("B")
	require.NoError(t, err)
	assert.Empty(t, bs, "no edge out of the deleted generation")
	bp, err := GetConvPredecessor("B")
	require.NoError(t, err)
	assert.Empty(t, bp, "no edge into the deleted generation")

	// The live head and actor are untouched.
	head, err := GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.Equal(t, "C", head.CurrentConvID, "the live head is unchanged")
}

// TestDeleteAnchor_RebasesHeadAlias guards the JOH-330 corner: a head_alias is
// conv-anchored on a SPECIFIC generation and resolves to the live head by walking
// the succession chain forward from that anchor (ResolveHeadAlias →
// ResolveLatestConv). Deleting the alias's exact anchor (genesis) generation while
// the owning actor survives at a LATER generation used to strand the alias on the
// now-dead anchor — the conv-scoped loop wipes the anchor→succ edge, so
// ResolveLatestConv(anchor) returns the dead anchor instead of the live head. The
// delete path must rebase such an alias's anchor onto the surviving head, mirroring
// the middle-generation succession bridge.
func TestDeleteAnchor_RebasesHeadAlias(t *testing.T) {
	setupTestDB(t)

	// One actor, three generations: A → B → C (C the live head).
	_, _, err := EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("A", "B", "clear")
	require.NoError(t, err, "rotate A→B")
	_, err = RotateAgentConv("B", "C", "reincarnate")
	require.NoError(t, err, "rotate B→C")

	// An alias 'foo' anchored on the genesis generation A.
	require.NoError(t, SetHeadAlias("foo", "A", "A"), "SetHeadAlias foo→A")

	// Precondition: the alias resolves to the live head before any delete.
	got, err := ResolveHeadAlias("foo")
	require.NoError(t, err)
	require.Equal(t, "C", got, "alias forwards to head before any delete")

	actor, err := AgentIDForConv("C")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// --- Delete the alias's exact anchor generation A (actor survives at C). ---
	_, err = DeleteAgentByConvID("A")
	require.NoError(t, err, "delete anchor generation")

	// The alias must be rebased forward and still resolve to the live head C.
	got, err = ResolveHeadAlias("foo")
	require.NoError(t, err)
	assert.Equal(t, "C", got, "alias still resolves to the live head after its anchor is deleted")

	// The anchor row no longer points at the dead conv A.
	h, err := GetHeadAlias("foo")
	require.NoError(t, err)
	require.NotNil(t, h, "alias row survives the anchor delete")
	assert.NotEqual(t, "A", h.AnchorConvID, "anchor was rebased off the deleted conv")
	assert.Equal(t, actor, h.AnchorAgentID, "anchor_agent_id still names the surviving actor")
}

// TestDeleteMiddleAnchor_RebasesAliasAndBridges exercises the JOH-330 rebase and
// the JOH-26 middle-generation succession bridge together: when the deleted conv is
// BOTH a middle generation AND a head_alias anchor (A → B → C → D, alias anchored on
// B), deleting B must heal the A→C bridge AND forward the alias anchor onto B's
// immediate successor C, so the alias still chain-walks to the live head D.
func TestDeleteMiddleAnchor_RebasesAliasAndBridges(t *testing.T) {
	setupTestDB(t)

	// One actor, four generations: A → B → C → D (D the live head).
	_, _, err := EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("A", "B", "clear")
	require.NoError(t, err, "rotate A→B")
	_, err = RotateAgentConv("B", "C", "reincarnate")
	require.NoError(t, err, "rotate B→C")
	_, err = RotateAgentConv("C", "D", "clear")
	require.NoError(t, err, "rotate C→D")

	// An alias anchored on the MIDDLE generation B.
	require.NoError(t, SetHeadAlias("foo", "B", "B"), "SetHeadAlias foo→B")

	// --- Delete the middle generation B (which is also the alias anchor). ---
	_, err = DeleteAgentByConvID("B")
	require.NoError(t, err, "delete middle anchor generation")

	// The succession chain is healed: A bridges straight to C.
	succ, err := GetConvSuccessor("A")
	require.NoError(t, err)
	assert.Equal(t, "C", succ, "an A→C bridge edge was recorded")

	// The alias anchor was forwarded one hop to C and still resolves to head D.
	h, err := GetHeadAlias("foo")
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, "C", h.AnchorConvID, "anchor rebased onto B's immediate successor C")
	got, err := ResolveHeadAlias("foo")
	require.NoError(t, err)
	assert.Equal(t, "D", got, "alias still resolves to the live head after the middle-anchor delete")
}

// TestDeleteAnchor_RebasesAllAliasesOnSameAnchor covers the WHERE-anchor_conv_id
// fan-out: more than one handle can be anchored on the same conv, and deleting that
// conv must rebase every one of them, not just the first.
func TestDeleteAnchor_RebasesAllAliasesOnSameAnchor(t *testing.T) {
	setupTestDB(t)

	// One actor, three generations: A → B → C (C the live head).
	_, _, err := EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("A", "B", "clear")
	require.NoError(t, err, "rotate A→B")
	_, err = RotateAgentConv("B", "C", "reincarnate")
	require.NoError(t, err, "rotate B→C")

	// Two handles, both anchored on the genesis generation A.
	require.NoError(t, SetHeadAlias("foo", "A", "A"), "SetHeadAlias foo→A")
	require.NoError(t, SetHeadAlias("bar", "A", "A"), "SetHeadAlias bar→A")

	// --- Delete the shared anchor A. ---
	_, err = DeleteAgentByConvID("A")
	require.NoError(t, err, "delete shared anchor")

	// Both aliases are rebased off the dead anchor and still resolve to head C.
	for _, handle := range []string{"foo", "bar"} {
		h, err := GetHeadAlias(handle)
		require.NoError(t, err)
		require.NotNil(t, h, "alias %q survives the anchor delete", handle)
		assert.NotEqual(t, "A", h.AnchorConvID, "alias %q rebased off the deleted conv", handle)
		got, err := ResolveHeadAlias(handle)
		require.NoError(t, err)
		assert.Equal(t, "C", got, "alias %q still resolves to the live head", handle)
	}
}

// TestDeleteLiveHead_AliasBreaks pins the intended boundary of the JOH-330 fix:
// when the deleted conv is the actor's LAST/live generation (the whole actor is
// torn down), the alias has no surviving head to rebase onto and may legitimately
// break — the rebase is scoped to predecessor deletes only.
func TestDeleteLiveHead_AliasBreaks(t *testing.T) {
	setupTestDB(t)

	// A single-generation actor A, with an alias anchored on it.
	_, _, err := EnsureAgentForConv("A", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	require.NoError(t, SetHeadAlias("foo", "A", "A"), "SetHeadAlias foo→A")
	got, err := ResolveHeadAlias("foo")
	require.NoError(t, err)
	require.Equal(t, "A", got, "alias resolves to A before delete")

	// --- Delete the live head A: the whole actor is removed. ---
	_, err = DeleteAgentByConvID("A")
	require.NoError(t, err, "delete live head")

	// The alias row is left as-is (still pointing at the now-dead A) — there is no
	// surviving generation to rebase onto, so the alias legitimately goes stale.
	h, err := GetHeadAlias("foo")
	require.NoError(t, err)
	require.NotNil(t, h, "alias row is not rebased away when the whole actor is gone")
	assert.Equal(t, "A", h.AnchorConvID, "anchor is left on the deleted head (alias breaks by design)")
}

// TestDeleteAgentByConvID_CronJobsActorScoped guards the JOH-26 PR3a delete
// move: cron jobs are agent-keyed (owner_agent / target_agent), so they are
// torn down with the ACTOR (its current-generation delete), not on a
// predecessor delete — and the agent_cron_runs FK cascade still fires on the
// live schema (the RENAME-COLUMN migration preserved the FK).
func TestDeleteAgentByConvID_CronJobsActorScoped(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// One actor, two generations: old → new (new is the live head).
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")
	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// A self-nudge owned by the actor, and a manager's job that TARGETS the
	// actor — both keyed (after resolution) on the actor's agent_id.
	selfJob, err := InsertAgentCronJob(&AgentCronJob{
		Name: "self-nudge", OwnerConv: "new", TargetKind: CronTargetConv,
		TargetConv: "new", IntervalSeconds: 600, Body: "check", Enabled: true,
	})
	require.NoError(t, err, "insert self job")
	mgrJob, err := InsertAgentCronJob(&AgentCronJob{
		Name: "mgr-ping", OwnerConv: "mgr", TargetKind: CronTargetConv,
		TargetConv: "new", IntervalSeconds: 600, Body: "status?", Enabled: true,
	})
	require.NoError(t, err, "insert mgr job")
	// A run row on each, so we can prove the FK cascade fires (or doesn't).
	for _, id := range []int64{selfJob, mgrJob} {
		_, err := InsertAgentCronRun(&AgentCronRun{JobID: id, FiredAt: time.Now().UTC(), Status: "ok"})
		require.NoError(t, err, "insert run")
	}
	countRuns := func() int {
		var n int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_cron_runs`).Scan(&n))
		return n
	}
	require.Equal(t, 2, countRuns(), "two run rows seeded")

	// --- Delete the PREDECESSOR generation: jobs + runs must be untouched. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.CronJobsOwned, "predecessor delete leaves owned jobs")
	assert.Zero(t, counts.CronJobsTarget, "predecessor delete leaves targeting jobs")
	for _, id := range []int64{selfJob, mgrJob} {
		j, err := GetAgentCronJob(id)
		require.NoError(t, err)
		assert.NotNil(t, j, "job %d survives a predecessor delete", id)
	}
	assert.Equal(t, 2, countRuns(), "run history survives a predecessor delete")

	// --- Delete the LIVE generation: the actor's owned + targeting jobs go,
	// and their runs cascade-delete. ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.CronJobsOwned, "the actor's owned job is removed")
	assert.Equal(t, int64(1), counts.CronJobsTarget, "the job targeting the actor is removed")
	for _, id := range []int64{selfJob, mgrJob} {
		j, err := GetAgentCronJob(id)
		require.NoError(t, err)
		assert.Nil(t, j, "job %d is gone once the actor is torn down", id)
	}
	assert.Zero(t, countRuns(), "cron runs cascade-delete with their jobs (FK preserved)")
}

// TestDeleteAgentByConvID_ActorKeyedHistoryActorScoped guards the JOH-26 PR3d
// teardown completeness: the agent-keyed tables with NO FK to agents —
// agent_sudo_grants, agent_spawn_history, agent_clone_history — are torn down
// with the ACTOR (its current-generation delete), not on a predecessor delete,
// and not left orphaned when the `agents` row is deleted. (The `agents` delete
// cascades agent_conversations but NOT these FK-less tables, so they must be
// deleted explicitly — otherwise they become invisible residue the export/count
// paths can no longer see.)
func TestDeleteAgentByConvID_ActorKeyedHistoryActorScoped(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// One actor, two generations: old → new (new is the live head).
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")
	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	now := time.Now().UTC()
	// A sudo grant on the live generation, a spawn-history row, and a
	// clone-history row — all keyed (after resolution) on the actor's agent_id,
	// none with an FK to agents.
	_, err = InsertSudoGrant(&SudoGrant{
		ConvID: "new", Slug: "agent.spawn", GrantedAt: now,
		ExpiresAt: now.Add(time.Hour), GrantedBy: "human",
	})
	require.NoError(t, err, "insert sudo grant")
	require.NoError(t, ClaimSpawnSlot("new", 10, time.Hour, now), "claim spawn slot")
	require.NoError(t, ClaimCloneSlot("new", time.Minute, now), "claim clone slot")

	countAll := func() (sudo, spawn, clone int) {
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_sudo_grants WHERE agent_id = ?`, actor).Scan(&sudo))
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_spawn_history WHERE spawner_agent_id = ?`, actor).Scan(&spawn))
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_clone_history WHERE source_agent_id = ?`, actor).Scan(&clone))
		return
	}
	if s, sp, cl := countAll(); !assert.Equal(t, []int{1, 1, 1}, []int{s, sp, cl}, "one of each seeded") {
		t.FailNow()
	}

	// --- Delete the PREDECESSOR generation: history must be untouched. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.SudoGrants, "predecessor delete leaves sudo grants")
	assert.Zero(t, counts.SpawnHistory, "predecessor delete leaves spawn history")
	assert.Zero(t, counts.CloneHistory, "predecessor delete leaves clone history")
	s, sp, cl := countAll()
	assert.Equal(t, 1, s, "sudo grant survives a predecessor delete")
	assert.Equal(t, 1, sp, "spawn history survives a predecessor delete")
	assert.Equal(t, 1, cl, "clone history survives a predecessor delete")

	// --- Delete the LIVE generation: the actor's grants + history are torn
	// down (and not orphaned behind the deleted agents row). ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.SudoGrants, "live-gen delete removes the sudo grant")
	assert.Equal(t, int64(1), counts.SpawnHistory, "live-gen delete removes the spawn-history row")
	assert.Equal(t, int64(1), counts.CloneHistory, "live-gen delete removes the clone-history row")
	s, sp, cl = countAll()
	assert.Zero(t, s, "no sudo grant remains once the actor is gone")
	assert.Zero(t, sp, "no spawn-history row remains once the actor is gone")
	assert.Zero(t, cl, "no clone-history row remains once the actor is gone")
}
