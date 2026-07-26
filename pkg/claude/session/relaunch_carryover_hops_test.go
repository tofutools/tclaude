package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resumeHop replays what `tclaude session new -r <conv>` does to the durable
// record when a human types no launch flags at all: resolve the posture, write
// the fresh launch row, then record the postures SaveSession's UPSERT does not
// own. Everything tmux- and pane-related is irrelevant to the erasure, so this
// covers the write ordering that is.
//
// Each hop stamps a later created_at, which is what makes it a NEW launch
// generation to the durable projection — the same thing that made the loss
// permanent rather than a one-launch fallback.
func resumeHop(t *testing.T, convID, cwd string, hop int) *NewParams {
	t.Helper()
	params := &NewParams{Resume: convID, Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))

	h, err := harness.Resolve(params.Harness)
	require.NoError(t, err)
	autoMemory, err := harness.ResolveAutoMemory(h, &params.AutoMemory)
	require.NoError(t, err)
	requested, err := harness.ParseContextFeatures(params.ContextFeatures)
	require.NoError(t, err)
	contextFeatures, err := harness.ResolveContextFeatures(h, requested)
	require.NoError(t, err)
	autoCompactWindow, err := harness.ResolveAutoCompactWindow(h, params.AutoCompactWindow)
	require.NoError(t, err)
	askTimeout, err := harness.ResolveAskTimeoutMode(h, params.AskUserQuestionTimeout)
	require.NoError(t, err)
	sandboxMode, err := harness.ValidateSandboxMode(h, params.Sandbox)
	require.NoError(t, err)
	remoteControl, err := harness.ResolveRemoteControl(h, params.RemoteControl)
	require.NoError(t, err)

	created := time.Now().Add(time.Duration(hop) * time.Minute)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: convID, ConvID: convID, Cwd: cwd, Status: "idle", Harness: h.Name,
		SandboxMode: sandboxMode, SandboxImplementation: params.SandboxImpl,
		AskUserQuestionTimeout: askTimeout,
		ApprovalPolicy:         harness.ClaudePermissionInherit,
		CreatedAt:              created, UpdatedAt: created,
	}))
	RecordLaunchPosture(convID, h, LaunchPosture{
		AutoMemory:        autoMemory,
		ContextFeatures:   contextFeatures,
		AutoCompactWindow: autoCompactWindow,
		RemoteControl:     remoteControl,
	})
	return params
}

// TestRecordedLaunchPostureSurvivesRepeatedFlaglessResumes is the TCL-730
// regression at the level the bug actually lived: not one launch, but the
// second and third. A flagless resume used to record its resolved defaults over
// the conversation's posture, so generation 2 read back "nothing pinned" and
// the original intent could never be recovered. Every hop here passes no flags.
func TestRecordedLaunchPostureSurvivesRepeatedFlaglessResumes(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	for hop := 1; hop <= 3; hop++ {
		t.Run(fmt.Sprintf("hop-%d", hop), func(t *testing.T) {
			params := resumeHop(t, carryoverConvID, cwd, hop)
			assert.Equal(t, "450000", params.AutoCompactWindow, "the pinned auto-compaction window must survive")
			assert.True(t, params.AutoMemory, "an opted-in agent must not silently revert to memory off")
			assert.Equal(t, "bundled-skills=off", params.ContextFeatures, "a lean agent must come back lean")
			assert.Equal(t, "5m", params.AskUserQuestionTimeout, "the auto-continue timeout must survive")
			assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox, "the recorded containment must survive")
			assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), params.SandboxImpl,
				"the outer sandbox implementation must survive")
			assert.True(t, params.RemoteControl, "an agent reachable from the app must come back reachable")
		})
	}

	// And the durable record itself, which is what a fourth relaunch would read.
	posture, err := db.RecordedLaunchPostureForConv(carryoverConvID)
	require.NoError(t, err)
	require.NotNil(t, posture)
	require.NotNil(t, posture.AutoCompactWindow)
	assert.Equal(t, "450000", *posture.AutoCompactWindow)
	require.NotNil(t, posture.AutoMemory)
	assert.True(t, *posture.AutoMemory)
	require.NotNil(t, posture.AskUserQuestionTimeout)
	assert.Equal(t, "5m", *posture.AskUserQuestionTimeout)
	require.NotNil(t, posture.RemoteControl, "RecordLaunchPosture must give RemoteControl a write-side twin")
	assert.True(t, *posture.RemoteControl)
	require.NotNil(t, posture.SandboxImplementation)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), *posture.SandboxImplementation)
}

// TestFlaglessResumeOfAnUnrecordedConvChangesNothing pins that the carryover
// does not invent a posture for a conversation that never had one: a legacy
// conv must still relaunch on plain defaults.
func TestFlaglessResumeOfAnUnrecordedConvChangesNothing(t *testing.T) {
	cwd := carryoverTestHome(t)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      carryoverConvID,
		ProjectDir:  convops.GetClaudeProjectPath(cwd),
		ProjectPath: cwd,
		Created:     "2026-01-01T00:00:00Z",
	}))

	params := resumeHop(t, carryoverConvID, cwd, 1)
	assert.Empty(t, params.AutoCompactWindow)
	assert.False(t, params.AutoMemory)
	assert.Empty(t, params.ContextFeatures)
	assert.Empty(t, params.AskUserQuestionTimeout)
	assert.Empty(t, params.Sandbox)
	assert.False(t, params.RemoteControl)
}
