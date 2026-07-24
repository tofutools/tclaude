package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	withOpenCodeRuntimeVerified(t, true)

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
	withOpenCodeRuntimeVerified(t, true)

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

func TestConvIDForPID_OpenCodeRuntimeDBErrorFailsClosed(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 6300
		servePID = 6200
	)
	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", servePID: "opencode"},
		parent: map[int]int{peerPID: servePID},
	}.install(t)

	d, err := db.Open()
	require.NoError(t, err)
	require.NoError(t, d.Close())

	assertOpenCodeAgentUnknown(t, peerPID)
}

func TestConvIDForPID_OpenCodePremintRuntimeFailsClosed(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 5300
		servePID = 5200
	)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-premint",
		ConvID:    "",
		ServerURL: "http://127.0.0.1:43212",
		Password:  "private",
		PID:       servePID,
		Cwd:       "/tmp/project",
	}))
	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", servePID: "opencode"},
		parent: map[int]int{peerPID: servePID},
	}.install(t)
	// A verified but not-yet-minted runtime still yields no conv-id: this proves
	// the empty-conv path fails closed independently of the ownership gate.
	withOpenCodeRuntimeVerified(t, true)

	assertOpenCodeAgentUnknown(t, peerPID)
}

// TestConvIDForPID_OpenCodeStalePIDReuseFailsClosed is the TCL-678 identity
// hardening regression. When an `opencode serve` crashes its runtime row
// lingers with a stale pid until reconcile/reap clears it. If a same-uid,
// `opencode`-named process inherits that pid in the meantime, the walk would
// reach an OpenCode-named ancestor at the recorded pid and — before this
// hardening — resolve the victim conversation, misclassifying the impostor as
// classAgent. The recovered-pid ownership gate rejects the match (the crashed
// server freed its port, so the impostor cannot own the recorded endpoint), so
// the lingering row must resolve to no conv-id / classAgentUnknown.
func TestConvIDForPID_OpenCodeStalePIDReuseFailsClosed(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4300
		servePID = 4200
	)
	const convID = "ses_opencode_victim"
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-victim",
		ConvID:    convID,
		ServerURL: "http://127.0.0.1:43213",
		Password:  "private",
		PID:       servePID,
		Cwd:       "/tmp/project",
	}))
	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", servePID: "opencode"},
		parent: map[int]int{peerPID: servePID},
	}.install(t)
	// The recorded pid no longer owns the recorded endpoint (crashed + reused).
	withOpenCodeRuntimeVerified(t, false)

	assertOpenCodeAgentUnknown(t, peerPID)
}

// TestConvIDForPID_OpenCodeSelfPIDParentProbeFailsClosed guards the TCL-678
// review follow-up: a managed serve is agentd's direct child, so convIDForPID's
// parent probe can pass agentd's own pid to openCodeRuntimeConvByPID. Subtree
// endpoint ownership would still match agentd (managed serves are its children),
// so the ownership gate alone would not reject a stale runtime row whose reused
// pid equals ours. Identity resolution must fail closed on the self pid instead
// of resolving the victim conversation.
func TestConvIDForPID_OpenCodeSelfPIDParentProbeFailsClosed(t *testing.T) {
	setupTestDB(t)

	// Derive the fake pids from our own so they can never collide with it (the
	// walk stubs procName/procParent, so the concrete values are arbitrary).
	agentdPID := os.Getpid()
	peerPID := agentdPID + 1_000_000
	openCodeChild := agentdPID + 2_000_000
	const convID = "ses_self_pid_victim"
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-self-pid-victim",
		ConvID:    convID,
		ServerURL: "http://127.0.0.1:43299",
		Password:  "private",
		PID:       agentdPID, // a stale row whose reused pid equals agentd's own
		Cwd:       "/tmp/project",
	}))
	fakeProcTree{
		name: map[int]string{
			peerPID:       "tclaude",
			openCodeChild: "opencode",
			agentdPID:     "agentd",
		},
		parent: map[int]int{
			peerPID:       openCodeChild,
			openCodeChild: agentdPID, // the serve's parent IS agentd
		},
	}.install(t)
	// Force the ownership gate to accept, isolating the self-pid guard as the
	// sole reason resolution fails closed.
	withOpenCodeRuntimeVerified(t, true)

	assertOpenCodeAgentUnknown(t, peerPID)
}

// withOpenCodeRuntimeVerified overrides the recovered-pid ownership gate so a
// synthetic proc tree (which binds no real listening socket) can drive identity
// resolution deterministically. It restores the production verifier on cleanup.
func withOpenCodeRuntimeVerified(t *testing.T, verified bool) {
	t.Helper()
	prev := openCodeRuntimeVerified
	openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool { return verified }
	t.Cleanup(func() { openCodeRuntimeVerified = prev })
}

func assertOpenCodeAgentUnknown(t *testing.T, peerPID int) {
	t.Helper()

	gotConv, hasAncestor := convIDForPID(peerPID)
	require.True(t, hasAncestor)
	require.Empty(t, gotConv)
	p := &peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}
	require.Equal(t, classAgentUnknown, classify(p))

	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, p))
	rec := httptest.NewRecorder()
	_, _, ok := authedCaller(rec, req)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
