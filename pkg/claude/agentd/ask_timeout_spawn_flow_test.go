package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These flow tests cover the AskUserQuestion idle-timeout spawn path: a
// Claude-Code-only per-session settings.json override (never|60s|5m|10m),
// delivered as part of the merged `--settings` payload. They assert at the
// Spawner boundary (World.SpawnAskTimeout records the value the production
// spawn path threaded into `tclaude session new --ask-user-question-timeout`),
// the same surface the sandbox/approval/model flow tests use.

// TestSpawnAskTimeout_ExplicitThreadsThrough: a spawn that chooses an explicit
// AskUserQuestion timeout threads it through the spawn path unchanged, so the
// agent launches with that per-session override.
func TestSpawnAskTimeout_ExplicitThreadsThrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                      "agentic-worker",
		"ask_user_question_timeout": "5m",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "5m", got, "an explicit AskUserQuestion timeout must thread through unchanged")
}

// TestSpawnAskTimeout_DefaultOmits: a plain spawn (no timeout chosen) threads
// "" through — inherit — so the production spawner emits no override and the
// agent keeps the operator's own settings.json value. This is what keeps the
// "don't modify by default" promise: an un-chosen spawn changes nothing.
func TestSpawnAskTimeout_DefaultOmits(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "plain-worker")

	got, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "", got, `an un-chosen timeout must thread "" (inherit — production omits the override)`)
}

// TestSpawnAskTimeout_InvalidRejected: a value outside Claude Code's option set
// is a 400 at the spawn boundary rather than a bogus --settings payload.
func TestSpawnAskTimeout_InvalidRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                      "bad-worker",
		"ask_user_question_timeout": "30s", // not one of never|60s|5m|10m
	})
	require.Equal(t, http.StatusBadRequest, spawn.Code,
		"an invalid timeout must 400; body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "invalid_ask_user_question_timeout",
		"the refusal should name the timeout validation; body=%s", spawn.Raw)
}

// TestSpawnAskTimeout_RejectedForCodex: the AskUserQuestion timeout is a
// Claude-Code-only setting (Codex has no such dialog), so an explicit value on a
// Codex spawn is a 400, not a flag silently dropped.
func TestSpawnAskTimeout_RejectedForCodex(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	spawn := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":                      "codex-worker",
		"harness":                   "codex",
		"ask_user_question_timeout": "5m",
	})
	require.Equal(t, http.StatusBadRequest, spawn.Code,
		"an AskUserQuestion timeout on a Codex spawn must 400; body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "invalid_ask_user_question_timeout",
		"the refusal should name the timeout validation; body=%s", spawn.Raw)
}

// TestGroupDefaultProfile_AskTimeoutAppliedToSpawn: a group's default profile
// carries an AskUserQuestion timeout; a spawn that doesn't specify one inherits
// it — the per-profile agentic default the operator asked for.
func TestGroupDefaultProfile_AskTimeoutAppliedToSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "agentic").Code, "set default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	got, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "5m", got, "a blank-timeout spawn must inherit the profile's AskUserQuestion timeout")
}

// TestGroupDefaultProfile_AskTimeoutExplicitOverrides: an explicit timeout in
// the spawn request wins over the group's default profile — the profile only
// fills a blank field.
func TestGroupDefaultProfile_AskTimeoutExplicitOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "p5m", "ask_user_question_timeout": "5m"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "p5m").Code, "set default_profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                      "worker",
		"ask_user_question_timeout": "10m",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "10m", got, "an explicit timeout must override the profile's")
}

// TestSpawnProfile_AskTimeoutRejectedForCodexProfile: the profile save gate is
// harness-aware — an AskUserQuestion timeout on a Codex profile is a 400, the
// same Claude-only gate the spawn path applies.
func TestSpawnProfile_AskTimeoutRejectedForCodexProfile(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name":                      "cx-bad",
		"harness":                   "codex",
		"model":                     "gpt-5-codex",
		"ask_user_question_timeout": "5m",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an AskUserQuestion timeout on a Codex profile must 400; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_ask_user_question_timeout",
		"the refusal should name the timeout validation; body=%s", rec.Body.String())
}
