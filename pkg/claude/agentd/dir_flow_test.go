package agentd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	ConvID        string `json:"conv_id"`
	StartDir      string `json:"start_dir"`
	StartBranch   string `json:"start_branch"`
	CurrentDir    string `json:"current_dir"`
	WorktreeDir   string `json:"worktree_dir"`
	CurrentBranch string `json:"current_branch"`
	Source        string `json:"source"`
	CallerConv    string `json:"caller_conv"`
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
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir, "", ""), "seed workdir")

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
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir, "", ""), "seed workdir")

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
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir, "", ""), "seed workdir")

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

// Scenario: the human clicks the per-row "open window" cog item.
//
// Expected: POST /api/open-window/{conv} resolves the agent and opens a
// terminal ATTACHED to its live session (`tclaude session attach
// <label>`), 200.
func TestDir_DashboardOpenWindowButton(t *testing.T) {
	f := newFlow(t)

	const conv = "dirow-aaaa-bbbb-cccc-dddd"
	const label = "lbl-dirow"
	const startDir = "/home/u/git"

	f.HaveConvWithTitle(conv, "dash-open-window")
	f.HaveAliveSession(conv, label, "tclaude-dirow", startDir)

	var gotCmd string
	t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
		gotCmd = cmd
		return nil
	}))

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, testharness.JSONRequest(t,
		http.MethodPost, "/api/open-window/"+conv, nil))
	require.Equal(t, http.StatusOK, rec.Code, "open-window: body=%s", rec.Body.String())
	assert.Contains(t, gotCmd, "session attach", "open-window should attach to the live session")
	assert.Contains(t, gotCmd, label, "open-window should attach by the session label")
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
	const worktreeBranch = "feature-x"

	f.HaveConvWithTitle(conv, "wt")
	f.HaveAliveSession(conv, "lbl-dir5", "tclaude-dir5", startDir)
	// The PostToolUse hook records the edit dir together with its git
	// worktree root + branch; seed all three, as a real edit would.
	require.NoError(t, db.UpsertAgentWorkdir(conv, currentDir, worktreeRoot, worktreeBranch),
		"seed workdir")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var info dirInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, worktreeRoot, info.WorktreeDir, "worktree_dir")
	assert.Equal(t, worktreeBranch, info.CurrentBranch, "current_branch")

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

// Scenario: one agent tries to open a terminal targeting a different
// agent via the cross-agent route.
//
// Expected: 403 — spawning a window on the human's desktop for someone
// else is human-only. The human (no agent identity) is allowed.
func TestDir_OpenForAnotherAgentIsHumanOnly(t *testing.T) {
	f := newFlow(t)

	const caller = "dirc-aaaa-bbbb-cccc-dddd"
	const target = "dirt-aaaa-bbbb-cccc-dddd"

	f.HaveConvWithTitle(target, "victim")
	f.HaveAliveSession(target, "lbl-dirt", "tclaude-dirt", "/home/u/git")
	require.NoError(t, db.UpsertAgentWorkdir(target, "/home/u/git/repo", "", ""), "seed workdir")

	opened := false
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opened = true
		return nil
	}))

	// An agent must not open a terminal for a different agent.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+target+"/dir",
			map[string]string{"which": "current"}), caller))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"cross-agent open should be 403; body=%s", rec.Body.String())
	assert.False(t, opened, "no terminal should have been spawned")

	// The human is allowed — it's their desktop.
	rec = testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+target+"/dir",
			map[string]string{"which": "current"})))
	assert.Equal(t, http.StatusOK, rec.Code,
		"human cross-agent open should be allowed; body=%s", rec.Body.String())
	assert.True(t, opened, "human open should have spawned a terminal")
}

// Scenario: a POST arrives with a malformed JSON body.
//
// Expected: 400, and no terminal spawned — bad input must not fall
// through to the default directory.
func TestDir_OpenRejectsMalformedJSON(t *testing.T) {
	f := newFlow(t)

	const conv = "dirm-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(conv, "badjson")
	f.HaveAliveSession(conv, "lbl-dirm", "tclaude-dirm", "/home/u/git")

	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		t.Error("openTerminal must not run on malformed input")
		return nil
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/whoami/dir", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"malformed body should be 400; body=%s", rec.Body.String())
}

// Scenario: a fresh agent that has not edited any files yet — no
// agent_workdir row, so there's nothing to resolve a worktree from.
//
// Expected: worktree_dir falls back to the launch dir.
func TestDir_WorktreeFallsBackToStart(t *testing.T) {
	f := newFlow(t)

	const conv = "dir6-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/notgit"

	f.HaveConvWithTitle(conv, "nowt")
	f.HaveAliveSession(conv, "lbl-dir6", "tclaude-dir6", startDir)

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/dir", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/dir: body=%s", rec.Body.String())
	var info dirInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, startDir, info.WorktreeDir, "worktree_dir falls back to start")
}

// Scenario: openTerminal can't pop a native window — no display, no
// terminal emulator installed, whatever the reason.
//
// Expected: POST /api/term/{conv} degrades to the in-browser terminal
// fallback (200, mode:"browser", a ws path the dashboard can open
// modal-term.js against) instead of failing outright.
func TestDir_DashboardTermButtonFallsBackToBrowser(t *testing.T) {
	f := newFlow(t)

	const conv = "dirfb-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"

	f.HaveConvWithTitle(conv, "dash-term-fallback")
	f.HaveAliveSession(conv, "lbl-dirfb", "tclaude-dirfb", startDir)

	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("no graphical display: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(dash, testharness.JSONRequest(t,
		http.MethodPost, "/api/term/"+conv, map[string]string{"which": "start"}))
	require.Equal(t, http.StatusOK, rec.Code, "term fallback: body=%s", rec.Body.String())
	var info struct {
		Dir   string `json:"dir"`
		Which string `json:"which"`
		Mode  string `json:"mode"`
		WS    string `json:"ws"`
	}
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, "browser", info.Mode, "no native window available, so mode must be browser")
	assert.Equal(t, startDir, info.Dir)
	assert.Equal(t, "/api/term-ws/"+conv+"?which=start", info.WS,
		"ws path must target the same conv + which the request asked for")
}

// Scenario: the human clicks the dashboard's dedicated "web term"
// button (body web:true) on a host that CAN pop a native window.
//
// Expected: POST /api/term/{conv} skips the native window entirely
// (openTerminal is never called) and reports mode:"browser" with a
// term-ws path, so the dashboard always streams the in-browser PTY.
func TestDir_DashboardWebTermButtonForcesBrowser(t *testing.T) {
	f := newFlow(t)

	const conv = "dirwt-aaaa-bbbb-cccc-dddd"
	const startDir = "/home/u/git"

	f.HaveConvWithTitle(conv, "dash-web-term")
	f.HaveAliveSession(conv, "lbl-dirwt", "tclaude-dirwt", startDir)

	// A native open WOULD succeed here — proving web:true bypasses it
	// rather than relying on the no-display fallback.
	nativeOpened := false
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		nativeOpened = true
		return nil
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(dash, testharness.JSONRequest(t,
		http.MethodPost, "/api/term/"+conv, map[string]any{"which": "start", "web": true}))
	require.Equal(t, http.StatusOK, rec.Code, "web term: body=%s", rec.Body.String())
	var info struct {
		Dir   string `json:"dir"`
		Which string `json:"which"`
		Mode  string `json:"mode"`
		WS    string `json:"ws"`
	}
	testharness.DecodeJSON(t, rec, &info)
	assert.False(t, nativeOpened, "web:true must never attempt a native window")
	assert.Equal(t, "browser", info.Mode, "web:true must always report mode:browser")
	assert.Equal(t, startDir, info.Dir)
	assert.Equal(t, "/api/term-ws/"+conv+"?which=start", info.WS,
		"ws path must target the same conv + which the request asked for")
}

// Scenario: same as above but for the "open window" (attach to live
// session) action.
//
// Expected: POST /api/open-window/{conv} degrades to the in-browser
// fallback attached to the same live session, instead of failing.
func TestDir_DashboardOpenWindowButtonFallsBackToBrowser(t *testing.T) {
	f := newFlow(t)

	const conv = "dirowfb-aaaa-bbbb-cccc-dddd"
	const label = "lbl-dirowfb"
	const startDir = "/home/u/git"

	f.HaveConvWithTitle(conv, "dash-open-window-fallback")
	f.HaveAliveSession(conv, label, "tclaude-dirowfb", startDir)

	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("no graphical display: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(dash, testharness.JSONRequest(t,
		http.MethodPost, "/api/open-window/"+conv, nil))
	require.Equal(t, http.StatusOK, rec.Code, "open-window fallback: body=%s", rec.Body.String())
	var info struct {
		ConvID string `json:"conv_id"`
		Label  string `json:"label"`
		Mode   string `json:"mode"`
		WS     string `json:"ws"`
	}
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, "browser", info.Mode, "no native window available, so mode must be browser")
	assert.Equal(t, label, info.Label)
	assert.Equal(t, "/api/open-window-ws/"+conv, info.WS)
}

// Scenario: the new term/open-window WebSocket upgrade routes carry
// the same human-consent threat model as every other dashboard /api/*
// route.
//
// Expected: a request with no dashboard session cookie is refused
// before ever reaching websocket.Upgrade.
func TestDir_TermWSRoutesRequireDashboardAuth(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec := testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/api/term-ws/whatever", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"term-ws without a session cookie must be refused; body=%s", rec.Body.String())

	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/api/open-window-ws/whatever", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"open-window-ws without a session cookie must be refused; body=%s", rec.Body.String())
}
