package agentd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestRunAutoNameRefinesFallbackWithContainedOneShot(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := db.EnsureAgentForConv("conv-auto-name", "session-start")
	require.NoError(t, err)
	fallback := session.FreeFloatingAgentName(time.Date(2026, 7, 28, 12, 17, 33, 0, time.UTC), agentID)
	require.NoError(t, db.SetAgentPendingName(agentID, fallback))

	oldRunner := runAutoNameHarness
	t.Cleanup(func() { runAutoNameHarness = oldRunner })
	var gotPlan SeanceExecPlan
	runAutoNameHarness = func(_ context.Context, plan SeanceExecPlan) SeanceExecResult {
		gotPlan = plan
		return SeanceExecResult{Stdout: "repair-session-naming", Started: true}
	}

	runAutoName(agentID, "conv-auto-name", harness.DefaultName, t.TempDir(), fallback, "fix session naming")

	assert.Contains(t, gotPlan.Argv, "--permission-mode")
	assert.Contains(t, gotPlan.Argv, "plan")
	assert.True(t, strings.Contains(strings.Join(gotPlan.Argv, " "), "sandbox"), gotPlan.Argv)
	actor, err := db.GetAgent(agentID)
	require.NoError(t, err)
	assert.Equal(t, "repair-session-naming", actor.PendingName)
}

func TestRunAutoNameRejectsMalformedOutputAndPreservesExplicitRename(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := db.EnsureAgentForConv("conv-auto-name", "session-start")
	require.NoError(t, err)
	fallback := session.FreeFloatingAgentName(time.Now(), agentID)
	require.NoError(t, db.SetAgentPendingName(agentID, fallback))

	oldRunner := runAutoNameHarness
	t.Cleanup(func() { runAutoNameHarness = oldRunner })
	runAutoNameHarness = func(_ context.Context, _ SeanceExecPlan) SeanceExecResult {
		return SeanceExecResult{Stdout: "Here is a name: bad output.", Started: true}
	}
	runAutoName(agentID, "conv-auto-name", harness.CodexName, t.TempDir(), fallback, "fix naming")
	actor, err := db.GetAgent(agentID)
	require.NoError(t, err)
	assert.Equal(t, fallback, actor.PendingName)

	require.NoError(t, db.SetAgentPendingName(agentID, "human-chosen-name"))
	runAutoNameHarness = func(_ context.Context, _ SeanceExecPlan) SeanceExecResult {
		return SeanceExecResult{Stdout: "valid-model-name", Started: true}
	}
	runAutoName(agentID, "conv-auto-name", harness.CodexName, t.TempDir(), fallback, "fix naming")
	actor, err = db.GetAgent(agentID)
	require.NoError(t, err)
	assert.Equal(t, "human-chosen-name", actor.PendingName)
}

func TestRunAutoNameKeepsUniqueFallbackOnGeneratedNameCollision(t *testing.T) {
	setupTestDB(t)
	firstID, _, err := db.EnsureAgentForConv("conv-first", "session-start")
	require.NoError(t, err)
	secondID, _, err := db.EnsureAgentForConv("conv-second", "session-start")
	require.NoError(t, err)
	firstFallback := session.FreeFloatingAgentName(time.Date(2026, 7, 28, 12, 17, 33, 0, time.UTC), firstID)
	secondFallback := session.FreeFloatingAgentName(time.Date(2026, 7, 28, 12, 17, 33, 0, time.UTC), secondID)
	require.NoError(t, db.SetAgentPendingName(firstID, firstFallback))
	require.NoError(t, db.SetAgentPendingName(secondID, secondFallback))

	oldRunner := runAutoNameHarness
	t.Cleanup(func() { runAutoNameHarness = oldRunner })
	runAutoNameHarness = func(_ context.Context, _ SeanceExecPlan) SeanceExecResult {
		return SeanceExecResult{Stdout: "fix-session-naming", Started: true}
	}

	runAutoName(firstID, "conv-first", harness.DefaultName, t.TempDir(), firstFallback, "fix naming")
	runAutoName(secondID, "conv-second", harness.DefaultName, t.TempDir(), secondFallback, "fix naming")

	first, err := db.GetAgent(firstID)
	require.NoError(t, err)
	second, err := db.GetAgent(secondID)
	require.NoError(t, err)
	assert.Equal(t, "fix-session-naming", first.PendingName)
	assert.Equal(t, secondFallback, second.PendingName,
		"a colliding generated title must preserve the actor's unique fallback")
}

func TestBrokeredAutoNameTargetRejectsForeignConversation(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:      "broker-caller",
		ConvID:  "caller-conv",
		Harness: harness.CodexName,
		Cwd:     t.TempDir(),
		Status:  session.StatusWorking,
	}))

	_, _, ok := brokeredAutoNameTarget("broker-caller", "victim-conv")
	assert.False(t, ok, "an ignored foreign-conversation hook must not continue into naming")

	gotHarness, gotCwd, ok := brokeredAutoNameTarget("broker-caller", "caller-conv")
	assert.True(t, ok)
	assert.Equal(t, harness.CodexName, gotHarness)
	assert.NotEmpty(t, gotCwd)
}
