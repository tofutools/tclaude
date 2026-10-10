package agentd_test

import (
	"encoding/json"
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
// human inbox and answers it once; nothing short of unrestricted trust reaches
// it, and lasting or local-only decisions stay local.
func TestPeerHumanInboxUnrestrictedOnlyAndOneShot(t *testing.T) {
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

	// Restricted trust, even holding other grants, sees nothing and cannot ask.
	require.Equal(t, 200, fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=shared"}).Code)
	for _, req := range []struct{ method, path string }{{"GET", "/api/human-inbox"}, {"POST", "/api/human-inbox/reply"}, {"POST", "/api/human-inbox/read"}, {"POST", "/api/human-inbox/access/x"}} {
		code, body := serve(req.method, req.path, map[string]any{"id": agentMsg, "body": "hi"})
		require.Equal(t, 403, code, req.path+": "+body)
	}
	for _, slug := range []string{agentd.PermHumanInboxRead, agentd.PermHumanInboxAnswer} {
		code, body := serve("POST", "/api/peer-access-requests", map[string]any{"permission": slug, "reason": "x"})
		require.Equal(t, 400, code, "the inbox slugs cannot be requested: "+body)
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug})
		require.Equal(t, 400, rec.Code, "the inbox slugs cannot be granted: "+rec.Body.String())
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
}
