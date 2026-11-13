package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// seedLongCopilotHistory writes n turns, each with a large tool output, into
// a Copilot agent's native event log.
func seedLongCopilotHistory(t *testing.T, f *testharness.Flow, convID string, n int) {
	t.Helper()
	c := f.World.Copilots.GetByConvID(convID)
	require.NotNil(t, c)
	require.NoError(t, c.AppendEvent(map[string]any{"type": "user.message", "data": map[string]any{"content": "ORIGINAL TASK: " + morphSecret}}))
	for i := range n {
		id := fmt.Sprintf("call_%d", i)
		for _, ev := range []map[string]any{
			{"type": "assistant.message", "data": map[string]any{"content": fmt.Sprintf("step %d ", i) + strings.Repeat("words ", 150), "model": "gpt-5.4",
				"toolRequests": []any{map[string]any{"toolCallId": id, "name": "bash", "arguments": map[string]any{"command": fmt.Sprintf("cat file%d", i)}}}}},
			{"type": "tool.execution_complete", "data": map[string]any{"toolCallId": id, "success": true,
				"result": map[string]any{"content": fmt.Sprintf("OUTPUT%d ", i) + strings.Repeat("x", 4000)}}},
			{"type": "user.message", "data": map[string]any{"content": fmt.Sprintf("next %d", i)}},
		} {
			require.NoError(t, c.AppendEvent(ev))
		}
	}
}

func carryUsage(r *transcript.LossReport, c transcript.Category) transcript.CategoryUsage {
	for _, u := range r.Categories {
		if u.Category == c {
			return u
		}
	}
	return transcript.CategoryUsage{}
}

// The operator states the destination window, carries everything, keeps a
// quarter of the tool output and skips the summary. The dry run shows the
// same plan the morph then applies, and the morphed agent is told in its
// carried history what was cut.
func TestMorphCarryOverridesPreviewAndApply(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphSource(t, f, harness.CopilotName, "carrier")
	seedLongCopilotHistory(t, f, spawn.ConvID, 40)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	base := map[string]any{"harness": "claude", "model": "sonnet", "approval": "default", "trust_dir": true}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// Automatic: sonnet's 200k window, a 60% budget.
	auto := morphJSON(t, f, spawn.AgentID, with(map[string]any{"dry_run": true}))
	require.NotNil(t, auto.History)
	assert.Equal(t, int64(200_000), auto.History.ContextWindowTokens)
	assert.Equal(t, "auto", auto.History.BudgetSource)
	assert.False(t, auto.History.WindowStated)
	require.Len(t, auto.History.Categories, 3)
	assert.Positive(t, carryUsage(auto.History, transcript.CategoryToolOutput).SourceTokens)

	// A stated window smaller than the history, everything carried, a
	// quarter of the tool output kept whole, no summary.
	carry := map[string]any{"target_window_tokens": 40_000, "history_budget": "all", "history_keep": map[string]int{"tool_output": 25}, "history": "truncate"}
	dry := morphJSON(t, f, spawn.AgentID, with(merge(carry, map[string]any{"dry_run": true})))
	require.NotNil(t, dry.History)
	assert.Equal(t, int64(40_000), dry.History.ContextWindowTokens)
	assert.True(t, dry.History.WindowStated)
	assert.Equal(t, int64(200_000), dry.History.DetectedWindowTokens, "the detected window is shown beside the stated one")
	assert.True(t, dry.History.CarryAll)
	assert.Zero(t, dry.History.Count(transcript.LossTruncatedBudget), "nothing is omitted")
	assert.True(t, dry.History.OverWindow, "carrying more than the stated window warns")
	assert.False(t, dry.History.NeedsSummary)
	assert.Equal(t, "native", dry.History.Fallback, "carry-all is delivered natively despite the warning")
	out := carryUsage(dry.History, transcript.CategoryToolOutput)
	require.NotNil(t, out.KeepPercent)
	assert.Equal(t, 25, *out.KeepPercent)
	assert.Positive(t, out.Shortened)

	rec := f.AsHuman().Morph(spawn.AgentID, with(carry))
	resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", resp.Result)
	require.NotNil(t, resp.History)
	assert.Equal(t, out.Shortened, carryUsage(resp.History, transcript.CategoryToolOutput).Shortened, "the morph applies the previewed plan")
	profile, err := db.ConversationResumeProfileForConv(resp.ConvID)
	require.NoError(t, err)
	text := historyText(t, mustGetHarness(t, harness.DefaultName), resp.ConvID, profile.Cwd)
	assert.Contains(t, text, "Cut during conversion, on the operator's carry-over settings")
	assert.Contains(t, text, "older tool outputs")
	assert.Contains(t, text, "OUTPUT39 ", "the newest output is whole")
	assert.Contains(t, text, "next 0", "every turn is carried")
	assert.Contains(t, text, morphSecret)
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func morphJSON(t *testing.T, f *testharness.Flow, agentID string, body map[string]any) crossMorphResp {
	t.Helper()
	rec := f.AsHuman().Morph(agentID, body)
	return decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
}

// Cut-first: the tool output gives way before whole turns are omitted.
func TestMorphCarryCutFirstToolOutput(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphSource(t, f, harness.CopilotName, "cutter")
	seedLongCopilotHistory(t, f, spawn.ConvID, 40)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	body := map[string]any{"harness": "claude", "model": "sonnet", "approval": "default", "dry_run": true,
		"history_budget": "50k", "history_keep": map[string]int{"tool_calls": 100}}
	oldest := morphJSON(t, f, spawn.AgentID, merge(body, map[string]any{"history_keep": map[string]int{"tool_calls": 100, "tool_output": 100}}))
	require.Positive(t, oldest.History.Count(transcript.LossTruncatedBudget), "the fixture overflows a 50k budget")
	assert.Equal(t, int64(50_000), oldest.History.BudgetTokens)
	assert.Equal(t, "stated", oldest.History.BudgetSource)

	cut := morphJSON(t, f, spawn.AgentID, merge(body, map[string]any{"history_cut_first": "tool_output"}))
	assert.Zero(t, cut.History.Count(transcript.LossTruncatedBudget), "shortening tool output made every turn fit")
	assert.Equal(t, transcript.CategoryToolOutput, cut.History.CutFirst)
	u := carryUsage(cut.History, transcript.CategoryToolOutput)
	require.NotNil(t, u.KeepPercent)
	assert.Less(t, *u.KeepPercent, 100)
}

func TestMorphCarryRefusals(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "refuser", nil)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	for _, c := range []struct {
		body map[string]any
		code int
		want string
	}{
		{map[string]any{"model": "opus", "history_budget": "all"}, http.StatusBadRequest, "only a cross-harness morph"},
		{map[string]any{"harness": "codex", "approval": "on-request", "history_budget": "lots"}, http.StatusBadRequest, "history_budget must be"},
		{map[string]any{"harness": "codex", "approval": "on-request", "history_cut_first": "reasoning"}, http.StatusBadRequest, "history_cut_first must be"},
		{map[string]any{"harness": "codex", "approval": "on-request", "history_keep": map[string]int{"tool_output": 101}}, http.StatusBadRequest, "0-100"},
		{map[string]any{"harness": "codex", "approval": "on-request", "target_window_tokens": 5}, http.StatusBadRequest, "target_window_tokens"},
		{map[string]any{"harness": "codex", "approval": "on-request", "history": "squash"}, http.StatusBadRequest, "history must be"},
	} {
		rec := f.AsHuman().Morph(spawn.AgentID, c.body)
		assert.Equalf(t, c.code, rec.Code, "%v: %s", c.body, rec.Body.String())
		assert.Contains(t, rec.Body.String(), c.want)
	}
}

// A profile named for a large window whose model lacks [1m] previews the
// standard window and says why the auto-compact window does not raise it.
func TestMorphWindowSourceExplainsIneffectiveAutoCompact(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphSource(t, f, harness.CopilotName, "windowed")
	seedMorphHistory(t, f, harness.CopilotName, spawn.ConvID)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	for name, model := range map[string]string{"plain[500k]": "fable", "wide[500k]": "fable[1m]"} {
		_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: name, Harness: "claude", Model: model, Approval: "default", AutoCompactWindow: "500000"})
		require.NoError(t, err)
	}
	plain := morphJSON(t, f, spawn.AgentID, map[string]any{"profile": "plain[500k]", "dry_run": true})
	assert.Equal(t, int64(200_000), plain.History.ContextWindowTokens)
	assert.Contains(t, plain.History.ContextWindowSource, "fable without [1m], so auto-compact 500k has no effect")
	wide := morphJSON(t, f, spawn.AgentID, map[string]any{"profile": "wide[500k]", "dry_run": true})
	assert.Equal(t, int64(500_000), wide.History.ContextWindowTokens)
	assert.Equal(t, "Claude auto-compact ceiling", wide.History.ContextWindowSource)
}

// A backup keeps the agent's old form as a separate, stopped agent with no
// groups or permissions, while the agent itself morphs.
func TestMorphBackupKeepsAStoppedStandaloneClone(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "shifter", nil)
	seedClaudeHistory(t, f, spawn.ConvID)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"harness": "codex", "model": "gpt-5.4", "approval": "on-request", "backup": true})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Result string `json:"result"`
		ConvID string `json:"conv_id"`
		Backup *struct {
			AgentID string `json:"agent_id"`
			ConvID  string `json:"conv_id"`
			Name    string `json:"name"`
			Warning string `json:"warning"`
		} `json:"backup"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "morphed", resp.Result)
	require.NotNil(t, resp.Backup, "the response names the backup")
	assert.Equal(t, "shifter (pre-morph backup)", resp.Backup.Name)
	require.NotEmptyf(t, resp.Backup.AgentID, "%+v", *resp.Backup)
	assert.NotEqual(t, spawn.AgentID, resp.Backup.AgentID, "the backup is its own agent")

	// The agent itself morphed and kept its group.
	agent, err := db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Equal(t, resp.ConvID, agent.CurrentConvID)
	groups, err := db.ListGroupsForConv(resp.ConvID)
	require.NoError(t, err)
	assert.Len(t, groups, 1)

	// The backup stays on the old harness, outside every group, stopped.
	backupProfile, err := db.ConversationResumeProfileForConv(resp.Backup.ConvID)
	require.NoError(t, err)
	require.NotNil(t, backupProfile)
	assert.Equal(t, harness.DefaultName, harnessOrDefaultName(backupProfile.Harness))
	backupGroups, err := db.ListGroupsForConv(resp.Backup.ConvID)
	require.NoError(t, err)
	assert.Empty(t, backupGroups, "a backup joins no group")
	assert.False(t, convAlive(t, resp.Backup.ConvID), "the backup is stopped: %s", resp.Backup.Warning)
	text := historyText(t, mustGetHarness(t, harness.DefaultName), resp.Backup.ConvID, backupProfile.Cwd)
	assert.Contains(t, text, morphSecret, "the backup carries the old history")
}

func harnessOrDefaultName(name string) string {
	if name == "" {
		return harness.DefaultName
	}
	return name
}

func convAlive(t *testing.T, convID string) bool {
	t.Helper()
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	for _, r := range rows {
		if r.TmuxSession != "" && session.IsTmuxSessionAlive(r.TmuxSession) {
			return true
		}
	}
	return false
}

func TestMorphBackupRefusesAnOfflineAgent(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "sleeper", nil)
	f.AsHuman().Stop(spawn.ConvID, true)
	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": "opus", "backup": true})
	assert.Equalf(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "backup_offline")
}
