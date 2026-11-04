package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type reapplyResult struct {
	AgentID string `json:"agent_id"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

func postReapply(t *testing.T, f *testharness.Flow, profile, agentID string) (int, reapplyResult, string) {
	t.Helper()
	rec := humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/"+profile+"/reapply", map[string]any{"agent_id": agentID})
	var out reapplyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// tcl-hxuwnc: Apply re-applies an edited profile to one agent at a time. A
// stopped agent takes the copy on its next launch, a fully idle running one is
// stopped, updated and resumed, and a busy one is skipped untouched. Fields a
// relaunch does not re-apply yet keep the agent's copied value, so the
// preview still lists them afterwards.
func TestProfileReapplyApply_StoppedIdleAndBusy(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("alpha")
	created := createProfile(t, f, map[string]any{
		"name": "worker", "harness": "claude", "ask_user_question_timeout": "5m", "startup_context": "Be brief.",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	stopped := spawnStopped(t, f, "alpha", map[string]any{"name": "stopped", "profile": "worker"})
	idle := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "idle", "profile": "worker"})
	require.Equalf(t, http.StatusOK, idle.Code, "spawn body=%s", idle.Raw)
	busy := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "busy", "profile": "worker"})
	require.Equalf(t, http.StatusOK, busy.Code, "spawn body=%s", busy.Raw)
	f.SetSessionStatus(idle.ConvID, session.StatusIdle)
	f.SetSessionStatus(busy.ConvID, session.StatusWorking)

	rec := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/worker", map[string]any{
		"name": "worker", "harness": "claude", "ask_user_question_timeout": "10m", "startup_context": "Be thorough.",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	preview := profileReapplyPreview(t, f, "worker")
	assert.True(t, preview.Apply.Enabled)
	busyBefore := relaunchProfileJSON(t, busy.ConvID)

	code, res, raw := postReapply(t, f, "worker", stopped.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "applied", res.Outcome)
	f.AssertResumeSpawned(f.AsHuman().Resume(stopped.ConvID))
	got, ok := f.World.SpawnAskTimeout(stopped.ConvID)
	require.True(t, ok)
	assert.Equal(t, "10m", got, "the next resume launches with the re-applied copy")

	code, res, raw = postReapply(t, f, "worker", idle.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "applied_restarted", res.Outcome)
	got, ok = f.World.SpawnAskTimeout(idle.ConvID)
	require.True(t, ok)
	assert.Equal(t, "10m", got, "the idle agent was restarted with the re-applied copy")

	code, res, raw = postReapply(t, f, "worker", busy.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "busy_skipped", res.Outcome)
	assert.Contains(t, res.Detail, "busy, skipped")
	assert.JSONEq(t, busyBefore, relaunchProfileJSON(t, busy.ConvID), "a busy agent is left untouched")

	// After the apply, only the not-yet field differs for the applied agents.
	after := profileReapplyPreview(t, f, "worker")
	for _, a := range after.Agents {
		keys := map[string]string{}
		for _, field := range a.Fields {
			keys[field.Key] = field.Status
		}
		if a.AgentID == busy.AgentID {
			assert.Equal(t, "changes", keys["ask_user_question_timeout"])
			continue
		}
		assert.Equal(t, map[string]string{"startup_context": "not reapplied yet"}, keys, a.AgentID)
	}
}

func TestProfileReapplyApply_Refusals(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "worker", "harness": "claude"}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "other", "harness": "claude"}).Code)
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})

	code, _, raw := postReapply(t, f, "other", spawn.AgentID)
	assert.Equal(t, http.StatusConflict, code, raw)
	assert.Contains(t, raw, "no_copy")
	code, _, _ = postReapply(t, f, "nope", spawn.AgentID)
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = postReapply(t, f, "worker", "agt_missing")
	assert.Equal(t, http.StatusNotFound, code)

	const caller = "reap-1111-2222-3333-5555"
	f.HaveMember("alpha", caller)
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/spawn-profiles/worker/reapply", map[string]any{"agent_id": spawn.AgentID})
	assert.Equal(t, http.StatusForbidden, rec.Code, "needs profiles.manage")

	ok, err := db.RetireAgentByID(spawn.AgentID, "test", "done")
	require.NoError(t, err)
	require.True(t, ok)
	code, _, raw = postReapply(t, f, "worker", spawn.AgentID)
	assert.Equal(t, http.StatusConflict, code, raw)
	assert.Contains(t, raw, "agent_retired")
}

// An agent caller re-applies only what it could do by morphing the target:
// profiles.manage alone is not enough, it needs agent.morph over the target,
// and the profile must not be operator-only. A disabled profile is refused
// for everyone.
func TestProfileReapplyApply_AgentCallerGates(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "worker", "harness": "claude"}).Code)
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "a", "profile": "worker"})
	const caller = "reap-1111-2222-3333-6666"
	f.HaveMember("alpha", caller)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermProfilesManage, "test"))
	reapply := func() (int, string) {
		rec := agentReq(t, f, caller, http.MethodPost, "/v1/spawn-profiles/worker/reapply", map[string]any{"agent_id": spawn.AgentID})
		return rec.Code, rec.Body.String()
	}
	code, raw := reapply()
	assert.Equal(t, http.StatusForbidden, code, "profiles.manage alone cannot touch another agent: %s", raw)

	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermAgentMorph, "test"))
	code, raw = reapply()
	require.Equal(t, http.StatusOK, code, raw)

	prof, err := db.GetSpawnProfile("worker")
	require.NoError(t, err)
	prof.OperatorOnly = true
	require.NoError(t, db.UpdateSpawnProfile(prof))
	code, raw = reapply()
	assert.Equal(t, http.StatusForbidden, code, raw)
	assert.Contains(t, raw, "operator")

	prof.OperatorOnly, prof.Disabled = false, true
	require.NoError(t, db.UpdateSpawnProfile(prof))
	code, _, raw = postReapply(t, f, "worker", spawn.AgentID)
	assert.NotEqual(t, http.StatusOK, code, "a disabled profile is refused, even for the operator: %s", raw)
}
