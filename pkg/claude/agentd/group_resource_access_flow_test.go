package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A group's members read and draw on its boards with no global boards.* slug;
// membership is the source, live, below an explicit deny.
func TestGroupBoardsAreMembersToReadAndWrite(t *testing.T) {
	f := newFlow(t)
	const (
		member   = "grp-res-member-1111"
		outsider = "grp-res-outsider-2222"
	)
	f.HaveConvWithTitle(member, "member")
	f.HaveConvWithTitle(outsider, "outsider")
	g := f.HaveGroup("design")
	f.HaveMember("design", member)
	for _, conv := range []string{member, outsider} {
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
	}
	dash := agentd.BuildDashboardHandlerForTest()
	operator := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	agent := func(conv, method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	draw := func(conv, board, el string, v int64) *httptest.ResponseRecorder {
		return agent(conv, http.MethodPatch, "/v1/boards/"+board, map[string]any{"elements": []any{rect(el, v, 1, 0)}})
	}
	listed := func(conv string) []string {
		t.Helper()
		rec := agent(conv, http.MethodGet, "/v1/boards", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var out struct{ Boards []struct{ ID string } }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		ids := []string{}
		for _, b := range out.Boards {
			ids = append(ids, b.ID)
		}
		return ids
	}

	// A member makes a board in its group, draws on it and reads it back.
	rec := agent(member, http.MethodPost, "/v1/boards", map[string]any{"id": "ux", "title": "UX", "group": "design"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, http.StatusCreated, operator(http.MethodPost, "/api/boards", map[string]any{"id": "loose", "title": "Loose"}).Code)
	rec = draw(member, "ux", "a", 1)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Len(t, decodeBoard(t, agent(member, http.MethodGet, "/v1/boards/ux", nil)).Elements, 1)
	assert.Equal(t, []string{"ux"}, listed(member), "the list shows the boards the member reads, not the unlinked one")
	require.Equal(t, http.StatusOK, agent(member, http.MethodPost, "/v1/boards/ux/session", nil).Code)

	// A board linked to no group keeps the global rules.
	rec = agent(member, http.MethodGet, "/v1/boards/loose", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermBoardsRead)
	assert.Equal(t, http.StatusForbidden, draw(member, "loose", "x", 1).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodPost, "/v1/boards", map[string]any{"id": "y", "title": "y"}).Code)

	// A non-member is refused, and the refusal names the group slug.
	rec = agent(outsider, http.MethodGet, "/v1/boards/ux", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsResourcesRead)
	rec = draw(outsider, "ux", "b", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsResourcesWrite)
	assert.Equal(t, http.StatusForbidden, agent(outsider, http.MethodGet, "/v1/boards", nil).Code)

	// The global slug still covers every board, for a non-member too.
	require.NoError(t, db.GrantAgentPermission(outsider, agentd.PermBoardsRead, "test"))
	assert.ElementsMatch(t, []string{"loose", "ux"}, listed(outsider))
	assert.Equal(t, http.StatusOK, agent(outsider, http.MethodGet, "/v1/boards/ux", nil).Code)
	assert.Equal(t, http.StatusForbidden, draw(outsider, "ux", "b", 1).Code, "boards.read does not write")

	// An operator lock refuses the member's change and keeps its read.
	require.Equal(t, http.StatusOK, operator(http.MethodPost, "/api/resource-locks", map[string]any{"kind": "board", "id": "ux", "locked": true}).Code)
	rec = draw(member, "ux", "a", 2)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "resource_locked")
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/boards/ux", nil).Code)
	// The operator is not refused by the lock, nor by any slug.
	assert.Equal(t, http.StatusOK, operator(http.MethodPatch, "/api/boards/ux", map[string]any{"elements": []any{rect("op", 1, 1, 0)}}).Code)
	require.Equal(t, http.StatusOK, operator(http.MethodPost, "/api/resource-locks", map[string]any{"kind": "board", "id": "ux", "locked": false}).Code)
	require.Equal(t, http.StatusOK, draw(member, "ux", "a", 3).Code)

	// An explicit deny of the write slug wins over membership; read stays.
	require.NoError(t, db.SetAgentPermissionOverride(member, agentd.PermGroupsResourcesWrite, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusForbidden, draw(member, "ux", "a", 4).Code)
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/boards/ux", nil).Code)
	_, err := db.RevokeAgentPermission(member, agentd.PermGroupsResourcesWrite)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, draw(member, "ux", "a", 5).Code)

	// A deny of read stops reading although write is still the member's.
	require.NoError(t, db.SetAgentPermissionOverride(member, agentd.PermGroupsResourcesRead, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/boards/ux", nil).Code)
	_, err = db.RevokeAgentPermission(member, agentd.PermGroupsResourcesRead)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/boards/ux", nil).Code)

	// Leaving the group revokes the access at once.
	require.NoError(t, db.RemoveAgentGroupMember(g.ID, member))
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/boards/ux", nil).Code)
	assert.Equal(t, http.StatusForbidden, draw(member, "ux", "a", 6).Code)

}
