package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func haveSavedPrompts(t *testing.T, f *testharness.Flow, prompts map[string]string) {
	t.Helper()
	for name, body := range prompts {
		rec := humanReq(t, f, http.MethodPost, "/v1/saved-prompts", map[string]any{"name": name, "body": body})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
}

// An explicit prompt set composes the brief in order, ahead of the typed
// initial message.
func TestSpawnPromptRefs_ComposeInitialMessage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	haveSavedPrompts(t, f, map[string]string{"cold-review": "Review the diff cold.", "ship-it": "Open a PR when done."})

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "prompt_refs": []string{"ship-it", "cold-review"}, "initial_message": "Fix the flaky test.",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	body := soleInboxMessage(t, spawn.ConvID).Body
	ship, review, brief := strings.Index(body, "Open a PR when done."), strings.Index(body, "Review the diff cold."), strings.Index(body, "Fix the flaky test.")
	require.True(t, ship >= 0 && review > ship && brief > review, "prompts in order, then the brief: %s", body)

	missing := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "other", "prompt_refs": []string{"nope"}})
	assert.Equal(t, http.StatusBadRequest, missing.Code)
	assert.Contains(t, string(missing.Raw), "nope")
}

// The group's default prompts apply when the spawn names none; a profile's
// prompts replace them, and an explicit empty set opts out of both.
func TestSpawnPromptRefs_ProfileAndGroupDefaults(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	haveSavedPrompts(t, f, map[string]string{"group-rules": "Follow the group rules.", "kit-rules": "Follow the kit rules."})
	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_prompts": []string{"group-rules"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusBadRequest, humanReq(t, f, http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_prompts": []string{"missing"}}).Code)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.Equal(t, []string{"group-rules"}, g.DefaultPrompts)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "kit", "prompt_refs": []string{"kit-rules"}}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "plain"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, soleInboxMessage(t, spawn.ConvID).Body, "Follow the group rules.")

	spawn = f.AsHuman().SpawnWith("alpha", map[string]any{"name": "kitted", "profile": "kit"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	body := soleInboxMessage(t, spawn.ConvID).Body
	assert.Contains(t, body, "Follow the kit rules.")
	assert.NotContains(t, body, "Follow the group rules.")

	spawn = f.AsHuman().SpawnWith("alpha", map[string]any{"name": "bare", "prompt_refs": []string{}, "initial_message": "Just this."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.NotContains(t, soleInboxMessage(t, spawn.ConvID).Body, "Follow the group rules.")
}

// A rename follows every reference; deletion is refused while one remains.
func TestSavedPromptRefs_RenameAndDeleteGuard(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	haveSavedPrompts(t, f, map[string]string{"rules": "The rules."})
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "kit", "prompt_refs": []string{"rules"}}).Code)
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_prompts": []string{"rules"}}).Code)

	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/saved-prompts/rules", map[string]any{"name": "house-rules", "body": "The rules."}).Code)
	p, err := db.GetSpawnProfile("kit")
	require.NoError(t, err)
	assert.Equal(t, []string{"house-rules"}, p.PromptRefs)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	assert.Equal(t, []string{"house-rules"}, g.DefaultPrompts)

	rec := humanReq(t, f, http.MethodDelete, "/v1/saved-prompts/house-rules", nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "kit")
	assert.Contains(t, rec.Body.String(), "alpha")
}
