package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type seancePlanView struct {
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
	Cwd         string `json:"cwd"`
	Hops        int    `json:"hops"`
	Requested   int    `json:"requested_back"`
	Exact       bool   `json:"exact"`
}

func requestSeancePlan(
	t *testing.T,
	f *testharness.Flow,
	caller string,
	body map[string]any,
) (*seancePlanView, int, string) {
	t.Helper()
	req := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/whoami/seance", body), caller)
	rec := testharness.Serve(f.Mux, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var out seancePlanView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out),
		"decode séance plan: %s", rec.Body.String())
	return &out, rec.Code, rec.Body.String()
}

// Production-path contract: the daemon resolves the caller's stable actor,
// walks the real succession table backward, and returns the dead generation's
// harness + cwd. The caller never needs direct access to ~/.tclaude/data.
func TestSeancePlan_DefaultAndSelectorsResolveThroughDaemon(t *testing.T) {
	f := newFlow(t)

	const (
		oldConv = "aaaabbbb-1111-2222-3333-444444444444"
		newConv = "ccccdddd-1111-2222-3333-444444444444"
	)
	oldCwd := f.TestCwd("seance-old")
	newCwd := f.TestCwd("seance-new")

	f.HaveConvWithTitle(oldConv, "worker-x")
	f.HaveAliveSession(oldConv, "seance-old-label", "seance-old-tmux", oldCwd)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "worker")
	f.HaveAliveSession(newConv, "seance-new-label", "seance-new-tmux", newCwd)
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err, "rotate actor old → new")

	agentID, err := db.AgentIDForConv(newConv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	t.Run("default self predecessor", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{"back": 1})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.Equal(t, "claude", got.Harness)
		assert.Equal(t, oldCwd, got.Cwd)
		assert.Equal(t, 1, got.Hops)
		assert.Equal(t, 1, got.Requested)
		assert.False(t, got.Exact)
	})

	t.Run("stable agent id walks from live head", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": agentID,
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("agent name walks from live head", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": "worker",
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("conversation prefix stays on exact dead generation", func(t *testing.T) {
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": oldConv[:8],
			"back":   99,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.True(t, got.Exact)
		assert.Zero(t, got.Hops)
	})

	t.Run("human may name an exact dead generation", func(t *testing.T) {
		req := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/whoami/seance", map[string]any{"target": oldConv}))
		rec := testharness.Serve(f.Mux, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got seancePlanView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, oldConv, got.Predecessor)
		assert.True(t, got.Exact)
	})

	t.Run("live exact generation is rejected", func(t *testing.T) {
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": newConv,
		})
		assert.Equal(t, http.StatusConflict, status, "body=%s", body)
		assert.Contains(t, body, "live generation")
	})

	t.Run("another actor is forbidden", func(t *testing.T) {
		const otherConv = "eeeeffff-1111-2222-3333-444444444444"
		f.HaveConvWithTitle(otherConv, "other-worker")
		f.HaveMember("alpha", otherConv)
		otherAgent, err := db.AgentIDForConv(otherConv)
		require.NoError(t, err)

		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": otherAgent,
		})
		assert.Equal(t, http.StatusForbidden, status, "body=%s", body)
		assert.Contains(t, body, "only with one of its own")
	})

	t.Run("removed worktree fails before subprocess planning", func(t *testing.T) {
		require.NoError(t, os.Remove(oldCwd), "remove predecessor startup dir")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{"back": 1})
		assert.Equal(t, http.StatusNotFound, status, "body=%s", body)
		assert.Contains(t, body, "grave is unreachable")
		assert.Contains(t, body, oldCwd)
	})
}

func TestSeancePlan_HumanMustNameTarget(t *testing.T) {
	f := newFlow(t)
	req := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/whoami/seance", map[string]any{"back": 1}))
	rec := testharness.Serve(f.Mux, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "must pass --target")
}

func TestSeancePlan_FirstGenerationGetsClearNotFound(t *testing.T) {
	f := newFlow(t)
	const conv = "11112222-3333-4444-5555-666677778888"
	f.HaveConvWithTitle(conv, "first-life")
	f.HaveAliveSession(conv, "first-life-label", "first-life-tmux", f.TestCwd("first-life"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", conv)

	_, status, body := requestSeancePlan(t, f, conv, map[string]any{"back": 1})
	assert.Equal(t, http.StatusNotFound, status, "body=%s", body)
	assert.Contains(t, body, "no predecessor")
}
