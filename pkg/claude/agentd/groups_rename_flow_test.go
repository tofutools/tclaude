package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: human renames a group. Membership / ownership / messages
// all stay attached because the schema uses integer foreign keys.
// Pins the production read paths: ListAgentGroupMembers under the new
// name resolves the same set; the old name 404s.
func TestGroupsRename_BasicMembersSurvive(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("alpha")
	const memberA = "aaa-aaaa-bbbb-cccc-1111"
	const memberB = "bbb-aaaa-bbbb-cccc-2222"
	f.HaveMember("alpha", memberA, "alice")
	f.HaveMember("alpha", memberB, "bob")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, memberA, "test"), "AddAgentGroupOwner")

	rec := postRename(t, f, "alpha", "alpha-renamed")
	require.Equal(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())

	// Old name no longer resolves.
	got, _ := db.GetAgentGroupByName("alpha")
	assert.Nil(t, got, "old name should 404 after rename")
	// New name resolves to the same id (foreign keys still match).
	got, err := db.GetAgentGroupByName("alpha-renamed")
	require.NoError(t, err, "new name should resolve")
	require.NotNil(t, got, "new name should resolve")
	assert.Equal(t, g.ID, got.ID, "rename should keep id stable")

	// Members still attached via the stable id.
	members, _ := db.ListAgentGroupMembers(got.ID)
	assert.Len(t, members, 2, "members should survive rename")
	// Owners likewise.
	owners, _ := db.ListAgentGroupOwners(got.ID)
	if assert.Len(t, owners, 1, "owner should survive rename") {
		assert.Equal(t, memberA, owners[0].ConvID, "owner conv-id")
	}

	// Audit row recorded.
	hist, err := db.ListAgentGroupRenames(got.ID)
	require.NoError(t, err, "ListAgentGroupRenames")
	if assert.Len(t, hist, 1, "audit row") {
		assert.Equal(t, "alpha", hist[0].OldName, "OldName")
		assert.Equal(t, "alpha-renamed", hist[0].NewName, "NewName")
	}
}

// Scenario: rename target collides with another existing group → 409.
// No mutations should land.
func TestGroupsRename_NameCollisionIsConflict(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	other := f.HaveGroup("beta")

	rec := postRename(t, f, "alpha", "beta")
	require.Equal(t, http.StatusConflict, rec.Code, "collision body=%s", rec.Body.String())
	// alpha untouched.
	a, _ := db.GetAgentGroupByName("alpha")
	assert.NotNil(t, a, "alpha should still exist after collision")
	// beta still has its original id.
	b, _ := db.GetAgentGroupByName("beta")
	if assert.NotNil(t, b, "beta should be untouched") {
		assert.Equal(t, other.ID, b.ID, "beta id should be stable")
	}
}

// Scenario: rename with an invalid name (embedded slash, control char,
// or empty) → 400. URL dispatcher would otherwise route the segments
// as path components. The validator runs BEFORE any mutation, so
// alpha must survive every reject.
func TestGroupsRename_RejectsInvalidNames(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	for _, bad := range []string{"", "has/slash", "has\\backslash", "  trailing-space  ", "\x01control"} {
		rec := postRename(t, f, "alpha", bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"bad name %q: body=%s", bad, rec.Body.String())
		a, _ := db.GetAgentGroupByName("alpha")
		require.NotNil(t, a, "alpha disappeared after rejecting %q — validator let a mutation through", bad)
	}
}

// Scenario: rename the source to its current name. Should succeed
// (200) as a no-op so the human can safely re-run a script after
// fixing a typo elsewhere. The audit row is still recorded so the
// "I ran rename" event is debuggable.
func TestGroupsRename_SameNameIsNoop(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	rec := postRename(t, f, "alpha", "alpha")
	require.Equal(t, http.StatusOK, rec.Code, "same-name rename body=%s", rec.Body.String())
	got, _ := db.GetAgentGroupByName("alpha")
	if assert.NotNil(t, got, "group should still exist with same id") {
		assert.Equal(t, g.ID, got.ID, "group id should be stable")
	}
	// Audit row still recorded.
	hist, _ := db.ListAgentGroupRenames(g.ID)
	assert.Len(t, hist, 1, "same-name rename should still log audit")
}

// Scenario: rename a 404'd group → 404 from the dispatcher (the
// dispatcher resolves the source before reaching the rename branch).
func TestGroupsRename_MissingSourceIs404(t *testing.T) {
	f := newFlow(t)

	rec := postRename(t, f, "no-such-group", "whatever")
	require.Equal(t, http.StatusNotFound, rec.Code,
		"missing source body=%s", rec.Body.String())
}

// Scenario: rename an archived group. archived_at must be preserved
// across the rename (it's a separate column on agent_groups, not tied
// to name).
func TestGroupsRename_PreservesArchivedState(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	require.NoError(t, db.ArchiveAgentGroup("alpha"), "archive")

	rec := postRename(t, f, "alpha", "alpha-renamed")
	require.Equal(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())
	got, _ := db.GetAgentGroupByName("alpha-renamed")
	require.NotNil(t, got, "renamed group missing")
	assert.Equal(t, g.ID, got.ID, "id should be stable")
	assert.True(t, got.IsArchived(), "archived state should survive rename")
}

// postRename is a small helper to keep the call sites concise.
// Routes as the human peer since rename is human-only by default.
func postRename(t *testing.T, f *testharness.Flow, oldName, newName string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/"+oldName+"/rename",
		map[string]string{"new_name": newName}))
	return testharness.Serve(f.Mux, r)
}
