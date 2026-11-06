package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// tcl-99b5ir gap 2: an operator's own (unrestricted) node reads this node's
// human inbox and answers it once; other grants do not reach it, and lasting
// or local-only decisions stay local.
func TestPeerHumanInboxUnrestrictedAndOneShot(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	fh := newFedHarness(t)
	f := fh.f
	var clip clipRecorder
	clip.install(t)
	f.HaveGroup("shared")
	f.HaveAliveSession(moveSourceConv, "inbox-agent", "inbox-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("shared", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	serve := func(method, path string, body any) (int, string) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}

	long := strings.Repeat("é", 3000)
	agentMsg, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: moveSourceConv, FromAgent: aid, FromTitle: "worker", GroupName: "shared", Subject: "need input", Body: long, CreatedAt: time.Now()})
	require.NoError(t, err)
	processMsg, err := db.InsertHumanMessage(&db.HumanMessage{Subject: "process step", Body: "decide", ProcessRunID: "run1", ProcessCommandID: "cmd1", CreatedAt: time.Now()})
	require.NoError(t, err)

	// Restricted trust holding other grants sees nothing.
	require.Equal(t, 200, fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=shared"}).Code)
	for _, req := range []struct{ method, path string }{{"GET", "/api/human-inbox"}, {"POST", "/api/human-inbox/reply"}, {"POST", "/api/human-inbox/read"}, {"POST", "/api/human-inbox/access/x"}} {
		code, body := serve(req.method, req.path, map[string]any{"id": agentMsg, "body": "hi"})
		require.Equal(t, 403, code, req.path+": "+body)
	}
	// A pending peer access request stays out of the remote inbox.
	code, body := serve("POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermMessageDirect, "reason": "coordinate"})
	require.Equal(t, 202, code, body)

	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))

	// An agent blocked on --ask-human.
	type result struct{ code int }
	done := make(chan result, 1)
	go func() {
		r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard", map[string]any{"text": "remote approved"})
		r.Header.Set("X-Tclaude-Ask-Human", "30s")
		r = agentd.AsAgentPeer(r, moveSourceConv)
		done <- result{testharness.Serve(f.Mux, r).Code}
	}()
	type inbox struct {
		Messages []struct {
			ID        int64  `json:"id"`
			Body      string `json:"body"`
			Truncated bool   `json:"truncated"`
			Read      bool   `json:"read"`
			Replyable bool   `json:"replyable"`
			FromAgent string `json:"from_agent"`
		} `json:"messages"`
		AccessRequests []struct {
			ID   string `json:"id"`
			Perm string `json:"perm"`
		} `json:"access_requests"`
	}
	var got inbox
	require.Eventually(t, func() bool {
		code, body := serve("GET", "/api/human-inbox", nil)
		if code != 200 || json.Unmarshal([]byte(body), &got) != nil {
			return false
		}
		return len(got.AccessRequests) == 1
	}, 5*time.Second, 20*time.Millisecond, "the agent's ask-human request is listed, the peer access request is not")
	require.Equal(t, agentd.PermHumanClipboard, got.AccessRequests[0].Perm)
	require.Len(t, got.Messages, 2)
	require.Equal(t, processMsg, got.Messages[0].ID, "newest first")
	require.False(t, got.Messages[0].Replyable, "a process obligation is resolved locally")
	require.True(t, got.Messages[1].Replyable)
	require.Equal(t, aid, got.Messages[1].FromAgent)
	require.True(t, got.Messages[1].Truncated)
	require.LessOrEqual(t, len(got.Messages[1].Body), 4096)
	require.True(t, strings.HasPrefix(long, got.Messages[1].Body), "cut on a rune boundary")

	code, body = serve("POST", "/api/human-inbox/reply", map[string]any{"id": processMsg, "body": "yes"})
	require.Equal(t, 409, code, body)
	code, body = serve("POST", "/api/human-inbox/reply", map[string]any{"id": agentMsg, "body": "go ahead", "attachment_token": "x"})
	require.Equal(t, 400, code, "no attachments remotely: "+body)
	code, body = serve("POST", "/api/human-inbox/reply", map[string]any{"id": agentMsg, "body": "go ahead"})
	require.Equal(t, 200, code, body)
	code, body = serve("POST", "/api/human-inbox/read", map[string]any{"id": processMsg})
	require.Equal(t, 200, code, body)
	m, err := db.GetHumanMessage(processMsg)
	require.NoError(t, err)
	require.False(t, m.ReadAt.IsZero())
	code, body = serve("POST", "/api/human-inbox/read", map[string]any{"id": processMsg, "read": false})
	require.Equal(t, 200, code, body)
	m, err = db.GetHumanMessage(processMsg)
	require.NoError(t, err)
	require.True(t, m.ReadAt.IsZero(), "read:false marks the notification unread again")
	code, body = serve("POST", "/api/human-inbox/read", map[string]any{"all": true})
	require.Equal(t, 200, code, body)
	m, err = db.GetHumanMessage(agentMsg)
	require.NoError(t, err)
	require.False(t, m.ReadAt.IsZero(), "all:true marks every notification read")

	reqID := got.AccessRequests[0].ID
	for _, d := range []string{"always", "always_scoped", "extend", " approve"} {
		code, body = serve("POST", "/api/human-inbox/access/"+reqID, map[string]any{"decision": d})
		require.Equal(t, 400, code, d+": "+body)
	}
	code, body = serve("POST", "/api/human-inbox/access/not-pending", map[string]any{"decision": "approve"})
	require.Equal(t, 404, code, body)
	code, body = serve("POST", "/api/human-inbox/access/"+reqID, map[string]any{"decision": "approve"})
	require.Equal(t, 200, code, body)
	select {
	case r := <-done:
		require.Equal(t, 200, r.code, "the one-shot approval lets the blocked call through")
		require.Equal(t, []string{"remote approved"}, clip.texts)
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked call was not released")
	}
	_, ok, err := db.AgentPermissionOverride(moveSourceConv, agentd.PermHumanClipboard)
	require.NoError(t, err)
	require.False(t, ok, "a remote answer leaves no lasting grant")
	require.Eventually(t, func() bool {
		rows, err := db.ListAuditLog(db.AuditLogFilter{})
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.ActorKind == db.AuditActorHuman && row.ActorLabel == "operator@"+peer.InstanceID {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the approval history names the deciding node")
	code, body = serve("POST", "/api/human-inbox/access/"+reqID, map[string]any{"decision": "deny"})
	require.Equal(t, 404, code, "one answer only: "+body)
}

type peerInboxView struct {
	Messages []struct {
		ID        int64 `json:"id"`
		Replyable bool  `json:"replyable"`
	} `json:"messages"`
	AccessRequests []struct {
		ID            string `json:"id"`
		Approvable    bool   `json:"approvable"`
		ApproveReason string `json:"approve_reason"`
	} `json:"access_requests"`
}

// A restricted peer reads and answers the human inbox only within the groups
// its human.inbox grants name: an agent's notices sent in such a group, never
// a senderless one, and the requests of an agent all of whose groups it holds
// the grant in. Deny takes only the answer grant; approve also takes the
// requested slug, as the action is scoped, within the peer ceiling, checked
// when it answers. Every answer is audited with the peer and the slug.
func TestPeerHumanInboxWithinGrantedGroups(t *testing.T) {
	h := newPeerProcessHarness(t)
	f := h.f
	f.HaveGroup("secret")
	secret := havePerformerAgent(t, f, "secret", "sec-conv", "sec", "dev", "idle")
	both := havePerformerAgent(t, f, "crew", "both-conv", "both", "dev", "idle")
	f.HaveMemberWithRole("secret", "both-conv", "dev")
	notice := func(conv, agentID, group string) int64 {
		id, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: conv, FromAgent: agentID, FromTitle: "x", GroupName: group, Subject: "s", Body: "b", CreatedAt: time.Now()})
		require.NoError(t, err)
		return id
	}
	mine := notice("proc-crew-member", h.member, "crew")
	hidden := notice("sec-conv", secret, "secret")
	senderless := notice("", "", "crew")
	elsewhere := notice("proc-crew-member", h.member, "secret")
	crewScope := `{"group":["crew"]}`
	takeA := agentd.AddPendingApprovalForTest("rA", agentd.PermGroupsRosterRead, h.member, "proc-crew-member", crewScope)
	takeB := agentd.AddPendingApprovalForTest("rB", agentd.PermGroupsRosterRead, secret, "sec-conv", `{"group":["secret"]}`)
	takeC := agentd.AddPendingApprovalForTest("rC", agentd.PermGroupsRosterRead, both, "both-conv", crewScope)
	takeD := agentd.AddPendingApprovalForTest("rD", agentd.PermHumanClipboard, h.member, "proc-crew-member", "")
	t.Cleanup(func() { takeA(); takeB(); takeC(); takeD() })
	read := func() peerInboxView {
		t.Helper()
		code, body := h.call("GET", "/api/human-inbox", nil)
		require.Equal(t, 200, code, body)
		var got peerInboxView
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		return got
	}
	decide := func(id, decision string) (int, string) {
		return h.call("POST", "/api/human-inbox/access/"+id, map[string]any{"decision": decision})
	}
	reply := func(id int64) (int, string) {
		return h.call("POST", "/api/human-inbox/reply", map[string]any{"id": id, "body": "go"})
	}

	code, body := h.call("GET", "/api/human-inbox", nil)
	require.Equal(t, 403, code, "no human.inbox grant: %s", body)
	h.grant(t, agentd.PermHumanInboxRead, "group=crew")
	got := read()
	require.Len(t, got.Messages, 1, "only an agent's notice sent in a granted group")
	require.Equal(t, mine, got.Messages[0].ID)
	require.Len(t, got.AccessRequests, 2, "not a request of an agent in a group the peer holds nothing in: %+v", got.AccessRequests)
	for _, r := range got.AccessRequests {
		require.Contains(t, []string{"rA", "rD"}, r.ID)
		require.False(t, r.Approvable, "no answer grant")
		require.NotEmpty(t, r.ApproveReason)
	}
	code, body = reply(mine)
	require.Equal(t, 403, code, "reading is not answering: %s", body)
	code, body = decide("rA", "deny")
	require.Equal(t, 403, code, body)

	h.grant(t, agentd.PermHumanInboxAnswer, "group=crew")
	for _, id := range []int64{hidden, senderless, elsewhere} {
		code, body = reply(id)
		require.Equal(t, 404, code, "a notice the peer does not read: %d %s", id, body)
	}
	code, body = reply(mine)
	require.Equal(t, 200, code, body)
	for _, id := range []string{"rB", "rC"} {
		code, body = decide(id, "deny")
		require.Equal(t, 404, code, "a request the peer does not read: %s %s", id, body)
	}

	// Outside the ceiling: the peer does not hold the slug it would approve.
	for _, r := range read().AccessRequests {
		require.False(t, r.Approvable, "%s: the ceiling", r.ID)
	}
	code, body = decide("rA", "approve")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsRosterRead)
	code, body = decide("rD", "approve")
	require.Equal(t, 403, code, "a slug no peer confers: %s", body)
	code, body = decide("rD", "deny")
	require.Equal(t, 200, code, "deny takes only the answer grant: %s", body)
	require.Equal(t, "deny", takeD())

	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	for _, r := range read().AccessRequests {
		require.True(t, r.Approvable, "%s: %s", r.ID, r.ApproveReason)
	}
	// Revoked after the listing said approvable: checked when it answers.
	h.revoke(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = decide("rA", "approve")
	require.Equal(t, 403, code, "approving after the grant was revoked: %s", body)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = decide("rA", "approve")
	require.Equal(t, 200, code, body)
	require.Equal(t, "approve", takeA())

	rows, err := db.ListAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	audited := map[string]bool{}
	for _, row := range rows {
		if strings.HasPrefix(row.ActorLabel, "remote:") && row.Status == 200 {
			for _, want := range []string{"decision=approve id=rA perm=" + agentd.PermGroupsRosterRead, "decision=deny id=rD perm=" + agentd.PermHumanClipboard, fmt.Sprintf("reply id=%d", mine)} {
				if strings.Contains(row.Detail, want) {
					audited[want] = true
				}
			}
		}
	}
	require.Len(t, audited, 3, "every answer is audited with the peer: %v", audited)

	// Unrestricted trust still reads every notice.
	h.trustUnrestricted(t)
	require.Len(t, read().Messages, 3, "unrestricted trust reads them all (the replied notice is still unread)")
}
