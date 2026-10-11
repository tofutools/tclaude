package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestMorphEncryptedSummaryHandoffFlow(t *testing.T) {
	for _, action := range []string{"submit", "timeout", "cancel", "offline", "outage"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv(session.ResourceDelegationDirEnv, "")
			t.Setenv("CODEX_HOME", "")
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			f.HaveGroup("crew")
			source := spawnMorphSource(t, f, harness.CodexName, "encrypted")
			cx := f.World.Codexes.GetByConvID(source.ConvID)
			require.NotNil(t, cx)
			require.NoError(t, cx.WriteExchange("Original task before compaction", "Lost decision before compaction"))
			boundary, err := json.Marshal(map[string]any{"type": "compacted", "payload": map[string]any{"message": "", "replacement_history": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Retained original task"}}},
				map[string]any{"type": "compaction", "encrypted_content": "gAAAA"},
			}}})
			require.NoError(t, err)
			file, err := os.OpenFile(cx.RolloutPath, os.O_WRONLY|os.O_APPEND, 0)
			require.NoError(t, err)
			_, err = file.Write(append(boundary, '\n'))
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.NoError(t, cx.WriteExchange("Latest readable instructions", "Readable recent answer"))
			f.SetSessionStatus(source.ConvID, session.StatusIdle)
			body := map[string]any{"harness": "claude", "approval": "default", "trust_dir": true, "dry_run": true}
			preview := f.AsHuman().Morph(source.AgentID, body)
			report := decodeCrossMorph(t, preview.Code, preview.Body.Bytes()).History
			require.True(t, report.NeedsHandoff)
			require.False(t, report.NeedsSummary, "encrypted context uses the same wait without requiring a large-cut checkbox")
			assert.Equal(t, "native_handoff", report.Fallback)
			delete(body, "dry_run")
			if action == "offline" {
				f.MarkOffline(source.TmuxSession)
				refused := f.AsHuman().Morph(source.AgentID, body)
				require.Equal(t, http.StatusConflict, refused.Code, refused.Body.String())
				assert.Contains(t, refused.Body.String(), "proceed_without_summary")
				body["proceed_without_summary"] = true
			}
			result := f.AsHuman().Morph(source.AgentID, body)
			response := decodeCrossMorph(t, result.Code, result.Body.Bytes())
			if action != "offline" {
				require.Equal(t, "pending", response.Result)
				pending, err := db.PendingMorphForAgent(source.AgentID)
				require.NoError(t, err)
				require.NotNil(t, pending)
				assert.Equal(t, "encrypted_compaction_summary", pending.HandoffReason)
				assert.Equal(t, "summary", pending.History)
				agentd.SweepPendingMorphsForTest()
				unchanged, err := db.GetAgent(source.AgentID)
				require.NoError(t, err)
				require.Equal(t, source.ConvID, unchanged.CurrentConvID)
				switch action {
				case "submit":
					submitted := f.AsAgent(source.ConvID).MorphHandoff("Remembered handoff: the lost decision was to retain API compatibility.")
					require.Equal(t, http.StatusOK, submitted.Code, submitted.Body.String())
					f.SetSessionStatus(source.ConvID, session.StatusIdle)
				case "timeout":
					pending.ExpiresAt = time.Now().Add(-time.Minute)
					require.NoError(t, db.SetAgentPendingMorphForConv(source.ConvID, pending))
				case "cancel":
					cancelled := f.AsHuman().Morph(source.AgentID, map[string]any{"cancel": true})
					require.Equal(t, http.StatusOK, cancelled.Code)
				case "outage":
					body["proceed_without_summary"] = true
					fallback := f.AsHuman().Morph(source.AgentID, body)
					require.Equal(t, "morphed", decodeCrossMorph(t, fallback.Code, fallback.Body.Bytes()).Result)
				}
				agentd.SweepPendingMorphsForTest()
			}
			agent, err := db.GetAgent(source.AgentID)
			require.NoError(t, err)
			if action == "timeout" || action == "cancel" {
				assert.Equal(t, source.ConvID, agent.CurrentConvID)
				return
			}
			require.NotEqual(t, source.ConvID, agent.CurrentConvID)
			profile, err := db.ConversationResumeProfileForConv(agent.CurrentConvID)
			require.NoError(t, err)
			text := historyText(t, mustGetHarness(t, harness.DefaultName), agent.CurrentConvID, profile.Cwd)
			assert.Contains(t, text, "Latest readable instructions")
			assert.Contains(t, text, "Readable recent answer")
			if action == "submit" {
				assert.Contains(t, text, "Remembered handoff")
			} else {
				assert.Contains(t, text, "Compaction summary unreadable; source could not write a handoff")
				assert.Contains(t, text, "transcript")
			}
		})
	}
}
