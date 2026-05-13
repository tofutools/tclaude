package agentd

import (
	"net/http/httptest"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRequireScopedLinkAuthority_HumanPasses: the human (no claude
// ancestor in the process tree) bypasses every permission check.
func TestRequireScopedLinkAuthority_HumanPasses(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	b, _ := db.CreateAgentGroup("b", "")
	id, _ := db.InsertAgentGroupLink(a, b, db.LinkModeMembersToMembers, "")
	link, _ := db.GetAgentGroupLinkByID(id)
	groupA, _ := db.GetAgentGroupByID(a)

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: false})
	if _, ok := requireScopedLinkAuthority(w, r, groupA, link, PermGroupsLinkRm); !ok {
		t.Fatalf("human should pass; body=%s", w.Body.String())
	}
}

// TestRequireScopedLinkAuthority_OwnerOfFromBypasses: an owner of the
// link's FROM side passes without holding the slug.
func TestRequireScopedLinkAuthority_OwnerOfFromBypasses(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	b, _ := db.CreateAgentGroup("b", "")
	id, _ := db.InsertAgentGroupLink(a, b, db.LinkModeMembersToMembers, "")
	link, _ := db.GetAgentGroupLinkByID(id)
	groupA, _ := db.GetAgentGroupByID(a)
	if err := db.AddAgentGroupOwner(a, "manager", "<test>"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireScopedLinkAuthority(w, r, groupA, link, PermGroupsLinkRm)
	if !ok {
		t.Fatalf("owner of FROM should bypass slug; body=%s", w.Body.String())
	}
	if caller != "manager" {
		t.Errorf("caller = %q, want manager", caller)
	}
}

// TestRequireScopedLinkAuthority_OwnerOfToDoesNotBypass: an owner of
// the link's TO side does NOT get the slug bypass. Owners can't
// unilaterally cut their inbound channels. Regression for the
// CodeRabbit critical comment on PR #51.
func TestRequireScopedLinkAuthority_OwnerOfToDoesNotBypass(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	b, _ := db.CreateAgentGroup("b", "")
	id, _ := db.InsertAgentGroupLink(a, b, db.LinkModeMembersToMembers, "")
	link, _ := db.GetAgentGroupLinkByID(id)
	groupB, _ := db.GetAgentGroupByID(b)
	if err := db.AddAgentGroupOwner(b, "manager", "<test>"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	if _, ok := requireScopedLinkAuthority(w, r, groupB, link, PermGroupsLinkRm); ok {
		t.Errorf("owner of TO should NOT bypass; expected 403 forcing the slug")
	}
}

// TestRequireScopedLinkAuthority_GrantedSlugAllowsRegardlessOfSide: an
// agent holding the slug passes even when scoped under the TO side.
func TestRequireScopedLinkAuthority_GrantedSlugAllowsRegardlessOfSide(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	b, _ := db.CreateAgentGroup("b", "")
	id, _ := db.InsertAgentGroupLink(a, b, db.LinkModeMembersToMembers, "")
	link, _ := db.GetAgentGroupLinkByID(id)
	groupB, _ := db.GetAgentGroupByID(b)
	if err := db.GrantAgentPermission("manager", PermGroupsLinkRm, "<test>"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireScopedLinkAuthority(w, r, groupB, link, PermGroupsLinkRm)
	if !ok {
		t.Fatalf("slug holder should pass even on TO side; body=%s", w.Body.String())
	}
	if caller != "manager" {
		t.Errorf("caller = %q, want manager", caller)
	}
}
