package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a group whose name contains a slash — the poison data the
// old slash-unaware group create accepted — must still be renameable
// from the dashboard. The browser sends the name percent-encoded
// (encodeURIComponent → %2F); the route has to decode that back into a
// single {name} wildcard rather than re-splitting it into path
// segments.
//
// Before the fix, the dashboard dispatcher hand-rolled the parse by
// splitting the already-decoded r.URL.Path, so "squad%2Falpha" arrived
// as two segments ("squad", "alpha") and the rename route was lost
// (404 / "expected /api/groups/{name}..."). This pins the Go 1.22
// {name}-wildcard routing that fixes it, asserting at the snapshot —
// the read surface the dashboard actually renders from.
func TestGroupSlashRename_DashboardRecoversPoisonGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		// HaveGroup writes straight through db.CreateAgentGroup, which has
		// no name validation — exactly how a slash name slipped in before
		// the create-side guard landed.
		f.HaveGroup("squad/alpha")

		mux := agentd.BuildDashboardHandlerForTest()

		// Pre-condition: the poison group is in the snapshot under its
		// slashed name.
		pre := fetchDashSnapshot(t, mux)
		require.True(t, snapshotHasGroup(pre, "squad/alpha"),
			"pre-rename: slashed group should be in the snapshot; got %v", groupNames(pre))

		// Rename it via the dashboard endpoint, with the name percent-
		// encoded the way the browser's encodeURIComponent sends it.
		body := strings.NewReader(`{"new_name":"squad-alpha"}`)
		r, err := http.NewRequest(http.MethodPost, "/api/groups/squad%2Falpha/rename", body)
		require.NoError(t, err, "build request")
		r.Header.Set("Content-Type", "application/json")
		rec := testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code,
			"POST /api/groups/squad%%2Falpha/rename body=%s", rec.Body.String())

		// Post-condition: the snapshot shows the clean name and no longer
		// the slashed one.
		post := fetchDashSnapshot(t, mux)
		assert.True(t, snapshotHasGroup(post, "squad-alpha"),
			"post-rename: renamed group missing; got %v", groupNames(post))
		assert.False(t, snapshotHasGroup(post, "squad/alpha"),
			"post-rename: slashed name should be gone; got %v", groupNames(post))
	})
}

// Scenario: the /v1 (SO_PEERCRED socket) twin of the above — the CLI's
// `tclaude agent groups rename` path. handleGroupByName used the same
// hand-rolled split; the modernized /v1/groups/{name} wildcard routes
// must carry a slashed group name through to the rename handler.
func TestGroupSlashRename_V1RecoversPoisonGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("ops/team")

		mux := agentd.BuildHandlerForTest()
		body := strings.NewReader(`{"new_name":"ops-team"}`)
		r, err := http.NewRequest(http.MethodPost, "/v1/groups/ops%2Fteam/rename", body)
		require.NoError(t, err, "build request")
		r.Header.Set("Content-Type", "application/json")
		r = agentd.AsHumanPeer(r) // human peer bypasses the groups.rename slug
		rec := testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code,
			"POST /v1/groups/ops%%2Fteam/rename body=%s", rec.Body.String())

		old, err := db.GetAgentGroupByName("ops/team")
		require.NoError(t, err, "lookup old name")
		assert.Nil(t, old, "slashed name should no longer resolve after rename")
		renamed, err := db.GetAgentGroupByName("ops-team")
		require.NoError(t, err, "lookup new name")
		assert.NotNil(t, renamed, "renamed group should resolve under the clean name")
	})
}

// Scenario: creating a group with a slash in the name is rejected up
// front. validateGroupName has always rejected slashes for rename and
// clone; create historically skipped it, which is the root cause that
// let a poison group exist at all. This pins the create-side guard so
// no new slash-named group can be made (the dashboard create endpoint
// shares handleGroups with the CLI's `groups create`).
func TestGroupCreate_RejectsSlashName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		newFlow(t) // sets up the test DB

		mux := agentd.BuildDashboardHandlerForTest()
		body := strings.NewReader(`{"name":"bad/name"}`)
		r, err := http.NewRequest(http.MethodPost, "/api/groups", body)
		require.NoError(t, err, "build request")
		r.Header.Set("Content-Type", "application/json")
		rec := testharness.Serve(mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"POST /api/groups with a slashed name should be rejected; body=%s", rec.Body.String())

		g, err := db.GetAgentGroupByName("bad/name")
		require.NoError(t, err, "group lookup")
		assert.Nil(t, g, "no slashed group should have been created")
	})
}

// snapshotHasGroup reports whether the dashboard snapshot lists a group
// with exactly this name.
func snapshotHasGroup(snap dashSnapshot, name string) bool {
	for _, g := range snap.Groups {
		if g.Name == name {
			return true
		}
	}
	return false
}

// groupNames extracts the group names from a snapshot — used for
// readable assertion failure messages.
func groupNames(snap dashSnapshot) []string {
	out := make([]string, 0, len(snap.Groups))
	for _, g := range snap.Groups {
		out = append(out, g.Name)
	}
	return out
}
