package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestOpenCodePeerIdentity_WhoamiAndInbox is the TCL-694 flow regression.
// It models the server-authoritative process topology OpenCode 1.18.4 uses:
//
//	tclaude socket peer -> shell -> agentd-owned opencode serve
//
// The serve pid exists only in opencode_runtimes (the sessions row tracks the
// attach pane instead). The same PID walk the Unix-socket middleware performs
// must resolve the non-UUID ses_ conversation, classify it as an agent, enroll
// that conversation shape, and authorize the production whoami + inbox list /
// read handlers.
func TestOpenCodePeerIdentity_WhoamiAndInbox(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 9300
		shellPID = 9200
		servePID = 9100
	)
	const convID = "ses_opencode_identity_test"

	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-opencode",
		ConvID:    convID,
		ServerURL: "http://127.0.0.1:43210",
		Password:  "private",
		PID:       servePID,
		Cwd:       "/tmp/project",
	}))
	fakeProcTree{
		name: map[int]string{
			peerPID:  "tclaude",
			shellPID: "bash",
			servePID: "opencode",
		},
		parent: map[int]int{
			peerPID:  shellPID,
			shellPID: servePID,
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	require.True(t, hasAncestor, "the opencode server must be recognised as a harness ancestor")
	require.Equal(t, convID, gotConv, "the serve pid must resolve through opencode_runtimes")
	require.Equal(t, classAgent, classify(&peer{
		PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}))

	// Mirror withIdentity's catch-all enrollment after resolving the peer.
	// This proves the OpenCode ses_ id shape is accepted without UUID
	// assumptions and receives a stable actor identity on its first call.
	enrolledCallers.Delete(convID)
	t.Cleanup(func() { enrolledCallers.Delete(convID) })
	enrollCallerOnce(gotConv)
	agentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "first resolved OpenCode call must enroll the ses_ conversation")
	require.NoError(t, db.SetAgentPendingName(agentID, "opencode-worker"))

	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		FromConv: "sender-conv",
		ToConv:   convID,
		Subject:  "startup brief",
		Body:     "verify OpenCode inbox identity",
	})
	require.NoError(t, err)

	mux := buildMux()
	serveAsProcessPeer := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		resolved, found := convIDForPID(peerPID)
		if resolved != "" {
			enrollCallerOnce(resolved)
		}
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{
			PID: peerPID, ConvID: resolved, HasClaudeAncestor: found,
		}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	whoami := serveAsProcessPeer(http.MethodGet, "/v1/whoami")
	require.Equal(t, http.StatusOK, whoami.Code, "whoami body=%s", whoami.Body.String())
	var identity whoamiResp
	require.NoError(t, json.Unmarshal(whoami.Body.Bytes(), &identity))
	assert.Equal(t, convID, identity.ConvID)
	assert.Equal(t, agentID, identity.AgentID)
	assert.Equal(t, "opencode-worker", identity.Title)

	inbox := serveAsProcessPeer(http.MethodGet, "/v1/inbox?limit=20")
	require.Equal(t, http.StatusOK, inbox.Code, "inbox body=%s", inbox.Body.String())
	var items []inboxItem
	require.NoError(t, json.Unmarshal(inbox.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, messageID, items[0].ID)
	assert.Equal(t, "startup brief", items[0].Subject)

	read := serveAsProcessPeer(http.MethodGet, fmt.Sprintf("/v1/messages/%d", messageID))
	require.Equal(t, http.StatusOK, read.Code, "inbox read body=%s", read.Body.String())
	assert.Contains(t, read.Body.String(), "verify OpenCode inbox identity")
	stored, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.False(t, stored.ReadAt.IsZero(), "recipient read must mark the inbox row read")
}

// TestConvIDForPID_OpenCodeRuntimeResolvesViaMatchedAncestorParent covers the
// belt-and-suspenders probe required by TCL-694. If the harness-named process
// reached by the walk is an OpenCode child/intermediate but the registered
// authoritative runtime is its differently-named parent, the parent's
// opencode_runtimes PID still resolves before the walk leaves that ancestor.
func TestConvIDForPID_OpenCodeRuntimeResolvesViaMatchedAncestorParent(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID         = 8300
		openCodeChild   = 8200
		recordedRuntime = 8100
	)
	const convID = "ses_opencode_parent_runtime"

	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-parent-runtime",
		ConvID:    convID,
		ServerURL: "http://127.0.0.1:43211",
		Password:  "private",
		PID:       recordedRuntime,
		Cwd:       "/tmp/project",
	}))
	fakeProcTree{
		name: map[int]string{
			peerPID:         "tclaude",
			openCodeChild:   "opencode",
			recordedRuntime: "bun",
		},
		parent: map[int]int{
			peerPID:       openCodeChild,
			openCodeChild: recordedRuntime,
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv,
		"the matched OpenCode ancestor's parent must be checked against opencode_runtimes")
}

// TestConvIDForPID_OpenCodeAttachResolvesViaPaneSessionPID proves the
// pre-existing sessions.pid probes remain the fallback for the other possible
// topology: the socket peer runs below the attach client in the tmux pane and
// the session row is keyed by that client's parent pane shell. The OpenCode
// runtime lookup is additive; this path must keep resolving before it.
func TestConvIDForPID_OpenCodeAttachResolvesViaPaneSessionPID(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID   = 7300
		attachPID = 7200
		paneShell = 7100
	)
	const convID = "ses_opencode_attach_pane"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "opencode-attach",
		PID:     paneShell,
		ConvID:  convID,
		Harness: "opencode",
		Status:  "working",
	}))
	fakeProcTree{
		name: map[int]string{
			peerPID:   "tclaude",
			attachPID: "opencode",
			paneShell: "sh",
		},
		parent: map[int]int{
			peerPID:   attachPID,
			attachPID: paneShell,
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv,
		"the existing sessions.pid(parent) probe must resolve an OpenCode attach ancestry")
}
