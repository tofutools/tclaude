package session_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
