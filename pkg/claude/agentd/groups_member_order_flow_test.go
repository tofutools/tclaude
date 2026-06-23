package agentd_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a group's member listing defaults to newest-first by
// conversation creation time (the dashboard's Age column). The default
// is shared by the CLI (`tclaude agent groups members`, via
// GET /v1/groups/{name}/members) and the browser dashboard, whose
// client-side column sort falls back to this server order when no column
// is active.
//
// Two further properties this pins:
//   - the backend returns each member's creation timestamp
//     (created_at), and owners who are not members carry it too and
//     interleave by age — they no longer land at the tail in random map
//     iteration order;
//   - the conv-ids and join order are chosen so neither the old
//     joined_at order nor conv-id order matches the wanted age order, so
//     the test can only pass if the creation-time sort is applied.
func TestGroupMembers_DefaultSortByAgeNewestFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		// Creation times: convNew is the most recent, convOwner sits between
		// mid and new, convOld is the oldest.
		mustHaveConvCreated(t, convOld, "old-worker", "2026-06-18T10:00:00Z")
		mustHaveConvCreated(t, convMid, "mid-worker", "2026-06-18T12:00:00Z")
		mustHaveConvCreated(t, convNew, "new-worker", "2026-06-18T15:00:00Z")
		mustHaveConvCreated(t, convOwner, "owner-conv", "2026-06-18T13:00:00Z")

		g := f.HaveGroup("alpha")
		// Insert in oldest→newest order so join order is the reverse of the
		// wanted newest-first order.
		f.HaveMember("alpha", convOld)
		f.HaveMember("alpha", convMid)
		f.HaveMember("alpha", convNew)
		// An owner who is not a member — must interleave by age (between mid
		// and new), not land at the tail.
		require.NoError(t, db.AddAgentGroupOwner(g.ID, convOwner, "test"))

		// Newest first; owner (13:00) sits between new (15:00) and mid (12:00).
		wantConvs := []string{convNew, convOwner, convMid, convOld}

		// Surface 1: GET /v1/groups/{name}/members — the CLI listing.
		members := f.ListGroupMembers("alpha")
		gotV1 := make([]string, len(members))
		v1Created := map[string]string{}
		for i, m := range members {
			gotV1[i] = m.ConvID
			v1Created[m.ConvID] = m.CreatedAt
		}
		assert.Equal(t, wantConvs, gotV1, "/v1 members should default to newest-first by creation time")
		assert.Equal(t, "2026-06-18T15:00:00Z", v1Created[convNew],
			"/v1 must surface each member's creation timestamp")
		assert.Equal(t, "2026-06-18T13:00:00Z", v1Created[convOwner],
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
		assert.Equal(t, wantConvs, gotDash, "dashboard snapshot members should default to newest-first by creation time")
		assert.Equal(t, "2026-06-18T15:00:00Z", createdByConv[convNew],
			"dashboard must surface each member's creation timestamp for the Age column")
		assert.Equal(t, "2026-06-18T13:00:00Z", createdByConv[convOwner],
			"owner-only rows must carry created_at too")
	})
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
