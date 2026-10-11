package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type crossMorphResp struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	Result  string `json:"result"`
	Before  struct {
		Harness string `json:"harness"`
		Model   string `json:"model"`
	} `json:"before"`
	After struct {
		Harness  string `json:"harness"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Approval string `json:"approval"`
		Sandbox  string `json:"sandbox"`
	} `json:"after"`
	History *transcript.LossReport `json:"history"`
}

func decodeCrossMorph(t *testing.T, code int, raw []byte) crossMorphResp {
	t.Helper()
	require.Equalf(t, http.StatusOK, code, "morph body=%s", raw)
	var resp crossMorphResp
	require.NoError(t, json.Unmarshal(raw, &resp))
	return resp
}

// seedClaudeHistory writes a short exchange with a tool call into a spawned
// Claude agent's native transcript, as the agent would have.
func seedClaudeHistory(t *testing.T, f *testharness.Flow, convID string) {
	t.Helper()
	cc := f.World.CCs.GetByConvID(convID)
	require.NotNil(t, cc, "the spawned agent has a Claude transcript")
	turn := func(typ, id, parent string, msg map[string]any) {
		require.NoError(t, cc.AppendTurn(map[string]any{"type": typ, "uuid": id, "parentUuid": parent, "cwd": cc.Cwd,
			"timestamp": "2026-10-01T10:00:00.000Z", "message": msg}))
	}
	turn("user", "u1", "", map[string]any{"role": "user", "content": morphSecret})
	turn("assistant", "a1", "u1", map[string]any{"id": "m1", "role": "assistant", "model": "claude-sonnet-5-5", "content": []any{
		map[string]any{"type": "thinking", "thinking": "private reasoning", "signature": "sig"},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}})
	turn("user", "u2", "a1", map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "README.md\nmain.go"}}})
	turn("assistant", "a2", "u2", map[string]any{"id": "m2", "role": "assistant", "model": "claude-sonnet-5-5", "content": []any{
		map[string]any{"type": "text", "text": "There are two files."}}})
	convops.ScanAndUpsertFile(cc.JsonlPath)
}

func TestMorphCrossHarnessClaudeToCodexAndBack(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "shifter", nil)
	seedClaudeHistory(t, f, spawn.ConvID)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	body := map[string]any{"harness": "codex", "model": "gpt-5.4", "effort": "high", "approval": "on-request"}
	dryBody := map[string]any{"dry_run": true}
	for k, v := range body {
		dryBody[k] = v
	}
	rec := f.AsHuman().Morph(spawn.AgentID, dryBody)
	dry := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "dry_run", dry.Result)
	assert.Equal(t, "claude", dry.Before.Harness)
	assert.Equal(t, "codex", dry.After.Harness)
	assert.Equal(t, "gpt-5.4", dry.After.Model)
	assert.Equal(t, "on-request", dry.After.Approval)
	require.NotNil(t, dry.History, "the dry run previews the history leg")
	assert.Equal(t, "native", dry.History.Fallback)
	assert.Equal(t, 1, dry.History.Count(transcript.LossReasoningDropped))
	assert.Equal(t, spawn.ConvID, dry.ConvID, "a dry run changes nothing")
	agent, err := db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Equal(t, spawn.ConvID, agent.CurrentConvID)

	rec = f.AsHuman().Morph(spawn.AgentID, body)
	resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", resp.Result)
	assert.Equal(t, spawn.AgentID, resp.AgentID)
	newConv := resp.ConvID
	require.NotEqual(t, spawn.ConvID, newConv, "a cross-harness morph continues in a new conversation")
	require.NotNil(t, resp.History)
	assert.Equal(t, "native", resp.History.Fallback)
	// The apply translates the history as it stands after the stop, so only
	// the content-derived counts are compared with the preview.
	for _, kind := range []transcript.LossKind{transcript.LossReasoningDropped, transcript.LossToolCallsFlattened} {
		assert.Equal(t, dry.History.Count(kind), resp.History.Count(kind), kind)
	}
	require.NotEmpty(t, resp.History.TranscriptPath)
	_, err = os.Stat(resp.History.TranscriptPath)
	assert.NoError(t, err, "the full transcript is kept")
	morphRoot := tclcommon.MorphArtifactsDir(spawn.AgentID)
	assert.True(t, strings.HasPrefix(resp.History.TranscriptPath, morphRoot+string(filepath.Separator)),
		"the transcript is kept in the agent's own agent-readable directory: %s", resp.History.TranscriptPath)
	assert.Equal(t, morphRoot, tclcommon.ExistingMorphArtifactsDir(spawn.AgentID),
		"later launches of the agent find the directory to reopen")
	profileForCwd, err := db.ConversationResumeProfileForConv(resp.ConvID)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(profileForCwd.Cwd, ".tclaude-morph"))
	assert.True(t, os.IsNotExist(err), "nothing is written into the agent's working directory")

	// Same agent, now on its Codex generation.
	agent, err = db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Equal(t, newConv, agent.CurrentConvID)
	id, err := db.AgentIDForConv(newConv)
	require.NoError(t, err)
	assert.Equal(t, spawn.AgentID, id)
	members := f.ListGroupMembers("crew")
	require.Len(t, members, 1)
	assert.Equal(t, newConv, members[0].ConvID)
	profile, err := db.ConversationResumeProfileForConv(newConv)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, harness.CodexName, profile.Harness)

	// Codex resumed the translated rollout natively with the new form.
	model, ok := f.World.SpawnModel(newConv)
	require.True(t, ok, "the Codex conversation was launched")
	assert.Equal(t, "gpt-5.4", model)
	effort, _ := f.World.SpawnEffort(newConv)
	assert.Equal(t, "high", effort)
	approval, _ := f.World.SpawnApproval(newConv)
	assert.Equal(t, "on-request", approval)
	require.NotNil(t, f.World.Codexes.GetByConvID(newConv), "the Codex sim resumed the translated rollout")
	codex := mustGetHarness(t, harness.CodexName)
	text := historyText(t, codex, newConv, profile.Cwd)
	for _, want := range []string{morphSecret, "⟦tool call · Bash⟧", "README.md\nmain.go", "There are two files."} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, text, "private reasoning")

	notes := morphNotes(t, newConv)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0].Body, "Was: claude · sonnet")
	assert.Contains(t, notes[0].Body, "Now: codex · gpt-5.4")
	assert.Contains(t, notes[0].Body, "History claude -> codex")
	assert.NotContains(t, notes[0].Body, "PELICAN", "the note carries counts, never transcript content")

	// Back across harnesses: a third generation, on Claude again, in the
	// form the agent had before.
	f.SetSessionStatus(newConv, session.StatusIdle)
	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"back": true})
	back := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", back.Result)
	assert.Equal(t, "claude", back.After.Harness)
	assert.Equal(t, "sonnet", back.After.Model)
	third := back.ConvID
	require.NotEqual(t, newConv, third)
	model, _ = f.World.SpawnModel(third)
	assert.Equal(t, "sonnet", model)
	claude := mustGetHarness(t, harness.DefaultName)
	assert.Contains(t, historyText(t, claude, third, profile.Cwd), morphSecret, "the history survives the round trip")
	assert.Contains(t, morphNotes(t, third)[0].Body, "morphed back")
}

func TestMorphCrossHarnessRefusals(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("crew")
	manager := spawnMorphWorker(t, f, "crew", "manager", map[string]any{"approval": "default"})
	worker := spawnMorphWorker(t, f, "crew", "worker", nil)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, manager.ConvID, "<test>"))
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "codexy", Harness: "codex", Model: "gpt-5.4", Approval: "on-request"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "codex-noapproval", Harness: "codex", Model: "gpt-5.4"})
	require.NoError(t, err)
	asManager := f.AsAgent(manager.ConvID)

	for name, tc := range map[string]struct {
		as   *testharness.Flow
		body map[string]any
		code int
		want string
	}{
		"agent free-form": {asManager, map[string]any{"harness": "codex", "model": "gpt-5.4", "approval": "on-request"},
			http.StatusForbidden, "profile_required"},
		"agent profile override": {asManager, map[string]any{"profile": "codexy", "effort": "high"},
			http.StatusForbidden, "profile_required"},
		"agent, harness-native sandbox": {asManager, map[string]any{"profile": "codexy"},
			http.StatusForbidden, "sandbox_restricted"},
		"operator without approval": {f.AsHuman(), map[string]any{"harness": "codex", "model": "gpt-5.4"},
			http.StatusBadRequest, "invalid_form"},
		"profile without approval": {f.AsHuman(), map[string]any{"profile": "codex-noapproval"},
			http.StatusBadRequest, "invalid_form"},
		"opencode needs provider/model": {f.AsHuman(), map[string]any{"harness": "opencode", "model": "gpt-5.4", "approval": "deny"},
			http.StatusBadRequest, "provider/model"},
		"harness and profile disagree": {f.AsHuman(), map[string]any{"harness": "gemini", "profile": "codexy"},
			http.StatusBadRequest, "invalid_arg"},
		"unknown harness": {f.AsHuman(), map[string]any{"harness": "nope", "approval": "x"},
			http.StatusBadRequest, "invalid_arg"},
	} {
		rec := tc.as.Morph(worker.AgentID, tc.body)
		assert.Equalf(t, tc.code, rec.Code, "%s: body=%s", name, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), tc.want, "%s", name)
	}
	agent, err := db.GetAgent(worker.AgentID)
	require.NoError(t, err)
	assert.Equal(t, worker.ConvID, agent.CurrentConvID, "no refusal moved the agent")
}

func TestMorphCrossHarnessPendingAppliesWhenIdle(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "busy", nil)
	seedClaudeHistory(t, f, spawn.ConvID)

	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"harness": "codex", "model": "gpt-5.4", "approval": "on-request"})
	resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "pending", resp.Result)
	require.NotNil(t, resp.History, "a pending cross-harness morph shows the history preview")
	pending, err := db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, "codex", pending.Target.Harness)

	agentd.SweepPendingMorphsForTest()
	agent, err := db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Equal(t, spawn.ConvID, agent.CurrentConvID, "still busy: nothing applied yet")

	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	agentd.SweepPendingMorphsForTest()
	agent, err = db.GetAgent(spawn.AgentID)
	require.NoError(t, err)
	require.NotEqual(t, spawn.ConvID, agent.CurrentConvID, "the watcher applied the cross-harness morph")
	model, _ := f.World.SpawnModel(agent.CurrentConvID)
	assert.Equal(t, "gpt-5.4", model)
	pending, err = db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Nil(t, pending)
	require.Len(t, morphNotes(t, agent.CurrentConvID), 1)
}

func TestMorphCrossHarnessAgentCarriesTclaudeLayer(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("crew")
	walled := map[string]any{"sandbox_implementation": "tclaude-layer"}
	manager := spawnMorphWorker(t, f, "crew", "manager", walled)
	worker := spawnMorphWorker(t, f, "crew", "worker", walled)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, manager.ConvID, "<test>"))
	seedClaudeHistory(t, f, worker.ConvID)
	f.SetSessionStatus(worker.ConvID, session.StatusIdle)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "codexy", Harness: "codex", Model: "gpt-5.4", Approval: "on-request"})
	require.NoError(t, err)

	rec := f.AsAgent(manager.ConvID).Morph(worker.AgentID, map[string]any{"profile": "codexy"})
	resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", resp.Result)
	codex := mustGetHarness(t, harness.CodexName)
	layerMode, err := harness.TclaudeLayerHarnessBuiltinMode(codex)
	require.NoError(t, err)
	assert.Equal(t, layerMode, resp.After.Sandbox, "Codex launches under tclaude's wall with its inner sandbox stood down")
	impl, _ := f.World.SpawnSandboxImplementation(resp.ConvID)
	assert.Equal(t, "tclaude-layer", impl, "tclaude's sandbox layer carries over")
	model, _ := f.World.SpawnModel(resp.ConvID)
	assert.Equal(t, "gpt-5.4", model)
}

// A form labelled with a profile authorizes a scoped --back, so a form with
// overrides on top of the profile must not carry the label.
func TestMorphCrossHarnessBackScopeNeedsAnExactProfileForm(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	worker := spawnMorphWorker(t, f, "crew", "worker", nil)
	outsider := spawnMorphWorker(t, f, "crew", "outsider", nil)
	seedClaudeHistory(t, f, worker.ConvID)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "cheap", Harness: "claude", Model: "haiku", Effort: "low"})
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermissionWithScope(outsider.ConvID, agentd.PermAgentMorph,
		`{"spawn_profile":["cheap"]}`, "<test>"))

	f.SetSessionStatus(worker.ConvID, session.StatusIdle)
	rec := f.AsHuman().Morph(worker.AgentID, map[string]any{"profile": "cheap", "effort": "max"})
	require.Equal(t, "morphed", decodeCrossMorph(t, rec.Code, rec.Body.Bytes()).Result)
	f.SetSessionStatus(worker.ConvID, session.StatusIdle)
	rec = f.AsHuman().Morph(worker.AgentID, map[string]any{"harness": "codex", "model": "gpt-5.4", "approval": "on-request"})
	moved := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", moved.Result)

	f.SetSessionStatus(moved.ConvID, session.StatusIdle)
	rec = f.AsAgent(outsider.ConvID).Morph(worker.AgentID, map[string]any{"back": true, "dry_run": true})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the previous form was cheap plus an override, which the cheap-scoped grant does not cover; body=%s", rec.Body.String())
}
