package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Archiving an active group flips the row's archived_at column,
// returns 200 + an "archived" action, and makes subsequent
// member-add attempts return 409.
func TestHandleGroupArchive_FlipsAndBlocks(t *testing.T) {
	setupTestDB(t)
	gID, err := db.CreateAgentGroup("team", "")
	if err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-1", Alias: "w",
	})

	// Archive (human path — empty caller bypasses permission gate).
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/groups/team/archive", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1}))
	handleGroupByName(w, r) // dispatcher entry point with selector
	if w.Code != http.StatusOK {
		t.Fatalf("archive status %d, body=%s", w.Code, w.Body.String())
	}

	// Re-fetch the group via the daemon helper (NOT the cached pointer)
	// to confirm archived_at landed.
	g, err := db.GetAgentGroupByName("team")
	if err != nil || g == nil {
		t.Fatalf("re-fetch group: %v / nil=%v", err, g == nil)
	}
	if !g.IsArchived() {
		t.Fatalf("expected archived flag, got %+v", g)
	}

	// Subsequent add-member attempts must 409. Use the dispatcher so
	// the request goes through the same path as a real CLI call.
	memberBody, _ := json.Marshal(map[string]string{"conv": "worker-1"})
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/groups/team/members",
		bytes.NewReader(memberBody))
	r2 = r2.WithContext(context.WithValue(r2.Context(), peerKey{}, &peer{PID: 1}))
	handleGroupByName(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Errorf("add-member on archived group: status %d, want 409; body=%s",
			w2.Code, w2.Body.String())
	}
}

// Unarchiving clears archived_at and re-allows mutations. Mirrors
// the archive test but verifies the reverse direction.
func TestHandleGroupUnarchive_ClearsAndAllows(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-1", Alias: "w",
	})
	if err := db.ArchiveAgentGroup("team"); err != nil {
		t.Fatalf("ArchiveAgentGroup: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/groups/team/unarchive", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1}))
	handleGroupByName(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unarchive status %d, body=%s", w.Code, w.Body.String())
	}

	g, _ := db.GetAgentGroupByName("team")
	if g.IsArchived() {
		t.Errorf("expected active after unarchive, got archived")
	}
}

// Listing endpoint defaults to filtering archived groups out;
// ?archived=1 includes them. Locks in the wire-shape contract the
// CLI's --archived flag depends on.
func TestHandleGroupsList_HidesArchivedByDefault(t *testing.T) {
	setupTestDB(t)
	if _, err := db.CreateAgentGroup("active-team", ""); err != nil {
		t.Fatalf("active: %v", err)
	}
	if _, err := db.CreateAgentGroup("retired-team", ""); err != nil {
		t.Fatalf("retired: %v", err)
	}
	if err := db.ArchiveAgentGroup("retired-team"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Default GET → archived hidden.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/groups", nil)
	handleGroups(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d", w.Code)
	}
	var out []groupSummary
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, g := range out {
		names[g.Name] = true
	}
	if !names["active-team"] {
		t.Errorf("active group missing from default list")
	}
	if names["retired-team"] {
		t.Errorf("archived group present in default list — should be hidden")
	}

	// ?archived=1 → both shown, archived flag set on the retired row.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/groups?archived=1", nil)
	handleGroups(w2, r2)
	var withArchived []groupSummary
	if err := json.Unmarshal(w2.Body.Bytes(), &withArchived); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var foundArchived bool
	for _, g := range withArchived {
		if g.Name == "retired-team" && g.Archived {
			foundArchived = true
		}
	}
	if !foundArchived {
		t.Errorf("?archived=1 should surface the archived group with archived=true; got %+v", withArchived)
	}
}

// requireGroupActive returns false (and writes 409) for archived
// groups, true for active ones. Direct unit test of the helper used
// by every mutation handler.
func TestRequireGroupActive(t *testing.T) {
	active := &db.AgentGroup{Name: "active"}
	w1 := httptest.NewRecorder()
	if !requireGroupActive(w1, active) {
		t.Errorf("active group rejected; body=%s", w1.Body.String())
	}

	archived := &db.AgentGroup{Name: "archived"}
	// Set archived_at via direct field mutation (the helper uses
	// IsZero/IsArchived which checks the time field).
	archived.ArchivedAt = time.Now()
	w2 := httptest.NewRecorder()
	if requireGroupActive(w2, archived) {
		t.Error("archived group should be rejected")
	}
	if w2.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w2.Code)
	}
}
