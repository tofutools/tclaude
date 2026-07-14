package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a group's member listing defaults to newest-first by its earliest
// known existence (normally agents.created_at, the actor birth time). The default
// is shared by the CLI (`tclaude agent groups members`, via
// GET /v1/groups/{name}/members) and the browser dashboard, whose
// client-side column sort falls back to this server order when no column
// is active.
//
// Four properties this pins:
//   - the Age sort keys on the actor birth time when it predates the current
//     conversation's conv_index.Created — the conv_index times below are
//     deliberately scrambled into a different order on the following day;
//   - the backend returns each member's creation timestamp (created_at), and
//     owners who are not members carry it too and interleave by age — they no
//     longer land at the tail in random map iteration order;
//   - the actor timestamps mix whole-second and fractional precision inside
//     one wall-clock second, so the test can only pass when Age is parsed and
//     compared as time rather than ordered lexically;
//   - the conv-ids and join order are chosen so neither the join order nor
//     conv-id order matches the wanted age order.
func TestGroupMembers_DefaultSortByAgeNewestFirst(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	// conv-ids are intentionally in ascending lexical order while their
	// creation times are NOT, so a join_at/conv_id ordering would come
	// out [old, mid, new, owner] — the opposite of newest-first.
	const (
		convOld   = "aaaa1111-1111-1111-1111-111111111111"
		convMid   = "bbbb2222-2222-2222-2222-222222222222"
		convNew   = "cccc3333-3333-3333-3333-333333333333"
		convOwner = "dddd4444-4444-4444-4444-444444444444" // owner, not member
	)
	// conv_index Created is seeded in a DELIBERATELY WRONG order (reversed vs the
	// wanted age order) to prove the Age sort ignores it in favour of the actor
	// birth time set below. If the sort ever regressed to conv_index.Created, the
	// order would come out [old, mid, owner, new] and the assertions would fail.
	mustHaveConvCreated(t, convOld, "old-worker", "2026-06-19T23:00:00Z")
	mustHaveConvCreated(t, convMid, "mid-worker", "2026-06-19T20:00:00Z")
	mustHaveConvCreated(t, convNew, "new-worker", "2026-06-19T01:00:00Z")
	mustHaveConvCreated(t, convOwner, "owner-conv", "2026-06-19T02:00:00Z")

	g := f.HaveGroup("alpha")
	// Insert in oldest→newest order so join order is the reverse of the
	// wanted newest-first order.
	f.HaveMember("alpha", convOld)
	f.HaveMember("alpha", convMid)
	f.HaveMember("alpha", convNew)
	// An owner who is not a member — must interleave by age (between mid
	// and new), not land at the tail.
	require.NoError(t, db.AddAgentGroupOwner(g.ID, convOwner, "test"))

	// The actual sort key: the actor's birth time (agents.created_at). Three rows
	// deliberately share one wall-clock second with mixed fractional precision.
	// Lexical RFC3339Nano ordering would put convMid's whole-second "...00Z"
	// ahead of the newer fractional values; parsed-time ordering yields the
	// intended new > owner > mid order. These are backdated AFTER enrollment.
	mustSetAgentCreated(t, convOld, "2026-06-18T11:59:59.999999999Z")
	mustSetAgentCreated(t, convMid, "2026-06-18T12:00:00Z")
	mustSetAgentCreated(t, convNew, "2026-06-18T12:00:00.004000001Z")
	mustSetAgentCreated(t, convOwner, "2026-06-18T14:00:00.002+02:00")

	// Newest first; owner (+2ms) sits between new (+4.000001ms) and mid.
	wantConvs := []string{convNew, convOwner, convMid, convOld}

	// Surface 1: GET /v1/groups/{name}/members — the CLI listing.
	members := f.ListGroupMembers("alpha")
	gotV1 := make([]string, len(members))
	v1Created := map[string]string{}
	for i, m := range members {
		gotV1[i] = m.ConvID
		v1Created[m.ConvID] = m.CreatedAt
	}
	assert.Equal(t, wantConvs, gotV1, "/v1 members should default to newest-first by Age")
	assert.Equal(t, db.CanonicalAgeTimestamp("2026-06-18T12:00:00.004000001Z"), v1Created[convNew],
		"/v1 must surface each member's actor birth timestamp")
	assert.Equal(t, db.CanonicalAgeTimestamp("2026-06-18T14:00:00.002+02:00"), v1Created[convOwner],
		"/v1 owner-only rows must carry created_at too")

	// Surface 2: the dashboard snapshot's per-group members (a separate
	// builder in dashboard.go) — order AND the created_at field it now
	// carries for the Age column.
	dash := agentd.BuildDashboardHandlerForTest()
	snap := fetchDashSnapshot(t, dash)
	var gotDash []string
	createdByConv := map[string]string{}
	found := false
	for _, grp := range snap.Groups {
		if grp.Name != "alpha" {
			continue
		}
		found = true
		for _, m := range grp.Members {
			gotDash = append(gotDash, m.ConvID)
			createdByConv[m.ConvID] = m.CreatedAt
		}
	}
	require.True(t, found, "group alpha missing from dashboard snapshot")
	assert.Equal(t, wantConvs, gotDash, "dashboard snapshot members should default to newest-first by Age")
	assert.Equal(t, db.CanonicalAgeTimestamp("2026-06-18T12:00:00.004000001Z"), createdByConv[convNew],
		"dashboard must surface each member's actor birth timestamp for the Age column")
	assert.Equal(t, db.CanonicalAgeTimestamp("2026-06-18T14:00:00.002+02:00"), createdByConv[convOwner],
		"owner-only rows must carry created_at too")
}

// A supported-upgrade/lazy-enrollment actor can be stamped long after its
// conversation began. Age must retain the older conversation creation time
// rather than making the historical member look newly born at migration time.
func TestGroupMembers_AgeRepairsLateActorEnrollment(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const legacyConv = "eeee5555-5555-5555-5555-555555555555"
	const conversationBirth = "2020-01-02T03:04:05.123456789Z"
	mustHaveConvCreated(t, legacyConv, "legacy-worker", conversationBirth)
	f.HaveGroup("legacy")
	f.HaveMember("legacy", legacyConv) // stamps agents.created_at now

	actor, err := db.GetAgentByConv(legacyConv)
	require.NoError(t, err)
	require.NotNil(t, actor)
	require.True(t, actor.CreatedAt.After(time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)),
		"precondition: actor enrollment is later than conversation birth")

	want := db.CanonicalAgeTimestamp(conversationBirth)
	members := f.ListGroupMembers("legacy")
	require.Len(t, members, 1)
	assert.Equal(t, want, members[0].CreatedAt, "CLI/API Age uses earliest known existence")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	for _, group := range snap.Groups {
		if group.Name == "legacy" {
			require.Len(t, group.Members, 1)
			assert.Equal(t, want, group.Members[0].CreatedAt,
				"dashboard Age uses earliest known existence")
			return
		}
	}
	t.Fatal("legacy group missing from dashboard snapshot")
}

// mustSetAgentCreated backdates a conv's actor birth timestamp
// (agents.created_at) to createdRFC3339 — the Age sort key. HaveMember /
// AddAgentGroupOwner enroll the actor with the current time, so a test that
// needs a deterministic age order overwrites it here. Written directly since
// there is no production path that rewrites an actor's birth time.
func mustSetAgentCreated(t *testing.T, convID, createdRFC3339 string) {
	t.Helper()
	conn, err := db.Open()
	require.NoError(t, err)
	res, err := conn.Exec(`UPDATE agents SET created_at = ?
		WHERE agent_id = (SELECT agent_id FROM agent_conversations WHERE conv_id = ?)`,
		createdRFC3339, convID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected exactly one actor row for conv %s", convID)
}

// mustHaveConvCreated drops a conv_index row carrying both a custom
// title and an explicit creation timestamp, so the listing's age sort
// has a deterministic key (HaveConvWithTitle alone leaves Created
// empty).
func mustHaveConvCreated(t *testing.T, convID, title, createdRFC3339 string) {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		CustomTitle: title,
		Created:     createdRFC3339,
		IndexedAt:   time.Now(),
	}))
}
