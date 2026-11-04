package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-sqs8sr: an edited spawn profile reaches new spawns only. Saving it says
// how many running agents keep their copy, and the re-apply preview shows,
// per agent, what a re-apply would change. Applying is not offered yet.

type reapplyPreview struct {
	Profile string `json:"profile"`
	Agents  []struct {
		AgentID string `json:"agent_id"`
		ConvID  string `json:"conv_id"`
		Tier    string `json:"tier"`
		Fields  []struct {
			Key    string `json:"key"`
			From   string `json:"from"`
			To     string `json:"to"`
			Status string `json:"status"`
		} `json:"fields"`
		Grants []struct {
			Slug string `json:"slug"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"grants"`
	} `json:"agents"`
	Apply struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	} `json:"apply"`
	GrantsNote string `json:"grants_note"`
}

func profileReapplyPreview(t *testing.T, f *testharness.Flow, name string) reapplyPreview {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/spawn-profiles/"+name+"/running-agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out reapplyPreview
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestProfileReapply_PreviewShowsPerAgentDiff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	created := createProfile(t, f, map[string]any{
		"name": "worker", "harness": "claude", "model": "opus",
		"peer_messaging": false, "auto_memory": false, "initial_message": "Start.",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	plain := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "plain", "profile": "worker"})
	require.Equalf(t, http.StatusOK, plain.Code, "spawn body=%s", plain.Raw)
	pinned := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "pinned", "profile": "worker", "peer_messaging": false})
	require.Equalf(t, http.StatusOK, pinned.Code, "spawn body=%s", pinned.Raw)
	retired := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "retired", "profile": "worker"})
	require.Equalf(t, http.StatusOK, retired.Code, "spawn body=%s", retired.Raw)
	ok, err := db.RetireAgentByID(retired.AgentID, "test", "done")
	require.NoError(t, err)
	require.True(t, ok)

	// Saving says how many running agents keep their copy.
	rec := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/worker", map[string]any{
		"name": "worker", "harness": "claude",
		"model": "sonnet", "peer_messaging": true, "auto_memory": true, "initial_message": "Start over.",
		"permission_overrides": map[string]any{"human.notify": map[string]any{"effect": "grant"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var saved struct {
		RunningAgentsWithCopy int `json:"running_agents_with_copy"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &saved))
	assert.Equal(t, 2, saved.RunningAgentsWithCopy, "the retired agent is not counted")

	preview := profileReapplyPreview(t, f, "worker")
	assert.False(t, preview.Apply.Enabled, "applying waits on relaunch reading the spawn copy")
	assert.Contains(t, preview.Apply.Reason, "tcl-an26ed")
	assert.Contains(t, preview.GrantsNote, "shown only, not applied")
	require.Len(t, preview.Agents, 2)

	statuses := map[string]map[string]string{}
	for _, a := range preview.Agents {
		assert.Equal(t, db.SpawnCopyTierNamed, a.Tier)
		got := map[string]string{}
		for _, field := range a.Fields {
			got[field.Key] = field.Status
		}
		statuses[a.AgentID] = got
		require.Len(t, a.Grants, 1, "the profile's new grant is listed")
		assert.Equal(t, "human.notify", a.Grants[0].Slug)
		assert.Equal(t, "", a.Grants[0].From)
		assert.Equal(t, "grant", a.Grants[0].To)
	}
	assert.NotContains(t, statuses, retired.AgentID)
	assert.Equal(t, map[string]string{
		"model":           "kept (fixed)",
		"peer_messaging":       "changes",
		"auto_memory":     "changes",
		"initial_message": "not reapplied (spawn only)",
	}, statuses[plain.AgentID])
	assert.Equal(t, "kept (explicit)", statuses[pinned.AgentID]["peer_messaging"], "a value the spawn request set stays")
	assert.Equal(t, "changes", statuses[pinned.AgentID]["auto_memory"])

	assert.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodGet, "/v1/spawn-profiles/nope/running-agents", nil).Code)
}

func TestProfileReapply_NeedsProfilesManage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "worker", "harness": "claude"}).Code)
	const caller = "reap-1111-2222-3333-4444"
	f.HaveMember("alpha", caller)
	assert.Equal(t, http.StatusForbidden, agentReq(t, f, caller, http.MethodGet, "/v1/spawn-profiles/worker/running-agents", nil).Code)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermProfilesManage, "test"))
	assert.Equal(t, http.StatusOK, agentReq(t, f, caller, http.MethodGet, "/v1/spawn-profiles/worker/running-agents", nil).Code)
}
