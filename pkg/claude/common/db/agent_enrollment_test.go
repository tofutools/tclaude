package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackfillAgentEnrollment is the upgrade-safety test the feature
// hinges on: a tclaude database that predates the enrollment model
// carries agentic rows (group memberships, grants, succession edges)
// but no agent_enrollment rows. The v29→v30 backfill must flag every
// one of those convs as an agent so none of them disappears off the
// roster when the user upgrades.
func TestBackfillAgentEnrollment(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Simulate a pre-v30 database: agentic rows exist, but the
	// enrollment table is empty. Raw INSERTs (not the Add*/Grant*
	// helpers) so the Go-level EnrollAgent triggers don't fire — this
	// is exactly the state an old DB is in right after the v30 table
	// is created but before the backfill runs.
	// A group for the membership / ownership rows to reference (the
	// FOREIGN KEY needs a real group). CreateAgentGroup has no
	// enrollment side-effect.
	gid, err := CreateAgentGroup("backfill-grp", "")
	require.NoError(t, err, "CreateAgentGroup")

	_, err = d.Exec(`DELETE FROM agent_enrollment`)
	require.NoError(t, err, "clear agent_enrollment")

	// Post-v73/v74 the membership/owner/permission tables (v73) and the
	// clone/spawn history + cron tables (v74) are agent-keyed, so this "every
	// conv in an agentic table gets enrolled" check uses the tables that remain
	// conv-keyed (head aliases, workdir, succession). The conv names are kept so
	// the assertions below are unchanged.
	mustExec(t, d, `INSERT INTO agent_head_aliases (handle, anchor_conv_id, created_at, by_conv)
		VALUES ('h-member', 'member-conv', '2020-01-01T00:00:00Z', '')`)
	mustExec(t, d, `INSERT INTO agent_workdir (conv_id, dir, updated_at, worktree_root, branch)
		VALUES ('owner-conv', '/tmp/o', '2020-01-01T00:00:00Z', '', '')`)
	mustExec(t, d, `INSERT INTO agent_head_aliases (handle, anchor_conv_id, created_at, by_conv)
		VALUES ('h-perm', 'perm-conv', '2020-01-01T00:00:00Z', '')`)
	_ = gid
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old-conv', 'new-conv', 'reincarnate', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_workdir (conv_id, dir, updated_at, worktree_root, branch)
		VALUES ('workdir-conv', '/tmp', '2020-01-01T00:00:00Z', '', '')`)
	// old-conv is ALSO referenced by an agentic table (a workdir row)
	// — yet it must still be excluded, since it has been superseded.
	// This pins the WHERE-clause exclusion (not just an omitted UNION
	// arm): a predecessor almost always shows up in agent_messages /
	// agent_workdir too.
	mustExec(t, d, `INSERT INTO agent_workdir (conv_id, dir, updated_at, worktree_root, branch)
		VALUES ('old-conv', '/tmp/old', '2020-01-01T00:00:00Z', '', '')`)

	require.NoError(t, backfillAgentEnrollment(d), "backfillAgentEnrollment")

	// Every live conv that appeared in an agentic table is now an
	// active agent — none of them vanished. new-conv is the head of
	// the succession chain, so it counts as live.
	for _, conv := range []string{
		"member-conv", "owner-conv", "perm-conv",
		"new-conv", "workdir-conv",
	} {
		st, serr := EnrollmentState(conv)
		require.NoError(t, serr, "EnrollmentState(%s)", conv)
		assert.Equal(t, EnrollmentActive, st,
			"conv %s appeared in an agentic table — backfill must enroll it", conv)
	}

	// old-conv is a superseded reincarnation predecessor — its identity
	// moved to new-conv. The backfill must NOT enroll it, even though it
	// has a workdir row, or it resurfaces as an un-retireable ghost.
	st, serr := EnrollmentState("old-conv")
	require.NoError(t, serr)
	assert.Equal(t, EnrollmentNone, st,
		"a superseded succession predecessor must NOT be backfilled into an agent")

	// A conv that was never agentic stays a plain conversation.
	st, serr = EnrollmentState("random-non-agent-conv")
	require.NoError(t, serr)
	assert.Equal(t, EnrollmentNone, st, "non-agentic conv must NOT be backfilled")

	// Idempotent: re-running the backfill changes nothing.
	require.NoError(t, backfillAgentEnrollment(d), "backfill re-run")
	active, lerr := listActiveEnrollments()
	require.NoError(t, lerr)
	assert.Len(t, active, 5, "re-running the backfill must not duplicate rows")
}

// TestMigrateV30toV31RemovesSupersededEnrollments reproduces the ghost-
// agent bug: a database upgraded by the original v29→v30 backfill has
// every reincarnation predecessor enrolled as an active agent. Those
// ghosts clutter the roster and can't be retired (the enrollment verbs
// redirect forward through the succession chain to the head). The
// v30→v31 migration must delete exactly the predecessors and leave the
// chain head — and every unrelated agent — untouched.
func TestMigrateV30toV31RemovesSupersededEnrollments(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// A 3-deep reincarnation chain: a → b → c (c is the live head).
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('chain-a', 'chain-b', 'reincarnate', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('chain-b', 'chain-c', 'reincarnate', '2020-01-02T00:00:00Z')`)

	// Simulate the buggy v30 state: every conv in the chain is enrolled
	// (the predecessors should never have been), plus one unrelated
	// agent and one retired predecessor.
	for _, conv := range []string{"chain-a", "chain-b", "chain-c", "loner"} {
		require.NoError(t, EnrollAgent(conv, "migration"))
	}
	_, err = RetireAgent("chain-a", "human", "")
	require.NoError(t, err, "retire chain-a (a retired ghost is still a ghost)")

	require.NoError(t, migrateV30toV31(d), "migrateV30toV31")

	// chain-a and chain-b are superseded predecessors — gone entirely.
	for _, conv := range []string{"chain-a", "chain-b"} {
		st, serr := EnrollmentState(conv)
		require.NoError(t, serr)
		assert.Equal(t, EnrollmentNone, st,
			"superseded predecessor %s must lose its enrollment row", conv)
	}
	// chain-c is the live head — never an old_conv_id — and stays.
	st, serr := EnrollmentState("chain-c")
	require.NoError(t, serr)
	assert.Equal(t, EnrollmentActive, st, "the chain head must keep its enrollment")
	// An unrelated agent is untouched.
	st, serr = EnrollmentState("loner")
	require.NoError(t, serr)
	assert.Equal(t, EnrollmentActive, st, "an unrelated agent must be untouched")

	// Idempotent: re-running deletes nothing more.
	require.NoError(t, migrateV30toV31(d), "migrateV30toV31 re-run")
	active, lerr := listActiveEnrollments()
	require.NoError(t, lerr)
	assert.Len(t, active, 2, "only chain-c + loner remain enrolled")
}

// TestEnrollmentLifecycle exercises the enroll → retire → reinstate
// state machine and the invariants the read paths depend on.
func TestEnrollmentLifecycle(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	// Unknown conv: no row.
	st, err := EnrollmentState("ghost")
	require.NoError(t, err)
	assert.Equal(t, EnrollmentNone, st)

	// Enroll → active. Idempotent.
	require.NoError(t, EnrollAgent("c1", "spawn"))
	require.NoError(t, EnrollAgent("c1", "cli"), "EnrollAgent must be idempotent")
	st, _ = EnrollmentState("c1")
	assert.Equal(t, EnrollmentActive, st)

	// Retire → retired; second retire is a no-op.
	did, err := RetireAgent("c1", "human", "done with it")
	require.NoError(t, err)
	assert.True(t, did, "first retire flips the bit")
	did, err = RetireAgent("c1", "human", "")
	require.NoError(t, err)
	assert.False(t, did, "retiring an already-retired agent is a no-op")
	st, _ = EnrollmentState("c1")
	assert.Equal(t, EnrollmentRetired, st)

	// EnrollAgent must NOT un-retire — only an explicit promote/reinstate does.
	require.NoError(t, EnrollAgent("c1", "cli"))
	st, _ = EnrollmentState("c1")
	assert.Equal(t, EnrollmentRetired, st, "a stray EnrollAgent must not resurrect a retired agent")

	// Reinstate → active; second reinstate is a no-op.
	did, err = ReinstateAgent("c1")
	require.NoError(t, err)
	assert.True(t, did)
	did, err = ReinstateAgent("c1")
	require.NoError(t, err)
	assert.False(t, did, "reinstating an already-active agent is a no-op")
	st, _ = EnrollmentState("c1")
	assert.Equal(t, EnrollmentActive, st)

	// PromoteAgent: none → active, and retired → active.
	prior, err := PromoteAgent("c2", "promote")
	require.NoError(t, err)
	assert.Equal(t, EnrollmentNone, prior, "promote of a fresh conv reports prior=none")
	_, _ = RetireAgent("c2", "human", "")
	prior, err = PromoteAgent("c2", "promote")
	require.NoError(t, err)
	assert.Equal(t, EnrollmentRetired, prior, "promote of a retired conv reports prior=retired")
	st, _ = EnrollmentState("c2")
	assert.Equal(t, EnrollmentActive, st, "promote always lands the conv active")

	// List surfaces split active vs retired.
	_, _ = RetireAgent("c2", "human", "")
	active, err := listActiveEnrollments()
	require.NoError(t, err)
	retired, err := listRetiredEnrollments()
	require.NoError(t, err)
	assert.Len(t, active, 1, "c1 active")
	assert.Len(t, retired, 1, "c2 retired")

	// DeleteEnrollment removes the row entirely.
	n, err := DeleteEnrollment("c1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	st, _ = EnrollmentState("c1")
	assert.Equal(t, EnrollmentNone, st)
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := d.Exec(q, args...)
	require.NoError(t, err, "exec failed: %s", q)
}
