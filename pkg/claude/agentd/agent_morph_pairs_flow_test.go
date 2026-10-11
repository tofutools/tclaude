package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// seedMorphHistory writes a short exchange with one tool call into a spawned
// agent's native history, in that harness's own on-disk format, through its
// simulator: what the agent would have written.
func seedMorphHistory(t *testing.T, f *testharness.Flow, harnessName, convID string) {
	t.Helper()
	switch harnessName {
	case harness.DefaultName:
		seedClaudeHistory(t, f, convID)
	case harness.CodexName:
		cx := f.World.Codexes.GetByConvID(convID)
		require.NotNil(t, cx)
		require.NoError(t, cx.WriteExchange(morphSecret, "Noted."))
		require.NoError(t, cx.WriteUserInput("List the files."))
		require.NoError(t, cx.AppendResponseItem(map[string]any{"type": "function_call", "name": "exec_command",
			"arguments": `{"cmd":"ls"}`, "call_id": "call_1"}))
		require.NoError(t, cx.AppendResponseItem(map[string]any{"type": "function_call_output", "call_id": "call_1",
			"output": "README.md\nmain.go"}))
		require.NoError(t, cx.WriteAgentMessage("There are two files."))
	case harness.CopilotName:
		c := f.World.Copilots.GetByConvID(convID)
		require.NotNil(t, c)
		for _, ev := range []map[string]any{
			{"type": "user.message", "data": map[string]any{"content": morphSecret}},
			{"type": "assistant.message", "data": map[string]any{"content": "", "model": "gpt-5.4",
				"toolRequests": []any{map[string]any{"toolCallId": "call_1", "name": "bash", "arguments": map[string]any{"command": "ls"}}}}},
			{"type": "tool.execution_complete", "data": map[string]any{"toolCallId": "call_1", "success": true,
				"result": map[string]any{"content": "README.md\nmain.go"}}},
			{"type": "assistant.message", "data": map[string]any{"content": "There are two files.", "model": "gpt-5.4"}},
		} {
			require.NoError(t, c.AppendEvent(ev))
		}
	case harness.GeminiName:
		g := f.World.Geminis.GetByConvID(convID)
		require.NotNil(t, g)
		for _, rec := range []map[string]any{
			{"id": "m-u1", "timestamp": "2026-10-01T10:00:00Z", "type": "user", "content": []any{map[string]any{"text": morphSecret}}},
			{"id": "m-g1", "timestamp": "2026-10-01T10:00:01Z", "type": "gemini", "content": "", "model": "gemini-2.5-pro",
				"toolCalls": []any{map[string]any{"id": "call_1", "name": "run_shell_command", "args": map[string]any{"command": "ls"},
					"status": "success", "result": []any{map[string]any{"functionResponse": map[string]any{
						"id": "call_1", "name": "run_shell_command", "response": map[string]any{"output": "README.md\nmain.go"}}}}}}},
			{"id": "m-g2", "timestamp": "2026-10-01T10:00:02Z", "type": "gemini", "content": "There are two files.", "model": "gemini-2.5-pro"},
		} {
			require.NoError(t, g.AppendRecord(rec))
		}
	default:
		t.Fatalf("no history seeder for %s", harnessName)
	}
}

func spawnMorphSource(t *testing.T, f *testharness.Flow, harnessName, name string) testharness.SpawnResp {
	t.Helper()
	if harnessName == harness.DefaultName {
		return spawnMorphWorker(t, f, "crew", name, nil)
	}
	// A pane parked on its folder-trust dialog never starts a session, so
	// it has no history to carry; trust the directory as an operator would.
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": name, "harness": harnessName,
		"trust_dir": harnessName != harness.CodexName})
	require.Equalf(t, http.StatusOK, spawn.Code, "%s spawn body=%s", harnessName, spawn.Raw)
	return spawn
}

// Every simulated pair moves through the morph endpoint: the destination
// resumes a native conversation that carries the source's turns and tool
// output, as the same agent. OpenCode imports through the real `opencode`
// CLI, so its pairs are covered by the TCLAUDE_MORPH_REALBIN test instead.
func TestMorphCrossHarnessPairMatrix(t *testing.T) {
	names := []string{harness.DefaultName, harness.CodexName, harness.CopilotName, harness.GeminiName}
	for _, src := range names {
		for _, dst := range names {
			if src == dst {
				continue
			}
			t.Run(src+"->"+dst, func(t *testing.T) {
				t.Setenv(session.ResourceDelegationDirEnv, "")
				t.Setenv("CODEX_HOME", "")
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
				f := newFlow(t)
				f.HaveGroup("crew")
				spawn := spawnMorphSource(t, f, src, "pair")
				seedMorphHistory(t, f, src, spawn.ConvID)
				f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

				dstHarness := mustGetHarness(t, dst)
				body := map[string]any{"harness": dst, "approval": dstHarness.Approval.DefaultPolicy(), "trust_dir": true}
				rec := f.AsHuman().Morph(spawn.AgentID, body)
				resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
				require.Equal(t, "morphed", resp.Result)
				require.NotNil(t, resp.History)
				assert.Equal(t, "native", resp.History.Fallback)
				require.NotEqual(t, spawn.ConvID, resp.ConvID)

				agent, err := db.GetAgent(spawn.AgentID)
				require.NoError(t, err)
				assert.Equal(t, resp.ConvID, agent.CurrentConvID, "the same agent continues on the new conversation")
				_, launched := f.World.SpawnModel(resp.ConvID)
				assert.True(t, launched, "%s resumed the translated conversation", dst)
				profile, err := db.ConversationResumeProfileForConv(resp.ConvID)
				require.NoError(t, err)
				require.NotNil(t, profile)
				assert.Equal(t, dst, profile.Harness)

				text := historyText(t, dstHarness, resp.ConvID, profile.Cwd)
				for _, want := range []string{morphSecret, "⟦tool call", "README.md\nmain.go", "There are two files."} {
					assert.Contains(t, text, want)
				}
				require.Len(t, morphNotes(t, resp.ConvID), 1)
				if dst == harness.CopilotName {
					c := f.World.Copilots.GetByConvID(resp.ConvID)
					require.NotNil(t, c)
					blocked, on := c.Blocked()
					assert.False(t, blocked, "trust_dir pre-trusted the directory; the pane is not parked on %s", on)
				}
			})
		}
	}
}
