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

// TestOwnerOfGroupContaining_RotationImmune pins the JOH-323 behaviour: the
// membership match resolves the target to its stable agent, so a member named
// by ANY of its generations is recognised. The pre-JOH-323 conv-literal scan
// (m.ConvID == targetConv) only matched the member's CURRENT conv, so naming
// the predecessor generation returned false — this asserts both generations
// now resolve true.
func TestOwnerOfGroupContaining_RotationImmune(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker-old"}), "add member")
	require.NoError(t, db.AddAgentGroupOwner(gID, "manager", "<test>"), "add owner")

	// The worker reincarnates: worker-new becomes the live generation of the
	// same actor; worker-old is now a predecessor conv of that same agent.
	_, err := db.RotateAgentConv("worker-old", "worker-new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	assert.True(t, ownerOfGroupContaining("manager", "worker-new"),
		"current generation of the member resolves to the member's agent")
	assert.True(t, ownerOfGroupContaining("manager", "worker-old"),
		"predecessor generation still resolves to the same agent (rotation-immune)")
	assert.False(t, ownerOfGroupContaining("manager", "stranger"),
		"unrelated conv is still not a member")
}

// TestSameActor exercises the rotation-immune actor-equality primitive that
// backs the JOH-323 self-checks. It must (1) treat two generations of one
// agent as equal, (2) keep distinct agents distinct, and (3) preserve the
// non-agent semantics of the old conv-literal compare — a plain conversation
// (no actor row) matches only itself, never another conv and never an agent.
func TestSameActor(t *testing.T) {
	setupTestDB(t)

	// Agent A across two generations (a /clear or reincarnate rotation).
	_, _, err := db.EnsureAgentForConv("a-gen1", "test")
	require.NoError(t, err, "enrol a-gen1")
	_, err = db.RotateAgentConv("a-gen1", "a-gen2", "reincarnate")
	require.NoError(t, err, "rotate a-gen1 -> a-gen2")

	// A separate agent B.
	_, _, err = db.EnsureAgentForConv("b-gen1", "test")
	require.NoError(t, err, "enrol b-gen1")

	// Same conv-id → equal (cheap short-circuit, no DB hit needed).
	assert.True(t, sameActor("a-gen1", "a-gen1"), "identical conv is the same actor")
	assert.True(t, sameActor("plain-1", "plain-1"), "identical non-agent conv is the same actor")

	// Two generations of the SAME agent → equal (the rotation-immune case).
	assert.True(t, sameActor("a-gen1", "a-gen2"), "two generations of agent A are the same actor")
	assert.True(t, sameActor("a-gen2", "a-gen1"), "order does not matter")

	// Distinct agents → NOT equal (no over-authorization).
	assert.False(t, sameActor("a-gen1", "b-gen1"), "agent A and agent B are different actors")
	assert.False(t, sameActor("a-gen2", "b-gen1"), "any generation of A differs from B")

	// Non-agent conv vs anything but itself → NOT equal (preserves the old
	// conv-literal semantics; no plain conversation collisions).
	assert.False(t, sameActor("plain-1", "plain-2"), "two distinct non-agent convs are not the same actor")
	assert.False(t, sameActor("a-gen1", "plain-1"), "an agent never matches a non-agent conv")
	assert.False(t, sameActor("plain-1", "a-gen1"), "a non-agent conv never matches an agent")

	// Empty conv-id is "no actor" → fail-closed, never equal to anything
	// (including another empty), regardless of the a == b short-circuit.
	assert.False(t, sameActor("", ""), "two empty conv-ids are not the same actor")
	assert.False(t, sameActor("", "a-gen1"), "empty conv never matches an agent")
	assert.False(t, sameActor("a-gen1", ""), "an agent never matches an empty conv")
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
