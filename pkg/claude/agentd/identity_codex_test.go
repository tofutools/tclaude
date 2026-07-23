package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// fakeProcTree stands in for the /proc readers convIDForPID walks. It
// models an ancestor chain as name + parent maps and installs itself over
// the procName / procParent package seams for the duration of a test.
type fakeProcTree struct {
	name   map[int]string
	parent map[int]int
}

func (f fakeProcTree) install(t *testing.T) {
	t.Helper()
	prevName, prevParent := procName, procParent
	procName = func(pid int) string { return f.name[pid] }
	procParent = func(pid int) int {
		if p, ok := f.parent[pid]; ok {
			return p
		}
		return 1 // reaching init ends the walk
	}
	t.Cleanup(func() { procName, procParent = prevName, prevParent })
}

// TestConvIDForPID_CodexAncestorResolvesViaPaneShPID is the JOH-206
// regression. A Codex agent's socket peer walks up to a "codex" ancestor
// (not claude/node) and — because Codex writes no per-pid session file — its
// conv-id comes from agentd's sessions table. The decisive detail this test
// pins: the spawn row is keyed by the tmux pane_pid, which is the
// `sh -c "… codex"` wrapper, and codex runs as that sh's DIRECT CHILD
// (verified live: codex pid 205165 was a child of sh pane pid 205164). So
// FindSessionByPID at the codex pid misses; the conv-id must resolve via the
// codex ancestor's PARENT pid — the pane sh, which is the row key.
func TestConvIDForPID_CodexAncestorResolvesViaPaneShPID(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4101 // the `tclaude agent` CLI process over the socket
		codexPID = 4050 // the codex runtime — recognised as the harness ancestor
		paneSh   = 4040 // the `sh -c "… codex"` pane wrapper = the tmux pane_pid
	)
	const convID = "11111111-2222-3333-4444-555555555555"

	// The spawn row `tclaude session new` actually writes: keyed by the
	// pane_pid (ParsePIDFromTmux) — the sh wrapper, NOT the codex pid.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess",
		PID:     paneSh,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name: map[int]string{peerPID: "tclaude", codexPID: "codex", paneSh: "sh"},
		parent: map[int]int{
			peerPID:  codexPID,
			codexPID: paneSh, // codex is the pane sh's direct child
			// paneSh's parent is unmapped → walk ends at init.
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor, "codex ancestor must be recognised")
	assert.Equal(t, convID, gotConv, "conv-id resolves via the codex ancestor's parent (pane sh) pid")

	// And the real authorization chokepoint accepts it as an agent — the
	// surface whoami / inbox gate on.
	p := &peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}
	assert.Equal(t, classAgent, classify(p))
}

// TestConvIDForPID_CodexAncestorResolvesViaOwnPID covers the variant where
// the sessions row is keyed by the harness pid itself — the pane shell
// exec'd into the harness (so pane_pid == the codex pid), or a hook
// corrected the row to FindClaudePID(). The direct
// FindSessionByPID(harness pid) lookup must still resolve it, so the fix
// works regardless of whether an sh wrapper survives between pane and codex.
func TestConvIDForPID_CodexAncestorResolvesViaOwnPID(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 7101
		codexPID = 7050
	)
	const convID = "22222222-3333-4444-5555-666666666666"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess2",
		PID:     codexPID,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", codexPID: "codex"},
		parent: map[int]int{peerPID: codexPID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv, "conv-id resolves via the harness pid directly")
}

// TestConvIDForPID_HarnessNameWithoutRecordedRowIsAgentUnknown is the
// un-forgeability guard, and the reason this fix resolves identity from the
// sessions table (host pids the daemon recorded at spawn) rather than from
// the harness process's own environment. A caller that merely *names* a
// process "codex" but whose pid AND parent pid map to no recorded row gets
// hasAncestor=true but NO conv-id → classAgentUnknown (403). It can never
// inherit another agent's identity by choosing a process name or planting an
// env var, because neither feeds the lookup — proven live: `cp tclaude
// /tmp/codex && TCLAUDE_SESSION_ID=<victim> /tmp/codex agent whoami` resolved
// to the victim under the env approach this replaces.
func TestConvIDForPID_HarnessNameWithoutRecordedRowIsAgentUnknown(t *testing.T) {
	setupTestDB(t)

	const (
		forgedPID = 8101 // an attacker-controlled process renamed "codex"
		shellPID  = 8050 // its real parent — no recorded session row
	)

	// A decoy row for a DIFFERENT session, keyed by a pid the attacker
	// cannot make its ancestor. It must never be returned.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "victim",
		PID:     9999,
		ConvID:  "victim-conv",
		Harness: "codex",
		Status:  "working",
	}))
	t.Setenv("TCLAUDE_SESSION_ID", "victim-conv")

	fakeProcTree{
		name:   map[int]string{forgedPID: "codex", shellPID: "sh"},
		parent: map[int]int{forgedPID: shellPID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(forgedPID)
	assert.True(t, hasAncestor, "a codex-named process counts as a harness ancestor")
	assert.Empty(t, gotConv, "but resolves to no conv-id without a recorded row")
	assert.Equal(t, classAgentUnknown,
		classify(&peer{PID: forgedPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}),
		"fail closed: a forged harness name never inherits another agent's identity")

	// Exercise a permission-gated endpoint with the same peer verdict. The
	// planted environment value must not promote the forged process to the
	// victim or bypass the classAgentUnknown denial.
	req := httptest.NewRequest(http.MethodPost, "/v1/whoami/task",
		bytes.NewBufferString(`{"url":"https://example.com/forged"}`))
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{
		PID: forgedPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}))
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "no resolvable conv-id")
}

func TestDaemonEndpoints_IgnorePlantedSessionEnvironment(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID   = 9101
		codexPID  = 9050
		paneSh    = 9040
		callerA   = "aaaaaaaa-2222-3333-4444-555555555555"
		plantedB  = "bbbbbbbb-2222-3333-4444-555555555555"
		taskURL   = "https://linear.app/example/issue/TCL-695"
		sessionID = "caller-a-session"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, PID: paneSh, ConvID: callerA, Harness: "codex", Status: "working",
	}))
	callerAgentID, _, err := db.EnsureAgentForConv(callerA, "test")
	require.NoError(t, err)
	plantedAgentID, _, err := db.EnsureAgentForConv(plantedB, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentPermissionOverride(
		callerA, PermSelfTask, db.PermEffectGrant, "test"))
	t.Setenv("TCLAUDE_SESSION_ID", plantedB)

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", codexPID: "codex", paneSh: "sh"},
		parent: map[int]int{peerPID: codexPID, codexPID: paneSh},
	}.install(t)
	gotConv, hasAncestor := convIDForPID(peerPID)
	require.True(t, hasAncestor)
	require.Equal(t, callerA, gotConv)
	resolvedPeer := &peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}

	serve := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, resolvedPeer))
		rec := httptest.NewRecorder()
		buildMux().ServeHTTP(rec, req)
		return rec
	}

	whoami := serve(http.MethodGet, "/v1/whoami", "")
	require.Equal(t, http.StatusOK, whoami.Code, whoami.Body.String())
	var identity whoamiResp
	require.NoError(t, json.Unmarshal(whoami.Body.Bytes(), &identity))
	assert.Equal(t, callerA, identity.ConvID,
		"peer-recorded caller A wins over planted environment caller B")

	task := serve(http.MethodPost, "/v1/whoami/task", `{"url":"`+taskURL+`"}`)
	require.Equal(t, http.StatusOK, task.Code, task.Body.String())
	callerRef, err := db.GetAgentTaskRef(callerAgentID)
	require.NoError(t, err)
	assert.Equal(t, taskURL, callerRef.URL)
	plantedRef, err := db.GetAgentTaskRef(plantedAgentID)
	require.NoError(t, err)
	assert.Empty(t, plantedRef.URL,
		"self-scoped permission-gated write must not target the planted environment identity")
}

// TestConvIDForPID_ClaudeAncestorViaSessionFile guards that the Claude
// Code path is untouched by the harness-agnostic match: a "claude"
// ancestor still resolves its conv-id from ~/.claude/sessions/<pid>.json
// without ever consulting the DB.
func TestConvIDForPID_ClaudeAncestorViaSessionFile(t *testing.T) {
	setupTestDB(t) // sets HOME to a temp dir

	const (
		peerPID   = 5201
		claudePID = 5100
	)
	const convID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	sessDir := filepath.Join(home, ".claude", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "5100.json"),
		[]byte(`{"sessionId":"`+convID+`"}`), 0o644))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", claudePID: "claude"},
		parent: map[int]int{peerPID: claudePID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv, "conv-id resolved from the per-pid session file")
}

// TestConvIDForPID_NoHarnessAncestorStaysHuman guards the warning in the
// handoff: recognising codex must NOT reclassify a real human. A peer with
// a plain (sshd/bash) ancestor chain reports no harness ancestor, so
// classify routes it through the operator-token branch — human with a
// valid token, unconfirmed without — exactly as before.
func TestConvIDForPID_NoHarnessAncestorStaysHuman(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID = 6301
		bashPID = 6200
		sshPID  = 6100
	)
	fakeProcTree{
		name: map[int]string{peerPID: "tclaude", bashPID: "bash", sshPID: "sshd"},
		parent: map[int]int{
			peerPID: bashPID,
			bashPID: sshPID,
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.False(t, hasAncestor, "no harness ancestor in a plain shell chain")
	assert.Empty(t, gotConv)

	assert.Equal(t, classHuman,
		classify(&peer{PID: peerPID, HasClaudeAncestor: false, HumanTokenValid: true}),
		"a token-bearing human is still the human")
	assert.Equal(t, classUnconfirmed,
		classify(&peer{PID: peerPID, HasClaudeAncestor: false}),
		"no ancestor and no token is still unconfirmed, not an agent")
}

// TestConvIDForPID_FollowsRowConvAcrossClear documents the /clear case. A
// /clear rotates the agent's conv-id but keeps the SAME process and pane, so
// the hook callback updates the SAME session row in place — located by the
// stable TCLAUDE_SESSION_ID label, conv-id advanced + identity migrated
// (issue #192), pid unchanged (the sh pane). Because the resolver reads the
// row's CURRENT conv-id (it caches nothing), it follows the rotation: a
// /clear'd agent resolves to its new conversation, never the retired one.
func TestConvIDForPID_FollowsRowConvAcrossClear(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4201
		codexPID = 4150
		paneSh   = 4140 // the pane sh pid the row stays keyed by across /clear
	)

	// Pre-/clear: the spawn row keyed by the pane sh pid, on conv A.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "clearsess", PID: paneSh, ConvID: "conv-A", Harness: "codex", Status: "working",
	}))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", codexPID: "codex", paneSh: "sh"},
		parent: map[int]int{peerPID: codexPID, codexPID: paneSh},
	}.install(t)

	got, ok := convIDForPID(peerPID)
	assert.True(t, ok)
	assert.Equal(t, "conv-A", got, "before /clear: the original conv")

	// /clear advances the SAME row's conv-id in place (id and pane pid
	// unchanged; SaveSession upserts on the primary key).
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "clearsess", PID: paneSh, ConvID: "conv-B", Harness: "codex", Status: "working",
	}))

	got, ok = convIDForPID(peerPID)
	assert.True(t, ok)
	assert.Equal(t, "conv-B", got, "after /clear: follows the row to the new conv, not the stale one")
}

// TestConvIDForPID_ReincarnationNewPaneIgnoresStaleRow documents the
// reincarnation case. Reincarnation spawns a FRESH pane (a new pane_pid) for
// the successor conv while the predecessor's row may linger. The walk from
// the new harness reaches only the new pane's pids, so it resolves the
// successor conv — a stale predecessor row, keyed by a pane pid the new
// process tree never touches, cannot bleed in.
func TestConvIDForPID_ReincarnationNewPaneIgnoresStaleRow(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID   = 5301
		newCodex  = 5250
		newPaneSh = 5240
		oldPaneSh = 5140 // predecessor pane — not in the successor's tree
	)

	// Stale predecessor row, still keyed by the old pane pid.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "old", PID: oldPaneSh, ConvID: "conv-old", Harness: "codex", Status: "exited",
	}))
	// Fresh reincarnated row keyed by the new pane pid, carrying the
	// migrated identity's new conv-id.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "new", PID: newPaneSh, ConvID: "conv-new", Harness: "codex", Status: "working",
	}))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", newCodex: "codex", newPaneSh: "sh"},
		parent: map[int]int{peerPID: newCodex, newCodex: newPaneSh},
	}.install(t)

	got, ok := convIDForPID(peerPID)
	assert.True(t, ok)
	assert.Equal(t, "conv-new", got,
		"resolves the successor via the new pane pid; the stale predecessor row is unreachable")
}
