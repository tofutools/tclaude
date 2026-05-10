package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: false})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv")
	if !ok {
		t.Fatalf("human caller should pass; got 403, body=%s", w.Body.String())
	}
	if caller != "" {
		t.Errorf("human caller convID should be empty, got %q", caller)
	}
}

func TestRequireCrossAgentPermission_NoPID(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 0})
	if _, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv"); ok {
		t.Errorf("should fail when PID is unknown")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireCrossAgentPermission_AgentNoConv(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: ""})
	if _, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "target-conv"); ok {
		t.Errorf("agent without conv-id should fail")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRequireCrossAgentPermission_DeniedWithoutSlugOrOwner(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	if _, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "stranger"); ok {
		t.Errorf("agent with no slug and no shared-owner relation should be denied")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestRequireCrossAgentPermission_GrantedSlugAllows(t *testing.T) {
	setupTestDB(t)
	if err := db.GrantAgentPermission("manager", PermAgentReincarnate, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "stranger")
	if !ok {
		t.Fatalf("granted slug should allow; body=%s", w.Body.String())
	}
	if caller != "manager" {
		t.Errorf("caller = %q, want manager", caller)
	}
}

func TestRequireCrossAgentPermission_GroupOwnerOfTargetAllows(t *testing.T) {
	setupTestDB(t)
	gID, err := db.CreateAgentGroup("team", "")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker", Alias: "w1",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := db.AddAgentGroupOwner(gID, "manager", "<test>"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: "manager"})
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "worker")
	if !ok {
		t.Fatalf("group owner of target should allow without slug; body=%s", w.Body.String())
	}
	if caller != "manager" {
		t.Errorf("caller = %q, want manager", caller)
	}
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
	if _, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, "worker-b"); ok {
		t.Errorf("owner of unrelated group should NOT allow targeting worker-b")
	}
}

func TestOwnerOfGroupContaining(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker"})
	_ = db.AddAgentGroupOwner(gID, "manager", "<test>")

	if !ownerOfGroupContaining("manager", "worker") {
		t.Errorf("manager owns group containing worker; should return true")
	}
	if ownerOfGroupContaining("manager", "stranger") {
		t.Errorf("stranger is not in any of manager's groups; should return false")
	}
	if ownerOfGroupContaining("not-an-owner", "worker") {
		t.Errorf("non-owner should return false")
	}
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
			if w.Code != http.StatusNotFound {
				t.Errorf("path %q got %d, want 404", path, w.Code)
			}
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
		GroupID: gID, ConvID: "worker-conv-id-12345678", Alias: "w",
	})
	// Grant manager the slug for both verbs so we get past auth.
	if err := db.GrantAgentPermission("manager", PermAgentReincarnate, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := db.GrantAgentPermission("manager", PermAgentCompact, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := db.GrantAgentPermission("manager", PermAgentRename, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := db.GrantAgentPermission("manager", PermAgentClone, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := db.GrantAgentPermission("manager", PermAgentStop, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := db.GrantAgentPermission("manager", PermAgentResume, "<test>"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	for _, verb := range []string{"reincarnate", "compact", "rename", "clone", "stop", "resume"} {
		t.Run(verb, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/"+verb, nil)
			r = r.WithContext(context.WithValue(r.Context(), peerKey{},
				&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
			handleAgentByConv(w, r)
			// Anything other than 404 or 401/403 means the dispatcher
			// routed the request to a real handler.
			if w.Code == http.StatusNotFound {
				t.Errorf("verb %q should be recognised; got 404 body=%s", verb, w.Body.String())
			}
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
				t.Errorf("verb %q auth check failed; got %d body=%s", verb, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleAgentByConv_UnknownVerb(t *testing.T) {
	setupTestDB(t)
	// Seed a conv so the selector resolves.
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "worker-conv-id-12345678", Alias: "w"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/teleport", nil)
	handleAgentByConv(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown verb got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandlePeers_HumanSeesAllGroups(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("alpha", "")
	g2, _ := db.CreateAgentGroup("beta", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "agent-1", Alias: "a1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "agent-2", Alias: "a2"})

	// Human caller: PID set, but no claude ancestor and no conv-id.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HasClaudeAncestor: false}))
	handlePeers(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "agent-1") || !strings.Contains(body, "agent-2") {
		t.Errorf("human caller should see both agents; got body=%s", body)
	}
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
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "peer-in-group") {
		t.Errorf("agent should see peer-in-group; body=%s", body)
	}
	if strings.Contains(body, "stranger") {
		t.Errorf("agent should NOT see stranger from a different group; body=%s", body)
	}
	if strings.Contains(body, `"conv_id":"me"`) {
		t.Errorf("agent should not appear in its own peers list; body=%s", body)
	}
}
