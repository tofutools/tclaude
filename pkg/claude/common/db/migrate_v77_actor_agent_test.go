package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV76toV77_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts every v77 agent-companion column is present. v77 is head, so
// the literal currentVersion tripwire lives here now (moved forward from v76).
func TestMigrateV76toV77_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 77, currentVersion, "tripwire: bump this and add a v77→v78 test when you add a migration")

	for _, spec := range v77AgentColumns {
		has, err := columnExists(d, spec.table, spec.agentCol)
		require.NoError(t, err, "columnExists %s.%s", spec.table, spec.agentCol)
		assert.Truef(t, has, "%s carries the agent companion column %s", spec.table, spec.agentCol)
	}
}

// TestMigrateV76toV77_BackfillsAgentRefs drives the real v76→v77 migration over
// hand-seeded v76-shaped rows in representative author + owner tables: every
// non-empty conv ref must resolve to that conv's owning actor, an unmapped /
// non-actor conv must stay '', a successor and its predecessor must collapse to
// the SAME agent_id (the stable identity), and a re-run must change nothing.
func TestMigrateV76toV77_BackfillsAgentRefs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Two actors. actorA spans two generations (g0 → g1, the head); actorB is a
	// single conv. 'plain' is a conv that was never an agent.
	agentB, _, err := EnsureAgentForConv("convB", "spawn")
	require.NoError(t, err, "EnsureAgentForConv convB")
	agentA, _, err := EnsureAgentForConv("g1", "spawn")
	require.NoError(t, err, "EnsureAgentForConv g1")
	require.NoError(t, LinkConvToAgent("g0", agentA, ConvRoleGeneration, "test"), "link g0")

	// Reshape the tables-under-test back to their v76 (pre-companion) form and pin
	// the version, then seed rows in that shape. Other tables keep their v77
	// columns — the migration's column-exists probe skips re-adding them, and its
	// backfill is a no-op over their empty contents.
	for _, s := range []string{
		`ALTER TABLE audit_log DROP COLUMN actor_agent`,
		`ALTER TABLE audit_log DROP COLUMN target_agent`,
		`ALTER TABLE sessions DROP COLUMN agent_id`,
		`ALTER TABLE agent_conv_succession DROP COLUMN agent_id`,
		`UPDATE schema_version SET version = 76`,
	} {
		mustExec(t, d, s)
	}

	// audit_log: actor is a predecessor generation of actorA; target is actorB.
	mustExec(t, d, `INSERT INTO audit_log (at, actor_conv, target_conv, verb)
		VALUES ('2020-01-01T00:00:00Z', 'g0', 'convB', 'spawn')`)
	// audit_log: a target that is not an actor stays ''.
	mustExec(t, d, `INSERT INTO audit_log (at, actor_conv, target_conv, verb)
		VALUES ('2020-01-02T00:00:00Z', 'convB', 'plain', 'message')`)
	// sessions: a session running the head generation g1.
	mustExec(t, d, `INSERT INTO sessions (id, conv_id, created_at, updated_at)
		VALUES ('s1', 'g1', '2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z')`)
	// agent_conv_succession: g0 → g1, both actorA; resolves via either conv.
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, succeeded_at)
		VALUES ('g0', 'g1', '2020-01-01T00:00:00Z')`)

	require.NoError(t, migrateV76toV77(d), "v76→v77 backfill")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 77, ver, "version advanced")

	// audit_log author/target resolve to their actors.
	var actorAgent, targetAgent string
	require.NoError(t, d.QueryRow(
		`SELECT actor_agent, target_agent FROM audit_log WHERE verb = 'spawn'`).Scan(&actorAgent, &targetAgent))
	assert.Equal(t, agentA, actorAgent, "actor conv (predecessor) backfills to its actor")
	assert.Equal(t, agentB, targetAgent, "target conv backfills to its actor")

	require.NoError(t, d.QueryRow(
		`SELECT target_agent FROM audit_log WHERE verb = 'message'`).Scan(&targetAgent))
	assert.Equal(t, "", targetAgent, "non-actor target leaves the agent column empty")

	// sessions owner resolves to the running actor.
	var sessAgent string
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM sessions WHERE id = 's1'`).Scan(&sessAgent))
	assert.Equal(t, agentA, sessAgent, "session conv backfills to its owning actor")

	// succession resolves via COALESCE(new, old) — both are actorA.
	var succAgent string
	require.NoError(t, d.QueryRow(
		`SELECT agent_id FROM agent_conv_succession WHERE old_conv_id = 'g0'`).Scan(&succAgent))
	assert.Equal(t, agentA, succAgent, "succession edge resolves to the rotating actor")

	// Idempotent: a re-run recomputes the same join and changes nothing.
	require.NoError(t, migrateV76toV77(d), "v76→v77 re-run is a clean no-op")
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM sessions WHERE id = 's1'`).Scan(&sessAgent))
	assert.Equal(t, agentA, sessAgent, "re-run leaves the backfilled agent intact")
}

// TestDualWrite_AuditLog pins the insert-time dual-write: a freshly logged audit
// row has actor_agent / target_agent DERIVED from its conv columns, with a
// non-actor target left ''.
func TestDualWrite_AuditLog(t *testing.T) {
	setupTestDB(t)

	actor, _, err := EnsureAgentForConv("actorConv", "spawn")
	require.NoError(t, err)
	target, _, err := EnsureAgentForConv("targetConv", "spawn")
	require.NoError(t, err)

	d, err := Open()
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		ActorKind: AuditActorAgent, ActorConv: "actorConv",
		TargetConv: "targetConv", Verb: "message",
	})
	require.NoError(t, err, "InsertAuditLog")

	var actorAgent, targetAgent string
	require.NoError(t, d.QueryRow(
		`SELECT actor_agent, target_agent FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&actorAgent, &targetAgent))
	assert.Equal(t, actor, actorAgent, "actor_agent dual-written from actor_conv")
	assert.Equal(t, target, targetAgent, "target_agent dual-written from target_conv")

	// A non-actor target leaves target_agent ''.
	_, err = InsertAuditLog(AuditLogEntry{
		ActorKind: AuditActorAgent, ActorConv: "actorConv",
		TargetConv: "not-an-agent", Verb: "message",
	})
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(
		`SELECT actor_agent, target_agent FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&actorAgent, &targetAgent))
	assert.Equal(t, actor, actorAgent)
	assert.Equal(t, "", targetAgent, "non-actor target leaves target_agent empty")
}

// TestDualWrite_Succession pins that a recorded succession edge derives its
// agent_id from the (always-enrolled) predecessor at write time.
func TestDualWrite_Succession(t *testing.T) {
	setupTestDB(t)

	agentA, _, err := EnsureAgentForConv("oldGen", "spawn")
	require.NoError(t, err)

	// newGen is not yet linked; the edge must still resolve via oldGen.
	require.NoError(t, RecordConvSuccession("oldGen", "newGen", "reincarnate"))

	d, err := Open()
	require.NoError(t, err)
	var succAgent string
	require.NoError(t, d.QueryRow(
		`SELECT agent_id FROM agent_conv_succession WHERE old_conv_id = 'oldGen'`).Scan(&succAgent))
	assert.Equal(t, agentA, succAgent, "succession agent_id derived via the enrolled predecessor")
}

// TestPropagation_Sessions pins the enrollment-time propagation: a session row
// written before its conv enrolls has agent_id '' at insert, then gets filled
// when EnsureAgentForConv links the conv.
func TestPropagation_Sessions(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	require.NoError(t, SaveSession(&SessionRow{
		ID: "sess-x", ConvID: "convX", CreatedAt: now, UpdatedAt: now,
	}), "SaveSession before enrollment")

	d, err := Open()
	require.NoError(t, err)

	// Before enrollment the derivation yields '' (the conv has no agent yet).
	var agentID string
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM sessions WHERE id = 'sess-x'`).Scan(&agentID))
	require.Equal(t, "", agentID, "no agent before the conv enrolls")

	// Enrolling the conv propagates the agent onto the pre-existing session row.
	agent, _, err := EnsureAgentForConv("convX", "spawn")
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM sessions WHERE id = 'sess-x'`).Scan(&agentID))
	assert.Equal(t, agent, agentID, "enrollment propagates agent_id onto the earlier session row")
}
