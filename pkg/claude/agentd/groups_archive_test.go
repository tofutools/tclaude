package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// serveV1 routes r through the production /v1 mux — the same dispatch a
// real daemon request takes after the move to Go 1.22 method+pattern
// routing for the /v1/groups/{name} endpoints.
func serveV1(w http.ResponseWriter, r *http.Request) {
	BuildHandlerForTest().ServeHTTP(w, r)
}

// Archiving an active group flips the row's archived_at column,
// returns 200 + an "archived" action, and makes subsequent
// member-add attempts return 409.
func TestHandleGroupArchive_FlipsAndBlocks(t *testing.T) {
	setupTestDB(t)
	gID, err := db.CreateAgentGroup("team", "")
	require.NoError(t, err, "CreateAgentGroup")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-1",
	})

	// Archive (human path — operator-token caller passes the gate).
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/groups/team/archive", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	serveV1(w, r) // dispatcher entry point with selector
	require.Equal(t, http.StatusOK, w.Code, "archive body=%s", w.Body.String())

	// Re-fetch the group via the daemon helper (NOT the cached pointer)
	// to confirm archived_at landed.
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err, "re-fetch group")
	require.NotNil(t, g, "re-fetch group nil")
	require.True(t, g.IsArchived(), "expected archived flag, got %+v", g)

	// Subsequent add-member attempts must 409. Use the dispatcher so
	// the request goes through the same path as a real CLI call.
	memberBody, _ := json.Marshal(map[string]string{"conv": "worker-1"})
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/groups/team/members",
		bytes.NewReader(memberBody))
	r2 = r2.WithContext(context.WithValue(r2.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	serveV1(w2, r2)
	assert.Equal(t, http.StatusConflict, w2.Code,
		"add-member on archived group: body=%s", w2.Body.String())
}

// Unarchiving clears archived_at and re-allows mutations. Mirrors
// the archive test but verifies the reverse direction.
func TestHandleGroupUnarchive_ClearsAndAllows(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-1",
	})
	require.NoError(t, db.ArchiveAgentGroup("team"), "ArchiveAgentGroup")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/groups/team/unarchive", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	serveV1(w, r)
	require.Equal(t, http.StatusOK, w.Code, "unarchive body=%s", w.Body.String())

	g, _ := db.GetAgentGroupByName("team")
	assert.False(t, g.IsArchived(), "expected active after unarchive, got archived")
}

// Listing endpoint defaults to filtering archived groups out;
// ?archived=1 includes them. Locks in the wire-shape contract the
// CLI's --archived flag depends on.
func TestHandleGroupsList_HidesArchivedByDefault(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateAgentGroup("active-team", "")
	require.NoError(t, err, "active")
	_, err = db.CreateAgentGroup("retired-team", "")
	require.NoError(t, err, "retired")
	require.NoError(t, db.ArchiveAgentGroup("retired-team"), "archive")

	// Default GET → archived hidden.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/groups", nil)
	handleGroups(w, r)
	require.Equal(t, http.StatusOK, w.Code, "list status")
	var out []groupSummary
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "decode")
	names := map[string]bool{}
	for _, g := range out {
		names[g.Name] = true
	}
	assert.True(t, names["active-team"], "active group missing from default list")
	assert.False(t, names["retired-team"], "archived group present in default list — should be hidden")

	// ?archived=1 → both shown, archived flag set on the retired row.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/groups?archived=1", nil)
	handleGroups(w2, r2)
	var withArchived []groupSummary
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &withArchived), "decode")
	var foundArchived bool
	for _, g := range withArchived {
		if g.Name == "retired-team" && g.Archived {
			foundArchived = true
		}
	}
	assert.True(t, foundArchived,
		"?archived=1 should surface the archived group with archived=true; got %+v", withArchived)
}

// requireGroupActive returns false (and writes 409) for archived
// groups, true for active ones. Direct unit test of the helper used
// by every mutation handler.
func TestRequireGroupActive(t *testing.T) {
	active := &db.AgentGroup{Name: "active"}
	w1 := httptest.NewRecorder()
	assert.True(t, requireGroupActive(w1, active), "active group rejected; body=%s", w1.Body.String())

	archived := &db.AgentGroup{Name: "archived"}
	// Set archived_at via direct field mutation (the helper uses
	// IsZero/IsArchived which checks the time field).
	archived.ArchivedAt = time.Now()
	w2 := httptest.NewRecorder()
	assert.False(t, requireGroupActive(w2, archived), "archived group should be rejected")
	assert.Equal(t, http.StatusConflict, w2.Code, "status")
}
