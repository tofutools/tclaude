package agentd_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A morph onto pi is budgeted like any other: to the window pi's models.json
// declares for a custom model, else the one a pi session last reported for
// it, and history that does not fit needs the human's confirmation.
func TestMorphOntoPi_BudgetsToTheModelWindowAndConfirmsLoss(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	f := newFlow(t)
	f.HaveGroup("crew")

	agentDir := testharness.PiAgentDirFor(f.World.HomeDir)
	require.NoError(t, os.MkdirAll(agentDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(
		`{"providers":{"local":{"models":[{"id":"small","contextWindow":16000}]}}}`), 0o600))

	// A pi agent on a built-in model reports its window through the
	// extension.
	_, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-big", "model": "openai/gpt-5.5"})
	typeInto(sim, "hello")
	sim.Measure(10_000, 400_000, 0.01, false)
	require.NoError(t, sim.Reply("hi"))

	spawn := spawnMorphWorker(t, f, "crew", "writer", nil)
	seedClaudeHistory(t, f, spawn.ConvID)
	cc := f.World.CCs.GetByConvID(spawn.ConvID)
	parent := "a2"
	for i := 0; i < 40; i++ {
		user, assistant := fmt.Sprintf("big-u-%d", i), fmt.Sprintf("big-a-%d", i)
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": user, "parentUuid": parent, "cwd": cc.Cwd, "message": map[string]any{"role": "user", "content": fmt.Sprintf("Task update %d", i)}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": assistant, "parentUuid": user, "cwd": cc.Cwd, "message": map[string]any{"role": "assistant", "content": strings.Repeat("Earlier implementation details. ", 800)}}))
		parent = assistant
	}
	convops.ScanAndUpsertFile(cc.JsonlPath)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	for _, tc := range []struct {
		model, source string
		window        int64
	}{
		{"local/small", "pi models.json", 16_000},
		{"openai/gpt-5.5", "pi reported context window", 400_000},
		{"elsewhere/unknown", "unknown model, assumed 32k", 32_000},
	} {
		body := map[string]any{"harness": "pi", "model": tc.model, "approval": "read-only", "dry_run": true}
		rec := f.AsHuman().Morph(spawn.AgentID, body)
		preview := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
		require.NotNil(t, preview.History, tc.model)
		assert.Equal(t, tc.window, preview.History.ContextWindowTokens, tc.model)
		assert.Equal(t, tc.source, preview.History.ContextWindowSource, tc.model)
		assert.Equal(t, tc.window*60/100, preview.History.BudgetTokens, tc.model)
		if tc.window < 100_000 {
			assert.True(t, preview.History.NeedsSummary, tc.model)
		}
	}

	body := map[string]any{"harness": "pi", "model": "local/small", "approval": "read-only"}
	rec := f.AsHuman().Morph(spawn.AgentID, body)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "history_confirmation_required")
	body["confirm_history_loss"] = true
	rec = f.AsHuman().Morph(spawn.AgentID, body)
	assert.Equal(t, "pending", decodeCrossMorph(t, rec.Code, rec.Body.Bytes()).Result)
	pending, err := db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, "summary", pending.History)
}
