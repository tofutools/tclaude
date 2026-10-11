package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestMorphHistoryWindowCatalogAndExplicitOverride(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "worker", nil)
	seedClaudeHistory(t, f, spawn.ConvID)
	body := map[string]any{"harness": "copilot", "model": "auto", "approval": "yolo", "dry_run": true}
	preview := func() {
		t.Helper()
		rec := f.AsHuman().Morph(spawn.AgentID, body)
		r := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
		require.NotNil(t, r.History)
		if body["history_window_tokens"] == nil {
			assert.EqualValues(t, 32_000, r.History.ContextWindowTokens)
			assert.EqualValues(t, 19_200, r.History.BudgetTokens)
			assert.Equal(t, "unknown model, assumed 32k", r.History.ContextWindowSource)
		} else {
			assert.EqualValues(t, 64_000, r.History.ContextWindowTokens)
			assert.Equal(t, "explicit history window", r.History.ContextWindowSource)
		}
	}
	preview()
	body["history_window_tokens"] = 64_000
	preview()
	require.NoError(t, db.ReplaceCopilotModelCatalog([]db.CopilotModelCatalogEntry{{ModelID: "auto", MaxPromptTokens: 16_000}}, time.Now()))
	rec := f.AsHuman().Morph(spawn.AgentID, body)
	r := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.NotNil(t, r.History)
	assert.EqualValues(t, 16_000, r.History.ContextWindowTokens, "a verified smaller destination limit wins")
	assert.Equal(t, "Copilot model prompt limit", r.History.ContextWindowSource)
	body["history_window_tokens"] = 1023
	rec = f.AsHuman().Morph(spawn.AgentID, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	agent, err := db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Equal(t, spawn.ConvID, agent.CurrentConvID, "preview and rejected overrides leave the source running")
}

func TestMorphHistoryWindowRespectsProfileAndExtendedClaude(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "worker", nil)
	seedClaudeHistory(t, f, spawn.ConvID)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "small", Harness: "copilot", Model: "auto", Approval: "yolo", ContextWindowMax: 16_000})
	require.NoError(t, err)
	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"profile": "small", "history_window_tokens": 64_000, "dry_run": true})
	r := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.NotNil(t, r.History)
	assert.EqualValues(t, 16_000, r.History.ContextWindowTokens)

	codex := spawnMorphSource(t, f, "codex", "other")
	seedMorphHistory(t, f, "codex", codex.ConvID)
	f.SetSessionStatus(codex.ConvID, session.StatusIdle)
	for _, body := range []map[string]any{
		{"harness": "claude", "model": "sonnet[1m]", "approval": "default", "dry_run": true},
		{"harness": "claude", "model": "sonnet[1m]", "approval": "default", "history_window_tokens": 500_000, "dry_run": true},
	} {
		rec = f.AsHuman().Morph(codex.AgentID, body)
		r = decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
		require.NotNil(t, r.History)
		want := int64(1_000_000)
		if body["history_window_tokens"] != nil {
			want = 500_000
		}
		assert.Equal(t, want, r.History.ContextWindowTokens)
		assert.Equal(t, want*60/100, r.History.BudgetTokens)
	}
}
