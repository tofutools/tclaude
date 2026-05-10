//go:build rewire

package agentd_test

import (
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDelete_PurgesAllReferencingRows pins the destructive-but-
// idempotent contract of DELETE /v1/agent/{conv}: every row in
// every agent / conv / session table that references the conv-id
// gone, and a re-delete behaves predictably.
//
// Scenario:
//  - "worker" is a member of group "alpha" with one granted
//    permission. Its tmux pane is live.
//  - DELETE ?force=1 → kills tmux + purges DB rows.
//  - All sweeps (ListGroupsForConv / ListAgentPermissionsForConv /
//    FindSessionsByConvID) come back empty.
//  - The group itself survives (destructive scope is bounded to
//    the conv).
//  - A second DELETE on the same conv returns 404 — already-
//    purged is observable, not a silent re-success that would
//    mask wrong-id typos.
func TestDelete_PurgesAllReferencingRows(t *testing.T) {
	f := newFlow(t)

	const target = "del-aaaa-bbbb-cccc-dddd"
	const label = "spwn-del-001"
	const tmuxSess = "tclaude-spwn-del-001"

	// Given: a worker with conv-index, group membership, granted
	// permission, and a live tmux pane.
	f.HaveConvWithTitle(target, "doomed")
	f.HaveAliveSession(target, label, tmuxSess, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", target, "doomed")
	if err := db.GrantAgentPermission(target, "self.compact", "test"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// When: the human force-deletes the worker.
	resp := f.AsHuman().Delete(target, true /* force */)

	// Then: the response declares success and reports purge counts.
	if resp.Action != "deleted" {
		t.Errorf("action = %q, want %q", resp.Action, "deleted")
	}
	if resp.DBCounts == nil {
		t.Errorf("expected db_counts in response; got nil (raw=%s)", resp.Raw)
	}

	// Then: every table that referenced `target` is empty.
	f.AssertDeleted(target)

	// And the group itself still exists — destructive scope is
	// bounded to the conv.
	stillThere, err := db.GetAgentGroupByName("alpha")
	if err != nil || stillThere == nil || stillThere.ID != g.ID {
		t.Errorf("group alpha should still exist after deleting one member; got %v err=%v", stillThere, err)
	}

	// When: the human re-deletes. The conv is gone, so the agent
	// resolver returns 404. Documents that the second DELETE is
	// observable, not silent.
	r2 := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/"+target+"/delete", nil))
	rec := testharness.Serve(f.Mux, r2)
	if rec.Code != http.StatusNotFound {
		t.Errorf("re-delete: status=%d body=%s, want 404 (already-purged is observable, not silent)",
			rec.Code, rec.Body.String())
	}
}
