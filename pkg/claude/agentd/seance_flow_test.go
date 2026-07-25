package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

func TestSeancePlan_PrunedExactGenerationDoesNotRedirect(t *testing.T) {
	f := newFlow(t)
	const (
		first  = "deadbeef-1111-2222-3333-444444444444"
		middle = "feedface-1111-2222-3333-444444444444"
		head   = "cafebabe-1111-2222-3333-444444444444"
	)
	firstCwd := f.TestCwd("seance-pruned-first")
	f.HaveConvWithTitle(first, "first")
	f.HaveAliveSession(first, "first-label", "first-tmux", firstCwd)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", first)
	f.HaveConvWithTitle(middle, "middle")
	f.HaveAliveSession(middle, "middle-label", "middle-tmux", f.TestCwd("seance-pruned-middle"))
	_, err := db.RotateAgentConv(first, middle, "reincarnate")
	require.NoError(t, err)
	f.HaveConvWithTitle(head, "head")
	f.HaveAliveSession(head, "head-label", "head-tmux", f.TestCwd("seance-pruned-head"))
	_, err = db.RotateAgentConv(middle, head, "reincarnate")
	require.NoError(t, err)
	require.NoError(t, db.DeleteConvIndex(first), "prune first generation from cache")

	for _, selector := range []string{first, first[:8]} {
		got, status, body := requestSeancePlan(t, f, head, map[string]any{
			"target": selector,
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "selector=%q body=%s", selector, body)
		assert.Equal(t, first, got.Predecessor,
			"exact pruned generation must not redirect to head then walk back to middle")
		assert.True(t, got.Exact)
	}
}

func TestSeancePlan_OpenCodeExactGenerationDoesNotRedirect(t *testing.T) {
	f := newFlow(t)
	const (
		first  = "ses_alpha111111111111111111111111"
		middle = "ses_bravo222222222222222222222222"
		head   = "ses_charlie3333333333333333333333"
	)
	firstCwd := f.TestCwd("seance-opencode-first")
	for _, generation := range []struct {
		id, title, cwd string
	}{
		{id: first, title: "open-first", cwd: firstCwd},
		{id: middle, title: "open-middle", cwd: f.TestCwd("seance-opencode-middle")},
		{id: head, title: "open-head", cwd: f.TestCwd("seance-opencode-head")},
	} {
		f.HaveConvWithTitle(generation.id, generation.title)
		f.HaveAliveSession(generation.id, generation.title+"-label", generation.title+"-tmux", generation.cwd)
		row, err := db.GetConvIndex(generation.id)
		require.NoError(t, err)
		require.NotNil(t, row)
		row.Harness = harness.OpenCodeName
		require.NoError(t, db.UpsertConvIndex(row))
		setSessionHarness(t, generation.id, harness.OpenCodeName)
	}
	f.HaveGroup("alpha")
	f.HaveMember("alpha", first)
	_, err := db.RotateAgentConv(first, middle, "reincarnate")
	require.NoError(t, err)
	_, err = db.RotateAgentConv(middle, head, "reincarnate")
	require.NoError(t, err)
	require.NoError(t, db.DeleteConvIndex(first), "prune first OpenCode generation from cache")

	for _, selector := range []string{first, first[:8]} {
		got, status, body := requestSeancePlan(t, f, head, map[string]any{"target": selector})
		require.Equal(t, http.StatusOK, status, "selector=%q body=%s", selector, body)
		assert.Equal(t, first, got.Predecessor)
		assert.Equal(t, harness.OpenCodeName, got.Harness)
		assert.Equal(t, firstCwd, got.Cwd)
		assert.True(t, got.Exact)
	}
}

func TestSeancePlan_RejectsUnboundedAndShortSelectors(t *testing.T) {
	f := newFlow(t)
	const (
		oldConv = "abcd1234-1111-2222-3333-444444444444"
		newConv = "9876fedc-1111-2222-3333-444444444444"
	)
	f.HaveConvWithTitle(oldConv, "old")
	f.HaveAliveSession(oldConv, "old-label", "old-tmux", f.TestCwd("bounded-old"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)
	f.HaveConvWithTitle(newConv, "new")
	f.HaveAliveSession(newConv, "new-label", "new-tmux", f.TestCwd("bounded-new"))
	_, err := db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)

	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{name: "short conversation prefix", body: map[string]any{"target": oldConv[:4]}, want: "at least 8"},
		{name: "target length", body: map[string]any{"target": strings.Repeat("x", 257)}, want: "too long"},
		{name: "back bound", body: map[string]any{"back": 129}, want: "between 1 and 128"},
		{name: "body bound", body: map[string]any{"target": strings.Repeat("x", 5<<10)}, want: "invalid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, status, body := requestSeancePlan(t, f, newConv, tc.body)
			assert.Equal(t, http.StatusBadRequest, status, "body=%s", body)
			assert.Contains(t, body, tc.want)
		})
	}

	t.Run("ambiguous exact prefix", func(t *testing.T) {
		const samePrefix = "abcd1234-aaaa-bbbb-cccc-dddddddddddd"
		f.HaveConvWithTitle(samePrefix, "same-prefix")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": oldConv[:8],
		})
		assert.Equal(t, http.StatusConflict, status, "body=%s", body)
		assert.Contains(t, body, "multiple generations")
	})

	t.Run("short agent name is not mistaken for a conversation prefix", func(t *testing.T) {
		f.HaveConvWithTitle(newConv, "ace")
		got, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": "ace",
			"back":   1,
		})
		require.Equal(t, http.StatusOK, status, "body=%s", body)
		assert.Equal(t, oldConv, got.Predecessor)
		assert.False(t, got.Exact)
	})

	t.Run("short OpenCode conversation prefix is rejected", func(t *testing.T) {
		const openCodeConv = "ses_alpha111111111111111111111111"
		f.HaveConvWithTitle(openCodeConv, "open-short-prefix")
		_, status, body := requestSeancePlan(t, f, newConv, map[string]any{
			"target": openCodeConv[:5],
		})
		assert.Equal(t, http.StatusBadRequest, status, "body=%s", body)
		assert.Contains(t, body, "at least 8")
	})
}
