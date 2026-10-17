package agentd_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type backgroundTerminalRow struct {
	Kind        string `json:"kind"`
	TmuxSession string `json:"tmux_session"`
	Key         string `json:"key"`
	Label       string `json:"label"`
	AgentID     string `json:"agent_id"`
	ConvID      string `json:"conv_id"`
	Which       string `json:"which"`
	Superseded  bool   `json:"superseded"`
	Killable    bool   `json:"killable"`
}

func fetchBackgroundTerminals(t *testing.T, mux http.Handler) []backgroundTerminalRow {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/background-terminals", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Terminals []backgroundTerminalRow `json:"terminals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Terminals
}

// Scenario: the Terminals tab lists tclaude tmux sessions nobody is attached
// to — an agent, a plain session, an agent's web shell, and an unrecognised
// session — each classified with the pane key its attach opens under. A
// session with a client attached (an open web terminal or native window) is
// not in the background and is left out.
func TestBackgroundTerminals_ListsUnattachedSessions(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const agentConv = "bgta-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(agentConv, "idle-worker")
	f.HaveAliveSession(agentConv, "spwn-bgta", "tmux-bgta", f.TestCwd("bgta"))
	f.HaveEnrolledAgent(agentConv)
	agentID, err := db.AgentIDForConv(agentConv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	const plainConv = "bgtb-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(plainConv, "quick question")
	f.HaveAliveSession(plainConv, "spwn-bgtb", "tmux-bgtb", f.TestCwd("bgtb"))

	const watchedConv = "bgtc-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(watchedConv, "watched")
	f.HaveAliveSession(watchedConv, "spwn-bgtc", "tmux-bgtc", f.TestCwd("bgtc"))
	f.World.Tmux.AttachClient("/dev/pts/9", "tmux-bgtc")

	sum := sha256.Sum256([]byte(agentConv))
	shell := fmt.Sprintf("tclaude-term-%x-current", sum[:8])
	f.World.Tmux.MarkAlive(shell)
	f.World.Tmux.MarkAlive("someone-elses-session")
	f.World.Tmux.MarkAlive("__tclaude-opencode-abc123")

	// A shell opened under an earlier generation of a since-reincarnated
	// agent is listed but not attachable: every attach endpoint follows the
	// agent (even by its old conversation id) to the successor's shell.
	const oldConv = "bgtd-1111-2222-3333-444444444444"
	const newConv = "bgte-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(oldConv, "rotated")
	f.HaveEnrolledAgent(oldConv)
	f.HaveConvWithTitle(newConv, "rotated")
	_, err = db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)
	resolved, _, err := agent.ResolveSelector(oldConv)
	require.NoError(t, err)
	require.Equal(t, newConv, resolved.ConvID, "attach selectors follow the succession chain")
	oldSum := sha256.Sum256([]byte(oldConv))
	staleShell := fmt.Sprintf("tclaude-term-%x-start", oldSum[:8])
	f.World.Tmux.MarkAlive(staleShell)

	rows := fetchBackgroundTerminals(t, mux)
	byTmux := map[string]backgroundTerminalRow{}
	for _, row := range rows {
		byTmux[row.TmuxSession] = row
	}
	assert.NotContains(t, byTmux, "tmux-bgtc", "an attached session is not in the background")

	require.Contains(t, byTmux, "tmux-bgta")
	assert.Equal(t, backgroundTerminalRow{
		Kind: "agent", TmuxSession: "tmux-bgta", Key: "window:" + agentID,
		Label: "idle-worker", AgentID: agentID, ConvID: agentConv,
	}, byTmux["tmux-bgta"])

	require.Contains(t, byTmux, "tmux-bgtb")
	assert.Equal(t, "session", byTmux["tmux-bgtb"].Kind)
	assert.Equal(t, "window:"+plainConv, byTmux["tmux-bgtb"].Key)

	require.Contains(t, byTmux, shell)
	assert.Equal(t, "shell", byTmux[shell].Kind)
	assert.Equal(t, "current", byTmux[shell].Which)
	assert.Equal(t, "term:"+agentID+":current:", byTmux[shell].Key)
	assert.Equal(t, "idle-worker", byTmux[shell].Label)

	require.Contains(t, byTmux, "someone-elses-session")
	assert.Equal(t, "other", byTmux["someone-elses-session"].Kind)
	assert.Empty(t, byTmux["someone-elses-session"].Key, "an unrecognised session has no dashboard attach")

	assert.NotContains(t, byTmux, "__tclaude-opencode-abc123", "tclaude's own plumbing is not a background terminal")

	require.Contains(t, byTmux, staleShell)
	assert.Equal(t, "shell", byTmux[staleShell].Kind)
	assert.True(t, byTmux[staleShell].Superseded)
	assert.Empty(t, byTmux[staleShell].AgentID)
	assert.Empty(t, byTmux[staleShell].Key, "no dashboard attach would reach the listed shell")

	assert.Equal(t, "agent", rows[0].Kind, "agents sort first")
	assert.True(t, byTmux[shell].Killable)
	assert.True(t, byTmux[staleShell].Killable)
	assert.True(t, byTmux["someone-elses-session"].Killable)
	assert.False(t, byTmux["tmux-bgta"].Killable, "a live agent is shut down, not killed")
	assert.False(t, byTmux["tmux-bgtb"].Killable, "a live session is shut down, not killed")
}

func killBackgroundTerminal(t *testing.T, mux http.Handler, name string) int {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/background-terminals/kill",
		map[string]string{"tmux_session": name}))
	return rec.Code
}

func agentShellName(conv, which string) string {
	sum := sha256.Sum256([]byte(conv))
	return fmt.Sprintf("tclaude-term-%x-%s", sum[:8], which)
}

// Scenario: the operator kills a forgotten web shell from the background
// popover. A live agent is refused (it has shutdown), and so is a shell that
// somebody attached to after the popover was drawn.
func TestBackgroundTerminals_KillEndsShellButNotLiveAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "bgtk-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveAliveSession(conv, "spwn-bgtk", "tmux-bgtk", f.TestCwd("bgtk"))
	f.HaveEnrolledAgent(conv)
	shell := agentShellName(conv, "worktree")
	f.World.Tmux.MarkAlive(shell)
	watched := agentShellName(conv, "start")
	f.World.Tmux.MarkAlive(watched)
	f.World.Tmux.AttachClient("/dev/pts/7", watched)

	assert.Equal(t, http.StatusConflict, killBackgroundTerminal(t, mux, "tmux-bgtk"))
	assert.True(t, f.World.Tmux.IsAlive("tmux-bgtk"))
	assert.Equal(t, http.StatusNotFound, killBackgroundTerminal(t, mux, watched))
	assert.True(t, f.World.Tmux.IsAlive(watched))

	assert.Equal(t, http.StatusOK, killBackgroundTerminal(t, mux, shell))
	assert.False(t, f.World.Tmux.IsAlive(shell))
	for _, row := range fetchBackgroundTerminals(t, mux) {
		assert.NotEqual(t, shell, row.TmuxSession)
	}
}

// Scenario: an agent's web shell outlives its use — the agent is deleted, or
// the worktree the shell was opened in is removed. The reaper kills those
// shells; a shell whose owner and directory still exist stays, and so does an
// orphan somebody is attached to.
func TestBackgroundTerminals_ReaperKillsOrphanedShells(t *testing.T) {
	f := newFlow(t)

	const kept = "bgtr-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(kept, "kept")
	f.HaveEnrolledAgent(kept)
	keptDir := f.TestCwd("bgtr-kept")
	require.NoError(t, os.MkdirAll(keptDir, 0o755))
	keptShell := agentShellName(kept, "current")
	f.World.Tmux.MarkAliveIn(keptShell, keptDir)

	// The worktree the shell was opened in has been removed.
	goneShell := agentShellName(kept, "worktree")
	f.World.Tmux.MarkAliveIn(goneShell, f.TestCwd("bgtr-removed-worktree"))

	// The agent the shell belonged to was deleted.
	const deleted = "bgts-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(deleted, "deleted")
	f.HaveEnrolledAgent(deleted)
	_, err := db.DeleteAgentByConvID(deleted)
	require.NoError(t, err)
	ownerlessShell := agentShellName(deleted, "current")
	f.World.Tmux.MarkAliveIn(ownerlessShell, keptDir)
	watchedShell := agentShellName(deleted, "start")
	f.World.Tmux.MarkAliveIn(watchedShell, keptDir)
	f.World.Tmux.AttachClient("/dev/pts/8", watchedShell)

	agentd.RunReaperTickForTest(time.Now())

	assert.True(t, f.World.Tmux.IsAlive(keptShell), "owner and directory still exist")
	assert.False(t, f.World.Tmux.IsAlive(goneShell), "its directory was removed")
	assert.False(t, f.World.Tmux.IsAlive(ownerlessShell), "its agent was deleted")
	assert.True(t, f.World.Tmux.IsAlive(watchedShell), "somebody is attached to it")
}
