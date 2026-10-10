package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type morphResp struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	Result  string `json:"result"`
	Before  struct {
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Approval string `json:"approval"`
	} `json:"before"`
	After struct {
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Approval string `json:"approval"`
		Profile  string `json:"profile"`
	} `json:"after"`
	PendingMorph *db.AgentPendingMorph `json:"pending_morph"`
	Authority    string                `json:"authority"`
}

func decodeMorph(t *testing.T, code int, raw []byte) morphResp {
	t.Helper()
	require.Equalf(t, http.StatusOK, code, "morph body=%s", raw)
	var resp morphResp
	require.NoError(t, json.Unmarshal(raw, &resp))
	return resp
}

func spawnMorphWorker(t *testing.T, f *testharness.Flow, group, name string, extra map[string]any) testharness.SpawnResp {
	t.Helper()
	body := map[string]any{"name": name, "model": "sonnet", "effort": "low"}
	for k, v := range extra {
		body[k] = v
	}
	spawn := f.AsHuman().SpawnWith(group, body)
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	return spawn
}

func morphNotes(t *testing.T, convID string) []*db.AgentMessage {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(convID, 50)
	require.NoError(t, err)
	var out []*db.AgentMessage
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, "Morphed") {
			out = append(out, m)
		}
	}
	return out
}

func TestMorphIdleAgentRelaunchesWithNewFormAndMorphsBack(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "worker", nil)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": "opus[1m]", "effort": "high"})
	resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "morphed", resp.Result)
	assert.Equal(t, "sonnet", resp.Before.Model)
	assert.Equal(t, "opus[1m]", resp.After.Model)
	assert.Equal(t, "high", resp.After.Effort)
	assert.Equal(t, spawn.AgentID, resp.AgentID)
	assert.Equal(t, spawn.ConvID, resp.ConvID, "a morph resumes the same conversation")

	model, _ := f.World.SpawnModel(spawn.ConvID)
	effort, _ := f.World.SpawnEffort(spawn.ConvID)
	assert.Equal(t, "opus[1m]", model, "the resume must launch the new model")
	assert.Equal(t, "high", effort)
	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, spawn.AgentID, agentID)
	members := f.ListGroupMembers("crew")
	require.Len(t, members, 1)
	assert.Equal(t, spawn.ConvID, members[0].ConvID)

	notes := morphNotes(t, spawn.ConvID)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0].Body, "Was: claude · sonnet · effort low")
	assert.Contains(t, notes[0].Body, "Now: claude · opus[1m] · effort high")
	assert.Contains(t, notes[0].Body, "identity, inbox, groups and permissions are unchanged")

	audit, err := db.ListAuditLog(db.AuditLogFilter{Verb: "morph"})
	require.NoError(t, err)
	require.NotEmpty(t, audit)
	assert.Contains(t, audit[0].Detail, "morphed")
	assert.Equal(t, spawn.AgentID, audit[0].TargetAgent)

	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"back": true})
	back := decodeMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "morphed", back.Result)
	assert.Equal(t, "sonnet", back.After.Model)
	assert.Equal(t, "low", back.After.Effort)
	model, _ = f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "sonnet", model, "--back restores the previous form")
	assert.Contains(t, morphNotes(t, spawn.ConvID)[0].Body, "morphed back")
}

func TestMorphBusyAgentWaitsForIdleAndIsVisible(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "busy", nil)

	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": "opus"})
	resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "pending", resp.Result)
	require.NotNil(t, resp.PendingMorph)
	assert.Equal(t, "operator", resp.PendingMorph.Actor)
	assert.True(t, f.World.Tmux.IsAlive(spawn.TmuxSession), "a pending morph must not stop the busy agent")

	peer := f.AsHuman().FindPeer(spawn.ConvID)
	require.NotNil(t, peer)
	require.NotNil(t, peer.PendingMorph, "agent ls must show the pending morph")
	assert.Equal(t, "opus", *peer.PendingMorph.Target.Model)
	snap := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	assert.Contains(t, snap.Body.String(), `"pending_morph"`, "the dashboard snapshot must show the pending morph")

	agentd.SweepPendingMorphsForTest()
	model, _ := f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "sonnet", model, "still busy: nothing applied yet")

	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	agentd.SweepPendingMorphsForTest()
	model, _ = f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "opus", model, "the watcher applies the morph once the agent is idle")
	pending, err := db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Nil(t, pending)
	require.Len(t, morphNotes(t, spawn.ConvID), 1)
}

func TestMorphNowStopsABusyAgent(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "stuck", nil)

	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"effort": "max", "now": true})
	resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "morphed", resp.Result)
	effort, _ := f.World.SpawnEffort(spawn.ConvID)
	assert.Equal(t, "max", effort)
}

func TestMorphCancelDropsPendingMorph(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "busy", nil)
	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": "opus"})
	require.Equal(t, "pending", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)

	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"cancel": true})
	assert.Equal(t, "cancelled", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	agentd.SweepPendingMorphsForTest()
	model, _ := f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "sonnet", model)
}

func TestMorphSelfNeedsSelfMorphAndWaitsForTurnEnd(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "selfie", nil)
	// Ownership must not confer a self-morph.
	require.NoError(t, db.AddAgentGroupOwner(g.ID, spawn.ConvID, "<test>"))
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	rec := f.AsAgent(spawn.ConvID).Morph("", map[string]any{"model": "opus"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	rec = f.AsAgent(spawn.ConvID).Morph(spawn.AgentID, map[string]any{"model": "opus"})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"groups.members.morph must never cover a self-target; body=%s", rec.Body.String())

	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermSelfMorph, "<test>"))
	rec = f.AsAgent(spawn.ConvID).Morph("", map[string]any{"model": "opus", "now": true})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a self-morph cannot use now; body=%s", rec.Body.String())

	rec = f.AsAgent(spawn.ConvID).Morph("", map[string]any{"model": "opus"})
	resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "pending", resp.Result, "a self-morph waits for the calling turn to end even when idle")
	agentd.SweepPendingMorphsForTest()
	model, _ := f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "opus", model)
	notes := morphNotes(t, spawn.ConvID)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0].Body, "by yourself")
}

func TestMorphOwnerScopeAndRefusals(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("crew")
	manager := spawnMorphWorker(t, f, "crew", "manager", map[string]any{"approval": "default"})
	worker := spawnMorphWorker(t, f, "crew", "worker", nil)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, manager.ConvID, "<test>"))
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "cheap", Harness: "claude", Model: "haiku", Effort: "low", Sandbox: "off"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "codexy", Harness: "codex", Model: "gpt-5.5"})
	require.NoError(t, err)

	asManager := f.AsAgent(manager.ConvID)
	rec := asManager.Morph(worker.AgentID, map[string]any{"profile": "cheap", "dry_run": true})
	dry := decodeMorph(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, "dry_run", dry.Result)
	assert.Equal(t, "haiku", dry.After.Model)
	assert.Equal(t, "cheap", dry.After.Profile)
	assert.Equal(t, agentd.PermGroupsMembersMorph, dry.Authority, "ownership confers groups.members.morph")

	for name, tc := range map[string]struct {
		body map[string]any
		code int
		want string
	}{
		"sandbox field":         {map[string]any{"sandbox": "off"}, http.StatusBadRequest, "not_morphable"},
		"cross-harness profile": {map[string]any{"profile": "codexy"}, http.StatusBadRequest, "cross_harness"},
		"bad model":             {map[string]any{"model": "no such model!"}, http.StatusBadRequest, "invalid_form"},
		"broader approval":      {map[string]any{"approval": "bypassPermissions"}, http.StatusForbidden, "approval_restricted"},
		"nothing to change":     {map[string]any{}, http.StatusBadRequest, "invalid_arg"},
	} {
		rec := asManager.Morph(worker.AgentID, tc.body)
		assert.Equalf(t, tc.code, rec.Code, "%s: body=%s", name, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), tc.want, "%s", name)
	}

	// A spawn_profile-scoped grant admits only morphs into those profiles.
	outsider := spawnMorphWorker(t, f, "crew", "outsider", nil)
	require.NoError(t, db.GrantAgentPermissionWithScope(outsider.ConvID, agentd.PermAgentMorph,
		`{"spawn_profile":["cheap"]}`, "<test>"))
	asOutsider := f.AsAgent(outsider.ConvID)
	rec = asOutsider.Morph(worker.AgentID, map[string]any{"model": "opus", "dry_run": true})
	assert.Equal(t, http.StatusForbidden, rec.Code, "free-form morph under a profile-scoped grant; body=%s", rec.Body.String())
	rec = asOutsider.Morph(worker.AgentID, map[string]any{"profile": "cheap", "dry_run": true})
	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	rec = asOutsider.Morph(worker.AgentID, map[string]any{"profile": "cheap", "model": "opus", "dry_run": true})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"explicit fields on top of a scoped profile are a free-form morph; body=%s", rec.Body.String())

	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "vip", Harness: "claude", Model: "opus", OperatorOnly: true})
	require.NoError(t, err)
	rec = asManager.Morph(worker.AgentID, map[string]any{"profile": "vip", "dry_run": true})
	assert.Equal(t, http.StatusForbidden, rec.Code, "operator-only profile; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "operator_only")

	// An agent may not replace or cancel a pending morph the operator requested.
	rec = f.AsHuman().Morph(worker.AgentID, map[string]any{"model": "opus"})
	require.Equal(t, "pending", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)
	rec = asManager.Morph(worker.AgentID, map[string]any{"model": "haiku"})
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	rec = asManager.Morph(worker.AgentID, map[string]any{"cancel": true})
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}

func TestMorphWatcherDropsExpiredAndStaleGenerations(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := spawnMorphWorker(t, f, "crew", "slow", nil)
	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": "opus"})
	require.Equal(t, "pending", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)

	pending, err := db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	pending.ExpiresAt = pending.RequestedAt.Add(-time.Minute)
	require.NoError(t, db.SetAgentPendingMorphForConv(spawn.ConvID, pending))
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	agentd.SweepPendingMorphsForTest()
	model, _ := f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "sonnet", model, "an expired pending morph must not apply")
	left, err := db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Nil(t, left)

	stale := *pending
	stale.ExpiresAt = time.Now().Add(time.Hour)
	stale.ConvID = "some-older-generation"
	require.NoError(t, db.SetAgentPendingMorphForConv(spawn.ConvID, &stale))
	agentd.SweepPendingMorphsForTest()
	model, _ = f.World.SpawnModel(spawn.ConvID)
	assert.Equal(t, "sonnet", model, "a pending morph for another generation must not apply")
	left, err = db.PendingMorphForAgent(spawn.AgentID)
	require.NoError(t, err)
	assert.Nil(t, left)
	audit, err := db.ListAuditLog(db.AuditLogFilter{Verb: "morph", Search: "dropped"})
	require.NoError(t, err)
	assert.Len(t, audit, 2)
}

func TestMorphOtherHarnessesResumeWithNewForm(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")

	for _, tc := range []struct{ harness, model, effort string }{
		{"codex", "gpt-5.4", "high"},
		{"copilot", "gpt-5.4", "high"},
	} {
		spawn := f.AsHuman().SpawnHarness("crew", tc.harness+"-worker", tc.harness)
		require.Equalf(t, http.StatusOK, spawn.Code, "%s spawn body=%s", tc.harness, spawn.Raw)
		f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
		rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"model": tc.model, "effort": tc.effort})
		resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
		assert.Equal(t, "morphed", resp.Result, tc.harness)
		model, _ := f.World.SpawnModel(spawn.ConvID)
		effort, _ := f.World.SpawnEffort(spawn.ConvID)
		assert.Equal(t, tc.model, model, tc.harness)
		assert.Equal(t, tc.effort, effort, tc.harness)
	}

	gemini := f.AsHuman().SpawnHarness("crew", "gemini-worker", "gemini")
	require.Equalf(t, http.StatusOK, gemini.Code, "gemini spawn body=%s", gemini.Raw)
	rec := f.AsHuman().Morph(gemini.AgentID, map[string]any{"effort": "high"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "Gemini has no effort; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_form")
}
