package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// whoamiAsCaller drives the real /v1/whoami — the endpoint behind `tclaude
// agent whoami` — as a caller at pid, whose identity the daemon resolves the
// production way: by walking the process tree from the socket peer's pid. It
// returns the whole answer rather than just the title because the failure
// this file is about does not produce a wrong title, it produces no identity:
// an unidentifiable caller gets an empty body, which the CLI renders as
// "(unnamed)".
func whoamiAsCaller(t *testing.T, f *testharness.Flow, pid int) (convID, title string) {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsResolvedPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/whoami", nil), pid))
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/whoami body=%s", rec.Body.String())
	var resp struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
		"decode whoami body=%s", rec.Body.String())
	require.False(t, resp.IsHuman, "an agent's caller must never classify as the human")
	return resp.ConvID, resp.Title
}

// recordPanePID stamps the tmux pane pid onto a spawned session's row — the
// host fact identity resolution keys a caller against. Production reads it
// from tmux (ParsePIDFromTmux) at `tclaude session new`, which is the
// subprocess boundary flow tests swap, so the row arrives here without one;
// the same stamp is what the hook-broker flow tests do.
func recordPanePID(t *testing.T, label string, panePID int) {
	t.Helper()
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	require.NotNil(t, row, "session row %s should exist", label)
	row.PID = panePID
	require.NoError(t, db.SaveSession(row))
}

// TestCopilotSpawn_WhoamiNamesTheAgentFromInsideItsPane is TCL-1049 at the
// surface the operator reported it on: a Copilot agent spawned with a name
// shows that name in the dashboard but answers "(unnamed)" about itself.
//
// The spawn is the production one; what the test supplies is the pane's
// process tree, in the shape measured on the real 1.0.78 CLI — a `copilot`
// binary and its npm loader that both report a RENAMED MAIN THREAD as their
// process name ("MainThread" / "node-MainThread") while their executables
// name them correctly. Before the fix the daemon matched ancestors by name
// only, so this caller had no harness ancestor at all: whoami returned an
// empty body (rendered "(unnamed)") and every other agent command was a 403.
func TestCopilotSpawn_WhoamiNamesTheAgentFromInsideItsPane(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, _ := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir": true,
		"name":      "copilot-worker",
	})
	// tclaude agent whoami -> the shell Copilot runs tools in -> the copilot
	// binary -> the npm loader -> the pane the daemon recorded.
	const (
		peerPID = 5501
		toolSh  = 5502
		sea     = 5503
		loader  = 5504
		paneSh  = 5505
	)
	recordPanePID(t, resp.Label, paneSh)
	t.Cleanup(agentd.SetProcTreeWithExeForTest(
		map[int]string{
			peerPID: "tclaude", toolSh: "bash",
			sea: "MainThread", loader: "node-MainThread", paneSh: "sh",
		},
		map[int]string{sea: harness.CopilotName, loader: "node"},
		map[int]int{peerPID: toolSh, toolSh: sea, sea: loader, loader: paneSh},
	))

	convID, title := whoamiAsCaller(t, f, peerPID)
	assert.Equal(t, resp.ConvID, convID,
		"the pane's caller must be identified as the agent that was spawned there")
	assert.Equal(t, "copilot-worker", title,
		"whoami must report the spawn-time name, not the (unnamed) placeholder")
}

// Parity: a harness whose process name already matches — Claude Code runs as
// node — resolves through the unchanged name branch, with no executable
// evidence available for any pid in its tree. This is the arm that would fail
// if the fix had replaced the name test instead of extending it.
func TestClaudeSpawn_WhoamiNamesTheAgentFromInsideItsPane(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"trust_dir": true,
		"name":      "claude-worker",
	})
	require.Equalf(t, http.StatusOK, resp.Code, "claude spawn body=%s", resp.Raw)

	const (
		peerPID = 5601
		toolSh  = 5602
		node    = 5603
		paneSh  = 5604
	)
	recordPanePID(t, resp.Label, paneSh)
	t.Cleanup(agentd.SetProcTreeWithExeForTest(
		map[int]string{peerPID: "tclaude", toolSh: "bash", node: "node", paneSh: "sh"},
		nil,
		map[int]int{peerPID: toolSh, toolSh: node, node: paneSh},
	))

	convID, title := whoamiAsCaller(t, f, peerPID)
	assert.Equal(t, resp.ConvID, convID)
	assert.Equal(t, "claude-worker", title)
}
