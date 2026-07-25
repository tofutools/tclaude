package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-730 — `conv resume` and the watch-mode resume already replayed the
// recorded sandbox, approval, auto-memory, startup-context and auto-compaction
// postures, but not the AskUserQuestion idle-timeout. That cost the resumed
// agent its auto-continue behaviour AND wrote a blank column that the durable
// projection then asserted as "known: inherit" — so the value was gone for
// every later relaunch too, not just this one.

// TestResumeLaunchCmd_PreservesRecordedAskTimeout pins that the recorded
// timeout reaches the resumed pane. It rides the `--settings` payload rather
// than a flag, so it has to be on the spec, not just in the environment.
func TestResumeLaunchCmd_PreservesRecordedAskTimeout(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "ask-timeout-source", ConvID: resumeConvClaude, Harness: harness.DefaultName,
		Cwd: t.TempDir(), AskUserQuestionTimeout: "5m",
	}))

	cmd, _, h, err := resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	require.Equal(t, harness.DefaultName, h.Name)
	assert.Contains(t, cmd, "askUserQuestionTimeout",
		"the recorded AskUserQuestion timeout must reach the resumed pane's --settings payload")
	assert.Contains(t, cmd, "5m")
}

// TestResumeAskTimeout_UnrecordedStaysInherit pins that a conversation with
// nothing recorded keeps Claude Code's own settings.json posture: the carryover
// replays a decision, it never invents one.
func TestResumeAskTimeout_UnrecordedStaysInherit(t *testing.T) {
	setupTestDB(t)
	h, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	timeout, err := resumeAskTimeout(h, resumeConvClaude)
	require.NoError(t, err)
	assert.Empty(t, timeout)
}

// TestResumeAskTimeout_DroppedForAHarnessWithoutTheDialog pins the fail-soft
// rule: a Claude-only posture recorded against a conv now resuming under Codex
// is dropped, not turned into a resume failure.
func TestResumeAskTimeout_DroppedForAHarnessWithoutTheDialog(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "ask-timeout-codex", ConvID: resumeConvCodex, Harness: harness.DefaultName,
		AskUserQuestionTimeout: "5m",
	}))
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	timeout, err := resumeAskTimeout(codex, resumeConvCodex)
	require.NoError(t, err, "a posture the harness cannot honour is dropped, never fatal")
	assert.Empty(t, timeout)
}

// TestResumeRemoteControl_PreservesRecordedPosture pins that an agent that was
// reachable from claude.ai before the resume is reachable after it. The daemon's
// own relaunch path already carried this; a human-typed resume dropped it.
func TestResumeRemoteControl_PreservesRecordedPosture(t *testing.T) {
	setupTestDB(t)
	const sessionID = "remote-control-source"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: resumeConvClaude, Harness: harness.DefaultName, Cwd: t.TempDir(),
	}))
	require.NoError(t, db.SetSessionRemoteControl(sessionID, true))

	h, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	on, err := resumeRemoteControl(h, resumeConvClaude)
	require.NoError(t, err)
	assert.True(t, on)

	// Never in the direction of more exposure: a harness with no Remote Access
	// resumes without it rather than failing.
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	off, err := resumeRemoteControl(codex, resumeConvClaude)
	require.NoError(t, err)
	assert.False(t, off)
	// And an unrecorded conversation stays off.
	unrecorded, err := resumeRemoteControl(h, resumeConvCodex)
	require.NoError(t, err)
	assert.False(t, unrecorded)
}

// TestResumeContextPosture_ReadsTheRecordedValues pins the row-side read the
// watch-mode resume needs: without it that resume applied the posture to the
// pane but left its own row blank, so the NEXT resume read "nothing recorded"
// and the posture evaporated at generation 2.
func TestResumeContextPosture_ReadsTheRecordedValues(t *testing.T) {
	setupTestDB(t)
	const sessionID = "watch-posture-source"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: resumeConvClaude, Harness: harness.DefaultName, Cwd: t.TempDir(),
	}))
	require.NoError(t, db.SetSessionAutoMemory(sessionID, true))
	require.NoError(t, db.SetSessionContextFeatures(sessionID, map[string]string{"bundled-skills": "off"}))
	require.NoError(t, db.SetSessionAutoCompactWindow(sessionID, "450000"))

	autoMemory, contextFeatures, autoCompactWindow, err := resumeContextPosture(resumeConvClaude)
	require.NoError(t, err)
	assert.True(t, autoMemory)
	assert.Equal(t, map[string]string{"bundled-skills": "off"}, contextFeatures)
	assert.Equal(t, "450000", autoCompactWindow)
}
