package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// requestWithPeer returns a request with the given peer attached to
// context, so handlers reading peerFromContext see this identity.
func requestWithPeer(p *peer) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(r.Context(), peerKey{}, p)
	return r.WithContext(ctx)
}

func TestRequireCrossAgentPermission_HumanAlwaysPasses(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HumanTokenValid: true})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv")
	require.True(t, ok, "human caller should pass; body=%s", w.Body.String())
	assert.Empty(t, caller, "human caller convID should be empty")
}

func TestRequireCrossAgentPermission_NoPID(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 0})
	_, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv")
	assert.False(t, ok, "should fail when PID is unknown")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "status")
}

func TestRequireCrossAgentPermission_AgentNoConv(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: ""})
	_, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv")
	assert.False(t, ok, "agent without conv-id should fail")
	assert.Equal(t, http.StatusForbidden, w.Code, "status")
}

func TestRequireCrossAgentPermission_DeniedWithoutSlugOrOwner(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	_, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "stranger")
	assert.False(t, ok, "agent with no slug and no shared-owner relation should be denied")
	assert.Equal(t, http.StatusForbidden, w.Code, "status; body=%s", w.Body.String())
}

func TestRequireCrossAgentPermission_GrantedSlugAllows(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentReincarnate, "<test>"), "grant")
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "stranger")
	require.True(t, ok, "granted slug should allow; body=%s", w.Body.String())
	assert.Equal(t, "manager", caller, "caller")
}

func TestRequireCrossAgentPermission_GroupOwnerOfTargetAllows(t *testing.T) {
	setupTestDB(t)
	gID, err := db.CreateAgentGroup("team", "")
	require.NoError(t, err, "create group")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker",
	}), "add member")
	require.NoError(t, db.AddAgentGroupOwner(gID, "manager", "<test>"), "add owner")
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "worker")
	require.True(t, ok, "group owner of target should allow without slug; body=%s", w.Body.String())
	assert.Equal(t, "manager", caller, "caller")
}

func TestRequireCrossAgentPermission_OwnerOfDifferentGroupDoesNotAllow(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("team-a", "")
	g2, _ := db.CreateAgentGroup("team-b", "")
	// manager owns team-a but the target is in team-b.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "worker-a"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "worker-b"})
	_ = db.AddAgentGroupOwner(g1, "manager", "<test>")

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	_, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "worker-b")
	assert.False(t, ok, "owner of unrelated group should NOT allow targeting worker-b")
}

func TestOwnerOfGroupContaining(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker"})
	_ = db.AddAgentGroupOwner(gID, "manager", "<test>")

	assert.True(t, ownerOfGroupContaining("manager", "worker"),
		"manager owns group containing worker; should return true")
	assert.False(t, ownerOfGroupContaining("manager", "stranger"),
		"stranger is not in any of manager's groups; should return false")
	assert.False(t, ownerOfGroupContaining("not-an-owner", "worker"),
		"non-owner should return false")
}

func TestHandleAgentByConv_BadPath(t *testing.T) {
	setupTestDB(t)
	cases := []string{
		"/v1/agent/",
		"/v1/agent/justaselector",
		"/v1/agent//reincarnate",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, path, nil)
			handleAgentByConv(w, r)
			assert.Equal(t, http.StatusNotFound, w.Code, "path %q", path)
		})
	}
}

func TestHandleAgentByConv_KnownVerbsRoute(t *testing.T) {
	// Both supported verbs should resolve and reach their handler
	// (which will then 503 because there's no live tmux session in
	// the test, but the dispatcher routing itself succeeds).
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	// Grant manager the slug for both verbs so we get past auth.
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentReincarnate, "<test>"), "grant")
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentCompact, "<test>"), "grant")
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentRename, "<test>"), "grant")
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentClone, "<test>"), "grant")
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentStop, "<test>"), "grant")
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"), "grant")
	for _, verb := range []string{"reincarnate", "compact", "rename", "clone", "stop", "resume"} {
		t.Run(verb, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/"+verb, nil)
			r = r.WithContext(context.WithValue(r.Context(), peerKey{},
				&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
			handleAgentByConv(w, r)
			// Anything other than 404 or 401/403 means the dispatcher
			// routed the request to a real handler.
			assert.NotEqual(t, http.StatusNotFound, w.Code,
				"verb %q should be recognised; body=%s", verb, w.Body.String())
			assert.False(t, w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden,
				"verb %q auth check failed; got %d body=%s", verb, w.Code, w.Body.String())
		})
	}
}

func TestHandleAgentByConv_UnknownVerb(t *testing.T) {
	setupTestDB(t)
	// Seed a conv so the selector resolves.
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker-conv-id-12345678"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/teleport", nil)
	handleAgentByConv(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "unknown verb; body=%s", w.Body.String())
}

func TestHandlePeers_HumanSeesAllGroups(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("alpha", "")
	g2, _ := db.CreateAgentGroup("beta", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "agent-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "agent-2"})

	// Human caller: operator token, no claude ancestor.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	handlePeers(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "agent-1", "human caller should see agent-1")
	assert.Contains(t, body, "agent-2", "human caller should see agent-2")
}

func TestHandlePeers_AgentScopedToOwnGroups(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("alpha", "")
	g2, _ := db.CreateAgentGroup("beta", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "me"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "peer-in-group"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "stranger"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HasClaudeAncestor: true, ConvID: "me"}))
	handlePeers(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "peer-in-group", "agent should see peer-in-group")
	assert.NotContains(t, body, "stranger", "agent should NOT see stranger from a different group")
	assert.NotContains(t, body, `"conv_id":"me"`, "agent should not appear in its own peers list")
}
