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

	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, alias, role, descr, joined_at)
		VALUES (?, 'member-conv', 'm', '', '', '2020-01-01T00:00:00Z')`, gid)
	mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		VALUES (?, 'owner-conv', '2020-01-01T00:00:00Z', '')`, gid)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by)
		VALUES ('perm-conv', 'self.rename', '2020-01-01T00:00:00Z', '')`)
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old-conv', 'new-conv', 'reincarnate', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_workdir (conv_id, dir, updated_at, worktree_root, branch)
		VALUES ('workdir-conv', '/tmp', '2020-01-01T00:00:00Z', '', '')`)

	require.NoError(t, backfillAgentEnrollment(d), "backfillAgentEnrollment")

	// Every conv that appeared in an agentic table is now an active
	// agent — none of them vanished.
	for _, conv := range []string{
		"member-conv", "owner-conv", "perm-conv",
		"old-conv", "new-conv", "workdir-conv",
	} {
		st, serr := EnrollmentState(conv)
		require.NoError(t, serr, "EnrollmentState(%s)", conv)
		assert.Equal(t, EnrollmentActive, st,
			"conv %s appeared in an agentic table — backfill must enroll it", conv)
	}

	// A conv that was never agentic stays a plain conversation.
	st, serr := EnrollmentState("random-non-agent-conv")
	require.NoError(t, serr)
	assert.Equal(t, EnrollmentNone, st, "non-agentic conv must NOT be backfilled")

	// Idempotent: re-running the backfill changes nothing.
	require.NoError(t, backfillAgentEnrollment(d), "backfill re-run")
	active, lerr := ListActiveAgents()
	require.NoError(t, lerr)
	assert.Len(t, active, 6, "re-running the backfill must not duplicate rows")
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
	active, err := ListActiveAgents()
	require.NoError(t, err)
	retired, err := ListRetiredAgents()
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
