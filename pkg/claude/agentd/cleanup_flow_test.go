package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// cleanupResp mirrors the unexported agentd.cleanupResponse so flow
// tests can decode the /api/cleanup/* result without importing the
// internal type.
type cleanupResp struct {
	Mode     string `json:"mode"`
	Outcomes []struct {
		ConvID string   `json:"conv_id"`
		Title  string   `json:"title"`
		Result string   `json:"result"`
		Detail string   `json:"detail"`
		Groups []string `json:"groups"`
	} `json:"outcomes"`
	Removed  int      `json:"removed"`
	Deleted  int      `json:"deleted"`
	Skipped  int      `json:"skipped"`
	Failed   int      `json:"failed"`
	Warnings []string `json:"warnings"`
}

// postCleanup fires a cleanup request at the dashboard mux and decodes
// the 200 response. Fatals on any non-200 — error-surface scenarios
// use a raw testharness.Serve instead.
func postCleanup(t *testing.T, mux http.Handler, path, body string) cleanupResp {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
	require.NoError(t, err, "build request")
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "POST %s body=%s", path, rec.Body.String())
	var resp cleanupResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode cleanup response")
	return resp
}

// flowGroupHasMember walks the v1 members surface — the same list
// `tclaude agent groups members` renders — and reports whether convID
// is still in it.
func flowGroupHasMember(f *testharness.Flow, group, convID string) bool {
	for _, m := range f.ListGroupMembers(group) {
		if m.ConvID == convID {
			return true
		}
	}
	return false
}

// Scenario: the per-group 🧹 cleanup button. The browser POSTs the
// human-edited member list — and a careless human could leave an
// online member ticked. The daemon's own tmux re-check, not the
// client's selection, is what protects the live agent: it stays in
// the group, the offline one is removed.
func TestCleanup_Group_RemovesOfflineKeepsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const offlineConv = "offl-1111-2222-3333-4444"
	const onlineConv = "onln-1111-2222-3333-4444"
	f.HaveConvWithTitle(offlineConv, "stale-worker")
	f.HaveConvWithTitle(onlineConv, "live-worker")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveGroup("squad")
	f.HaveMember("squad", offlineConv, "stale")
	f.HaveMember("squad", onlineConv, "live")
	f.MarkOffline("tmux-offl")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+offlineConv+`","`+onlineConv+`"]}`)

	assert.Equal(t, 1, resp.Removed, "exactly the offline member removed")
	assert.Equal(t, 1, resp.Skipped, "the online member skipped by the tmux re-check")
	f.AssertNotGroupMember("squad", offlineConv)
	assert.True(t, flowGroupHasMember(f, "squad", onlineConv),
		"online member must survive cleanup")
}

// Scenario: an offline group OWNER is excluded by default — a cleanup
// run without the opt-in leaves it untouched. Ticking "include
// offline owners" both removes the membership AND strips the owner
// row, and the response warns that the group is now ownerless.
func TestCleanup_Group_OwnerExcludedUnlessOptedIn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const ownerConv = "ownr-1111-2222-3333-4444"
	f.HaveConvWithTitle(ownerConv, "boss")
	f.HaveAliveSession(ownerConv, "spwn-ownr", "tmux-ownr", "/tmp/ownr")
	g := f.HaveGroup("squad")
	f.HaveMember("squad", ownerConv, "boss")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"), "seed owner")
	f.MarkOffline("tmux-ownr")

	mux := agentd.BuildDashboardHandlerForTest()

	// Default pass: owner stays put.
	def := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+ownerConv+`"]}`)
	assert.Equal(t, 0, def.Removed, "owner not removed by default")
	assert.Equal(t, 1, def.Skipped, "owner reported skipped")
	assert.True(t, flowGroupHasMember(f, "squad", ownerConv), "owner still a member")

	// Opt-in pass: owner removed, owner row gone, ownerless warning.
	inc := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+ownerConv+`"],"include_owners":true}`)
	assert.Equal(t, 1, inc.Removed, "owner removed with include_owners")
	f.AssertNotGroupMember("squad", ownerConv)
	isOwner, err := db.IsAgentGroupOwner(g.ID, ownerConv)
	require.NoError(t, err)
	assert.False(t, isOwner, "owner row must be stripped too")
	assert.NotEmpty(t, inc.Warnings, "expected an ownerless-group warning")
}

// Scenario: the Agents-tab 🧹 cleanup button — delete=true. Offline
// agents are purged (conv + every group/owner/perm row); an online
// agent in the same request is skipped by the tmux re-check.
func TestCleanup_Agents_DeleteOfflineSkipsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const offlineConv = "offl-1111-2222-3333-4444"
	const onlineConv = "onln-1111-2222-3333-4444"
	f.HaveConvWithTitle(offlineConv, "stale-worker")
	f.HaveConvWithTitle(onlineConv, "live-worker")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveGroup("squad")
	f.HaveMember("squad", offlineConv, "stale")
	f.HaveMember("squad", onlineConv, "live")
	f.MarkOffline("tmux-offl")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+offlineConv+`","`+onlineConv+`"],"delete":true}`)

	assert.Equal(t, 1, resp.Deleted, "offline agent purged")
	assert.Equal(t, 1, resp.Skipped, "online agent skipped")
	f.AssertDeleted(offlineConv)
	f.AssertNotGroupMember("squad", offlineConv)
	assert.True(t, flowGroupHasMember(f, "squad", onlineConv),
		"online agent untouched")
}

// Scenario: the Groups-tab "clean up all groups" button with delete
// left OFF — an offline agent is unjoined from every group it
// belongs to but its conversation history is left intact on disk.
func TestCleanup_Agents_RemoveFromAllGroupsKeepsConv(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "many-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "rover")
	f.HaveAliveSession(conv, "spwn-many", "tmux-many", "/tmp/many")
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", conv, "rover")
	f.HaveMember("beta", conv, "rover")
	f.MarkOffline("tmux-many")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"delete":false}`)

	assert.Equal(t, 1, resp.Removed, "agent removed from its groups")
	assert.Equal(t, 0, resp.Deleted, "delete=false must not purge")
	f.AssertNotGroupMember("alpha", conv)
	f.AssertNotGroupMember("beta", conv)
	// The conv itself survives — only memberships were dropped.
	row, err := db.GetConvIndex(conv)
	require.NoError(t, err)
	assert.NotNil(t, row, "conv_index row must survive a remove-from-groups cleanup")
}

// Scenario: a cleanup pointed at a group that doesn't exist returns
// 404 — keeps the modal's error toast readable instead of silently
// reporting "0 removed".
func TestCleanup_Group_UnknownGroupReturns404(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	mux := agentd.BuildDashboardHandlerForTest()
	r, err := http.NewRequest(http.MethodPost, "/api/cleanup/group",
		strings.NewReader(`{"group":"no-such-group","members":["x"]}`))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}
