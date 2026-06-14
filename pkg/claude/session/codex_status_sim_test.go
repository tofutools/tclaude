package session_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A Codex session's live status is driven by the SAME hook callback Claude
// Code uses: Codex delivers a field-identical payload for the subset of
// events it fires, so the shared event→status switch maps them unchanged.
// This walks one turn through the production ApplyHook and asserts the
// persisted status the read surfaces (session ls / watch / dashboard) show
// — in particular that a Codex PermissionRequest surfaces as
// needs-attention (StatusAwaitingPermission), Codex's stand-in for the
// Notification(permission_prompt) event it does not have. External package
// so it can drive the CodexSim without the testharness → session cycle.
func TestApplyHook_CodexLiveStatusPipeline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	const convID = "019ec004-4250-79b1-9ade-ebaea4159001"
	const sessionID = "agent-codex-status"

	// A Codex session tclaude spawned: its row is tagged harness=codex.
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusIdle,
		Harness: "codex",
		Cwd:     "/home/u/proj",
	}))

	// The sim owns a faithful rollout for this conv (not read by the
	// status path, but it makes the row correspond to a real session).
	cx := testharness.NewCodexSimWithID(t, dir, convID, "/home/u/proj")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("do the thing"))

	apply := func(event, tool string) {
		t.Helper()
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: event,
			ConvID:        convID,
			Cwd:           "/home/u/proj",
			ToolName:      tool,
		}, sessionID))
	}
	statusNow := func() *session.SessionState {
		t.Helper()
		st, err := session.LoadSessionState(sessionID)
		require.NoError(t, err)
		return st
	}

	// Prompt submitted → working.
	apply("UserPromptSubmit", "")
	assert.Equal(t, session.StatusWorking, statusNow().Status, "UserPromptSubmit → working")

	// A tool is about to run → still working.
	apply("PreToolUse", "Bash")
	assert.Equal(t, session.StatusWorking, statusNow().Status, "PreToolUse → working")

	// Codex asks the human to approve a command. With no Notification
	// event, PermissionRequest IS the needs-attention signal.
	apply("PermissionRequest", "Bash")
	attn := statusNow()
	assert.Equal(t, session.StatusAwaitingPermission, attn.Status,
		"Codex PermissionRequest → awaiting_permission (needs-attention)")

	// Turn ends → idle (Codex has no SessionEnd; Stop is the turn boundary).
	apply("Stop", "")
	done := statusNow()
	assert.Equal(t, session.StatusIdle, done.Status, "Stop → idle")

	// The harness tag survives the read-modify-write of every hook.
	assert.Equal(t, "codex", attn.Harness, "harness preserved through PermissionRequest")
	assert.Equal(t, "codex", done.Harness, "harness preserved through Stop")
}

func TestApplyHook_CodexPublishesWorkspaceBranch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "tclaude-test@example.invalid")
	runGit(t, repo, "config", "user.name", "tclaude test")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")

	const convID = "019ec004-4250-79b1-9ade-ebaea4159002"
	const sessionID = "agent-codex-branch"
	cx := testharness.NewCodexSimWithID(t, home, convID, repo)
	require.NoError(t, cx.Start())

	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      sessionID,
		ConvID:  convID,
		Status:  session.StatusIdle,
		Harness: "codex",
		Cwd:     repo,
	}))

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "SessionStart",
		ConvID:         convID,
		Cwd:            repo,
		TranscriptPath: cx.RolloutPath,
	}, sessionID))

	ws, err := db.GetAgentWorkspace(convID)
	require.NoError(t, err)
	assert.Equal(t, repo, ws.Cwd)
	assert.Equal(t, "main", ws.Branch, "Codex hooks publish the launch-dir branch")

	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "main", row.GitBranch)
	assert.Equal(t, "main", row.GitBranchStartup, "first observed Codex branch becomes init")

	runGit(t, repo, "checkout", "-b", "feature-x")
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName:  "Stop",
		ConvID:         convID,
		Cwd:            repo,
		TranscriptPath: cx.RolloutPath,
	}, sessionID))

	ws, err = db.GetAgentWorkspace(convID)
	require.NoError(t, err)
	assert.Equal(t, "feature-x", ws.Branch,
		"Codex current branch refreshes from git even when the rollout did not grow")

	loc := agent.ResolveLocation(convID)
	assert.Equal(t, "feature-x", loc.CurrentBranch, "dashboard resolver sees Codex current branch")
	assert.Equal(t, "main", loc.StartupBranch, "dashboard resolver keeps Codex init branch stable")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
