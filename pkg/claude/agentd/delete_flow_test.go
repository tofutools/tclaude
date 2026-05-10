//go:build rewire

package agentd_test

import (
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the human permanently deletes an agent.
//
// Setup: a worker named "doomed" is a member of group "alpha"
// and holds a granted permission (self.compact). Its tmux pane
// is live. The group has one other meaningful piece of state
// besides the worker (it exists; we want it to keep existing).
//
// Action: the human runs delete with force, killing the live
// pane inline before the row purge.
//
// Expected:
//   - The response says action="deleted" and includes the
//     per-table purge counts.
//   - Every row referencing the worker is gone: group
//     membership, permission grants, session rows.
//   - The group itself still exists. Delete only purges the
//     conv, not its surroundings.
//   - A second delete on the same conv returns 404 — the
//     selector no longer resolves. (We want this observable
//     rather than silent so a typo'd conv-id surfaces clearly.)
func TestDelete_PurgesAllReferencingRows(t *testing.T) {
	f := newFlow(t)

	const target = "del-aaaa-bbbb-cccc-dddd"
	const label = "spwn-del-001"
	const tmuxSess = "tclaude-spwn-del-001"

	f.HaveConvWithTitle(target, "doomed")
	f.HaveAliveSession(target, label, tmuxSess, "/tmp/work")
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", target, "doomed")
	if err := db.GrantAgentPermission(target, "self.compact", "test"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	resp := f.AsHuman().Delete(target, true /* force */)

	if resp.Action != "deleted" {
		t.Errorf("action = %q, want %q", resp.Action, "deleted")
	}
	if resp.DBCounts == nil {
		t.Errorf("expected db_counts in response; got nil (raw=%s)", resp.Raw)
	}

	f.AssertDeleted(target)

	stillThere, err := db.GetAgentGroupByName("alpha")
	if err != nil || stillThere == nil || stillThere.ID != g.ID {
		t.Errorf("group alpha should still exist; got %v err=%v", stillThere, err)
	}

	// Re-delete — the DSL's Delete fatals on non-200, so drop to
	// the lower-level helpers to observe the 404 explicitly.
	r2 := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/"+target+"/delete", nil))
	rec := testharness.Serve(f.Mux, r2)
	if rec.Code != http.StatusNotFound {
		t.Errorf("re-delete: status=%d body=%s, want 404",
			rec.Code, rec.Body.String())
	}
}
