package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestMorphOverBudgetSummaryFlow(t *testing.T) {
	for _, action := range []string{"submit", "timeout", "cancel", "busy_timeout", "outage_fallback", "offline_fallback"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv(session.ResourceDelegationDirEnv, "")
			t.Setenv("CODEX_HOME", "")
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			f.HaveGroup("crew")
			spawn := spawnMorphWorker(t, f, "crew", "writer", nil)
			seedClaudeHistory(t, f, spawn.ConvID)
			cc := f.World.CCs.GetByConvID(spawn.ConvID)
			parent := "a2"
			for i := 0; i < 40; i++ {
				user, assistant := fmt.Sprintf("big-u-%d", i), fmt.Sprintf("big-a-%d", i)
				instruction := fmt.Sprintf("Task update %d: keep the API compatible", i)
				require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": user, "parentUuid": parent, "cwd": cc.Cwd, "message": map[string]any{"role": "user", "content": instruction}}))
				require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": assistant, "parentUuid": user, "cwd": cc.Cwd, "message": map[string]any{"role": "assistant", "content": strings.Repeat("Earlier implementation details. ", 800)}}))
				parent = assistant
			}
			convops.ScanAndUpsertFile(cc.JsonlPath)
			f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
			body := map[string]any{"harness": "codex", "approval": "on-request", "dry_run": true}
			rec := f.AsHuman().Morph(spawn.AgentID, body)
			preview := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
			require.True(t, preview.History.NeedsSummary)
			require.GreaterOrEqual(t, preview.History.DroppedPercent, 25)
			delete(body, "dry_run")
			rec = f.AsHuman().Morph(spawn.AgentID, body)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "history_confirmation_required")
			pending, err := db.PendingMorphForAgent(spawn.AgentID)
			require.NoError(t, err)
			require.Nil(t, pending)
			body["confirm_history_loss"] = true
			rec = f.AsHuman().Morph(spawn.AgentID, body)
			require.Equal(t, "pending", decodeCrossMorph(t, rec.Code, rec.Body.Bytes()).Result)
			pending, err = db.PendingMorphForAgent(spawn.AgentID)
			require.NoError(t, err)
			require.Equal(t, "summary", pending.History)
			require.True(t, pending.HandoffRequired)
			status := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+spawn.AgentID+"/morph", nil))
			require.Equal(t, http.StatusOK, status.Code, status.Body.String())
			assert.Contains(t, status.Body.String(), "pending_morph")
			agentd.SweepPendingMorphsForTest()
			agent, err := db.GetAgent(spawn.AgentID)
			require.NoError(t, err)
			require.Equal(t, spawn.ConvID, agent.CurrentConvID)
			switch action {
			case "submit", "busy_timeout":
				// The real inbox path injects a native user nudge before the agent answers.
				require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "summary-nudge", "parentUuid": parent, "cwd": cc.Cwd, "message": map[string]any{"role": "user", "content": "[system: new agent message #123; delivery: inline; subject: Morph handoff requested] Write a history summary."}}))
				require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": "summary-response", "parentUuid": "summary-nudge", "cwd": cc.Cwd, "message": map[string]any{"role": "assistant", "content": strings.Repeat("Preparing the requested summary. ", 800)}}))
				convops.ScanAndUpsertFile(cc.JsonlPath)
				rec = f.AsAgent(spawn.ConvID).MorphHandoff("Earlier work summary: parser migration complete; preserve compatibility; next verify deployment.")
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
				if action == "busy_timeout" {
					f.SetSessionStatus(spawn.ConvID, session.StatusWorking)
					pending, err = db.PendingMorphForAgent(spawn.AgentID)
					require.NoError(t, err)
					pending.ExpiresAt = time.Now().Add(-time.Minute)
					require.NoError(t, db.SetAgentPendingMorphForConv(spawn.ConvID, pending))
				}
			case "timeout":
				pending.ExpiresAt = time.Now().Add(-time.Minute)
				require.NoError(t, db.SetAgentPendingMorphForConv(spawn.ConvID, pending))
			case "outage_fallback", "offline_fallback":
				if action == "offline_fallback" {
					f.MarkOffline(spawn.TmuxSession)
				}
				body["proceed_without_summary"] = true
				rec = f.AsHuman().Morph(spawn.AgentID, body)
				require.Equal(t, "morphed", decodeCrossMorph(t, rec.Code, rec.Body.Bytes()).Result)
			case "cancel":
				rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"cancel": true})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			agentd.SweepPendingMorphsForTest()
			agent, err = db.GetAgent(spawn.AgentID)
			require.NoError(t, err)
			pending, err = db.PendingMorphForAgent(spawn.AgentID)
			require.NoError(t, err)
			require.Nil(t, pending)
			if action != "submit" && action != "outage_fallback" && action != "offline_fallback" {
				assert.Equal(t, spawn.ConvID, agent.CurrentConvID, "cancel or timeout leaves the source unchanged")
				return
			}
			require.NotEqual(t, spawn.ConvID, agent.CurrentConvID)
			profile, err := db.ConversationResumeProfileForConv(agent.CurrentConvID)
			require.NoError(t, err)
			text := historyText(t, mustGetHarness(t, harness.CodexName), agent.CurrentConvID, profile.Cwd)
			assert.Contains(t, text, morphSecret)
			assert.Contains(t, text, "Task update 39")
			if action == "submit" {
				assert.Contains(t, text, "Earlier work summary")
			} else {
				assert.NotContains(t, text, "Earlier work summary")
				assert.Contains(t, text, "Source could not write a summary")
				assert.Contains(t, text, "transcript.md")
			}
		})
	}
}
