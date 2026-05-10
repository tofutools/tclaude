package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	if err := db.AddAgentGroupOwner(g.ID, memberA, "test"); err != nil {
		t.Fatalf("AddAgentGroupOwner: %v", err)
	}

	rec := postRename(t, f, "alpha", "alpha-renamed")
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Old name no longer resolves.
	if got, _ := db.GetAgentGroupByName("alpha"); got != nil {
		t.Errorf("old name should 404 after rename; got %+v", got)
	}
	// New name resolves to the same id (foreign keys still match).
	got, err := db.GetAgentGroupByName("alpha-renamed")
	if err != nil || got == nil {
		t.Fatalf("new name should resolve; got=%v err=%v", got, err)
	}
	if got.ID != g.ID {
		t.Errorf("rename should keep id stable; was %d, now %d", g.ID, got.ID)
	}

	// Members still attached via the stable id.
	members, _ := db.ListAgentGroupMembers(got.ID)
	if len(members) != 2 {
		t.Errorf("members should survive rename; got %d, want 2", len(members))
	}
	// Owners likewise.
	owners, _ := db.ListAgentGroupOwners(got.ID)
	if len(owners) != 1 || owners[0].ConvID != memberA {
		t.Errorf("owner should survive rename; got %+v", owners)
	}

	// Audit row recorded.
	hist, err := db.ListAgentGroupRenames(got.ID)
	if err != nil {
		t.Fatalf("ListAgentGroupRenames: %v", err)
	}
	if len(hist) != 1 || hist[0].OldName != "alpha" || hist[0].NewName != "alpha-renamed" {
		t.Errorf("audit row missing or wrong; got %+v", hist)
	}
}

// Scenario: rename target collides with another existing group → 409.
// No mutations should land.
func TestGroupsRename_NameCollisionIsConflict(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	other := f.HaveGroup("beta")

	rec := postRename(t, f, "alpha", "beta")
	if rec.Code != http.StatusConflict {
		t.Fatalf("collision: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	// alpha untouched.
	a, _ := db.GetAgentGroupByName("alpha")
	if a == nil {
		t.Error("alpha should still exist after collision")
	}
	// beta still has its original id.
	b, _ := db.GetAgentGroupByName("beta")
	if b == nil || b.ID != other.ID {
		t.Errorf("beta should be untouched; got %+v want id=%d", b, other.ID)
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
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad name %q: status=%d body=%s, want 400",
				bad, rec.Code, rec.Body.String())
			// If the rename slipped through, surface that distinctly
			// from "alpha was already gone for a different reason".
		}
		if a, _ := db.GetAgentGroupByName("alpha"); a == nil {
			t.Fatalf("alpha disappeared after rejecting %q — validator let a mutation through", bad)
		}
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
	if rec.Code != http.StatusOK {
		t.Fatalf("same-name rename: status=%d body=%s, want 200",
			rec.Code, rec.Body.String())
	}
	if got, _ := db.GetAgentGroupByName("alpha"); got == nil || got.ID != g.ID {
		t.Errorf("group should still exist with same id; got %+v", got)
	}
	// Audit row still recorded.
	hist, _ := db.ListAgentGroupRenames(g.ID)
	if len(hist) != 1 {
		t.Errorf("same-name rename should still log audit; got %d rows", len(hist))
	}
}

// Scenario: rename a 404'd group → 404 from the dispatcher (the
// dispatcher resolves the source before reaching the rename branch).
func TestGroupsRename_MissingSourceIs404(t *testing.T) {
	f := newFlow(t)

	rec := postRename(t, f, "no-such-group", "whatever")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing source: status=%d body=%s, want 404",
			rec.Code, rec.Body.String())
	}
}

// Scenario: rename an archived group. archived_at must be preserved
// across the rename (it's a separate column on agent_groups, not tied
// to name).
func TestGroupsRename_PreservesArchivedState(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	if err := db.ArchiveAgentGroup("alpha"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rec := postRename(t, f, "alpha", "alpha-renamed")
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetAgentGroupByName("alpha-renamed")
	if got == nil {
		t.Fatal("renamed group missing")
	}
	if got.ID != g.ID {
		t.Errorf("id should be stable; was %d, now %d", g.ID, got.ID)
	}
	if !got.IsArchived() {
		t.Error("archived state should survive rename")
	}
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
