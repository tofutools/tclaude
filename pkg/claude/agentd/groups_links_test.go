package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	r := requestWithPeer(&peer{PID: 999, HumanTokenValid: true})
	_, ok := requireScopedLinkAuthority(w, r, groupA, link, PermGroupsLinkRemove)
	require.True(t, ok, "human should pass; body=%s", w.Body.String())
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
	require.NoError(t, db.AddAgentGroupOwner(a, "manager", "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireScopedLinkAuthority(w, r, groupA, link, PermGroupsLinkRemove)
	require.True(t, ok, "owner of FROM should bypass slug; body=%s", w.Body.String())
	assert.Equal(t, "manager", caller, "caller")
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
	require.NoError(t, db.AddAgentGroupOwner(b, "manager", "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	_, ok := requireScopedLinkAuthority(w, r, groupB, link, PermGroupsLinkRemove)
	assert.False(t, ok, "owner of TO should NOT bypass; expected 403 forcing the slug")
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
	require.NoError(t, db.GrantAgentPermission("manager", PermGroupsLinkRemove, "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireScopedLinkAuthority(w, r, groupB, link, PermGroupsLinkRemove)
	require.True(t, ok, "slug holder should pass even on TO side; body=%s", w.Body.String())
	assert.Equal(t, "manager", caller, "caller")
}

// TestRequireGroupLinkAuthority_OwnerBypassesWhenUndecided: with no
// grant and no deny, owning the FROM group is enough to create a link.
func TestRequireGroupLinkAuthority_OwnerBypassesWhenUndecided(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	groupA, _ := db.GetAgentGroupByID(a)
	require.NoError(t, db.AddAgentGroupOwner(a, "manager", "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireGroupLinkAuthority(w, r, groupA, PermGroupsLinkAdd)
	require.True(t, ok, "undecided owner should bypass slug; body=%s", w.Body.String())
	assert.Equal(t, "manager", caller, "caller")
}

// TestRequireGroupLinkAuthority_DenyBeatsOwnerBypass: an explicit
// per-agent deny on groups.link.add is authoritative and suppresses the
// owner-of-FROM bypass, matching every other owner-implied slug
// (TCL-1018).
func TestRequireGroupLinkAuthority_DenyBeatsOwnerBypass(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	groupA, _ := db.GetAgentGroupByID(a)
	require.NoError(t, db.AddAgentGroupOwner(a, "manager", "<test>"))
	require.NoError(t, db.SetAgentPermissionOverride("manager", PermGroupsLinkAdd, db.PermEffectDeny, "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	_, ok := requireGroupLinkAuthority(w, r, groupA, PermGroupsLinkAdd)
	require.False(t, ok, "denied owner should be refused link-create")
	assert.Equal(t, http.StatusForbidden, w.Code, "status")
	assert.Contains(t, w.Body.String(), PermGroupsLinkAdd, "403 should name the slug")
}

// TestRequireScopedLinkAuthority_DenyBeatsOwnerBypass: same for the
// PATCH/DELETE path — owning the FROM side no longer overrides a deny
// on groups.link.remove (TCL-1018).
func TestRequireScopedLinkAuthority_DenyBeatsOwnerBypass(t *testing.T) {
	setupTestDB(t)
	a, _ := db.CreateAgentGroup("a", "")
	b, _ := db.CreateAgentGroup("b", "")
	id, _ := db.InsertAgentGroupLink(a, b, db.LinkModeMembersToMembers, "")
	link, _ := db.GetAgentGroupLinkByID(id)
	groupA, _ := db.GetAgentGroupByID(a)
	require.NoError(t, db.AddAgentGroupOwner(a, "manager", "<test>"))
	require.NoError(t, db.SetAgentPermissionOverride("manager", PermGroupsLinkRemove, db.PermEffectDeny, "<test>"))

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	_, ok := requireScopedLinkAuthority(w, r, groupA, link, PermGroupsLinkRemove)
	require.False(t, ok, "denied owner should be refused link-rm on the FROM side")
	assert.Equal(t, http.StatusForbidden, w.Code, "status")
	assert.Contains(t, w.Body.String(), PermGroupsLinkRemove, "403 should name the slug")
}
