package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dirInfo mirrors the daemon's dirResp wire shape.
type dirInfo struct {
	ConvID      string `json:"conv_id"`
	StartDir    string `json:"start_dir"`
	CurrentDir  string `json:"current_dir"`
	WorktreeDir string `json:"worktree_dir"`
	Source      string `json:"source"`
	CallerConv  string `json:"caller_conv"`
}

// Scenario: an agent launched in ~/git has since been editing files
// deep inside a repo. The PostToolUse hook recorded the edit dir into
// agent_workdir.
//
// Expected: `dir` reports the launch dir as start_dir and the recorded
// edit dir as current_dir, on both the self route (/v1/whoami/dir) and
// the cross-agent route (/v1/agent/{sel}/dir). source == "hook".
func TestDir_ReportsStartAndCurrent(t *testing.T) {
	f := newFlow(t)

	const conv = "dir1-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"
	const currentDir = "/home/u/git/repo/pkg/foo"

	f.HaveConvWithTitle(conv, "builder")
	f.HaveAliveSession(conv, "lbl-dir1", "tclaude-dir1", startDir)
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir), "seed workdir")

	// Self route: the calling agent asks about itself.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var self dirInfo
	testharness.DecodeJSON(t, rec, &self)
	assert.Equal(t, startDir, self.StartDir, "start_dir")
	assert.Equal(t, currentDir, self.CurrentDir, "current_dir")
	assert.Equal(t, "hook", self.Source, "source")

	// Cross-agent route: the human asks about the agent by conv-id.
	rec = testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+conv+"/dir", nil)))
	require.Equal(t, http.StatusOK, rec.Code, "agent/dir: body=%s", rec.Body.String())
	var other dirInfo
	testharness.DecodeJSON(t, rec, &other)
	assert.Equal(t, startDir, other.StartDir, "start_dir (cross-agent)")
	assert.Equal(t, currentDir, other.CurrentDir, "current_dir (cross-agent)")
}

// Scenario: a fresh agent that has not edited any files yet — the
// PostToolUse hook never recorded a workdir.
//
// Expected: current_dir falls back to the launch dir, and source says
// "fallback" so callers can tell the difference.
func TestDir_FallsBackToStartWhenNoEdit(t *testing.T) {
	f := newFlow(t)

	const conv = "dir2-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git/justlaunched"

	f.HaveConvWithTitle(conv, "fresh")
	f.HaveAliveSession(conv, "lbl-dir2", "tclaude-dir2", startDir)

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var info dirInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, startDir, info.StartDir, "start_dir")
	assert.Equal(t, startDir, info.CurrentDir, "current_dir falls back to start")
	assert.Equal(t, "fallback", info.Source, "source")
}

// Scenario: an agent asks the daemon to open a terminal in its working
// directory. The daemon spawns the window out-of-sandbox.
//
// Expected: openTerminal is invoked with a `cd <dir> && exec ...`
// payload — current_dir by default, start_dir with which="start".
func TestDir_OpenSpawnsTerminalInDir(t *testing.T) {
	f := newFlow(t)

	const conv = "dir3-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"
	const currentDir = "/home/u/git/repo/pkg/bar"

	f.HaveConvWithTitle(conv, "opener")
	f.HaveAliveSession(conv, "lbl-dir3", "tclaude-dir3", startDir)
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir), "seed workdir")

	var gotCmd string
	t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
		gotCmd = cmd
		return nil
	}))

	// Default which == current.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/dir",
			map[string]string{"which": "current"}), conv))
	require.Equal(t, http.StatusOK, rec.Code, "open current: body=%s", rec.Body.String())
	assert.Contains(t, gotCmd, currentDir, "terminal command should cd into current_dir")

	// which == start opens the launch dir instead.
	gotCmd = ""
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/dir",
			map[string]string{"which": "start"}), conv))
	require.Equal(t, http.StatusOK, rec.Code, "open start: body=%s", rec.Body.String())
	assert.Contains(t, gotCmd, startDir, "terminal command should cd into start_dir")
	assert.False(t, strings.Contains(gotCmd, currentDir),
		"start open must not target current_dir")
}

// Scenario: the human clicks the dashboard's "term" button.
//
// Expected: POST /api/term/{conv} resolves the agent, opens a terminal
// in the requested dir, and 200s.
func TestDir_DashboardTermButton(t *testing.T) {
	f := newFlow(t)

	const conv = "dir4-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"
	const currentDir = "/home/u/git/repo/cmd"

	f.HaveConvWithTitle(conv, "dash-term")
	f.HaveAliveSession(conv, "lbl-dir4", "tclaude-dir4", startDir)
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir), "seed workdir")

	var gotCmd string
	t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
		gotCmd = cmd
		return nil
	}))

	// The dashboard auth check pins Origin to popupBaseURL; the test
	// handler only injects the Origin header when that URL is set.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, testharness.JSONRequest(t,
		http.MethodPost, "/api/term/"+conv, map[string]string{"which": "current"}))
	require.Equal(t, http.StatusOK, rec.Code, "term button: body=%s", rec.Body.String())
	assert.Contains(t, gotCmd, currentDir, "dashboard term should cd into current_dir")
}

// Scenario: an agent is editing files deep inside a git worktree.
//
// Expected: worktree_dir is the git working-tree root containing
// current_dir, and `which=worktree` opens a terminal there.
func TestDir_WorktreeDir(t *testing.T) {
	f := newFlow(t)

	const conv = "dir5-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"
	const currentDir = "/home/u/git/repo/pkg/deep/nested"
	const worktreeRoot = "/home/u/git/repo"

	f.HaveConvWithTitle(conv, "wt")
	f.HaveAliveSession(conv, "lbl-dir5", "tclaude-dir5", startDir)
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir), "seed workdir")

	// Stub the git resolver: current_dir resolves to the worktree root.
	t.Cleanup(agentd.SetGitToplevelForTest(func(dir string) (string, bool) {
		if dir == currentDir {
			return worktreeRoot, true
		}
		return "", false
	}))

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var info dirInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, worktreeRoot, info.WorktreeDir, "worktree_dir")

	var gotCmd string
	t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
		gotCmd = cmd
		return nil
	}))
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/dir",
			map[string]string{"which": "worktree"}), conv))
	require.Equal(t, http.StatusOK, rec.Code, "open worktree: body=%s", rec.Body.String())
	assert.Contains(t, gotCmd, worktreeRoot, "terminal command should cd into the worktree root")
}

// Scenario: the agent's current dir is not inside any git repo.
//
// Expected: worktree_dir falls back to the launch dir.
func TestDir_WorktreeFallsBackToStart(t *testing.T) {
	f := newFlow(t)

	const conv = "dir6-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/notgit"

	f.HaveConvWithTitle(conv, "nowt")
	f.HaveAliveSession(conv, "lbl-dir6", "tclaude-dir6", startDir)

	t.Cleanup(agentd.SetGitToplevelForTest(func(string) (string, bool) {
		return "", false
	}))

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var info dirInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, startDir, info.WorktreeDir, "worktree_dir falls back to start")
}
