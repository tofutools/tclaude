package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// readProfile GETs one spawn profile back through the daemon, so a test can
// assert what the save boundary actually stored (as opposed to what it was
// handed).
func readProfile(t *testing.T, f *testharness.Flow, name string) map[string]any {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profiles/"+name, nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code, "read profile %s body=%s", name, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Scenario: on a 1M-context model Claude Code auto-compacts near the top of the
// window, well after answer quality has started to slide. Pinning
// CLAUDE_CODE_AUTO_COMPACT_WINDOW lower makes compaction fire while the agent is
// still sharp. These pin the daemon's resolution at the Spawner boundary
// (World.SpawnAutoCompactWindow — the same surface the auto-memory /
// remote-control spawn flow tests assert). The env-var rendering itself is unit
// tested in session.ApplyAutoCompactWindowEnv, and the parsing in
// harness.ParseAutoCompactWindow.

// TestClaudeSpawn_AutoCompactWindowDefaultsUnset: the load-bearing default. A
// plain spawn pins nothing, so Claude Code's own per-model threshold decides and
// tclaude injects no variable at all.
func TestClaudeSpawn_AutoCompactWindowDefaultsUnset(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	got, ok := f.World.SpawnAutoCompactWindow(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Empty(t, got,
		"a plain spawn must pin no auto-compaction window, leaving the model default in charge")
}

// TestClaudeSpawn_AutoCompactWindowNormalizes: the operator's shorthand reaches
// the launch as a canonical token count, so every layer below compares one form.
func TestClaudeSpawn_AutoCompactWindowNormalizes(t *testing.T) {
	for _, spelling := range []string{"450000", "450k", "0.45M"} {
		t.Run(spelling, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("cc-crew")

			resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
				"name":                "long-runner",
				"auto_compact_window": spelling,
			})
			require.Equal(t, 200, resp.Code,
				"auto_compact_window %q on a Claude Code spawn must be accepted; body=%s", spelling, resp.Raw)

			got, ok := f.World.SpawnAutoCompactWindow(resp.ConvID)
			require.True(t, ok, "the spawn should have been observed by the sim spawner")
			assert.Equal(t, "450000", got,
				"%q must reach the launch as a canonical token count", spelling)
		})
	}
}

// TestClaudeSpawn_AutoCompactWindowFromProfile: a profile's window fills a spawn
// that said nothing — the same tier behaviour effort / ask-timeout use. This is
// the path the operator actually uses: pin it once on a profile, get it on every
// agent spawned from it.
func TestClaudeSpawn_AutoCompactWindowFromProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "long-lived", "harness": "claude", "auto_compact_window": "450k",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "inherits-window", "profile": "long-lived",
	})
	require.Equal(t, 200, resp.Code, "spawn with profile; body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoCompactWindow(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "450000", got, "a profile's auto-compaction window must reach the launch")
}

// TestClaudeSpawn_ExplicitAutoCompactWindowOverridesProfile: the spawn form is
// what decides. An explicit per-spawn value beats the profile's.
func TestClaudeSpawn_ExplicitAutoCompactWindowOverridesProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "windowed", "harness": "claude", "auto_compact_window": "450k",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "wider", "profile": "windowed", "auto_compact_window": "600k",
	})
	require.Equal(t, 200, resp.Code, "spawn with profile; body=%s", resp.Raw)

	got, ok := f.World.SpawnAutoCompactWindow(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "600000", got, "an explicit per-spawn window must beat the profile's")
}

// TestClaudeSpawn_AutoCompactWindowRejectsBadValues: a typo or an out-of-range
// token count is a 400 at the boundary rather than a silently dropped flag.
func TestClaudeSpawn_AutoCompactWindowRejectsBadValues(t *testing.T) {
	for _, bad := range []string{"lots", "-450000", "1", "4500000000"} {
		t.Run(bad, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("cc-crew")

			resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
				"name": "bad-window", "auto_compact_window": bad,
			})
			assert.Equal(t, http.StatusBadRequest, resp.Code,
				"auto_compact_window %q must be rejected, not silently dropped; body=%s", bad, resp.Raw)
		})
	}
}

// TestCodexSpawn_AutoCompactWindowRejected: the window is a Claude Code
// variable. Requesting one for a harness that has no such knob must surface at
// the spawn boundary instead of vanishing at runtime — the same contract
// auto_memory and context_features apply.
func TestCodexSpawn_AutoCompactWindowRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cx-crew")

	resp := f.AsHuman().SpawnWith("cx-crew", map[string]any{
		"name": "codex-worker", "harness": "codex", "auto_compact_window": "450k",
	})
	assert.Equal(t, http.StatusBadRequest, resp.Code,
		"a Codex spawn must reject an auto-compaction window; body=%s", resp.Raw)
}

// TestProfile_AutoCompactWindowRoundTripsNormalized: the profile editor's
// shorthand is stored canonically, so the dashboard reads back one form no
// matter how the operator typed it.
func TestProfile_AutoCompactWindowRoundTripsNormalized(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "shorthand", "harness": "claude", "auto_compact_window": "0.5M",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	got := readProfile(t, f, "shorthand")
	assert.Equal(t, "500000", got["auto_compact_window"],
		"the profile must store the canonical token count, not the operator's shorthand")
}

// TestProfile_CodexAutoCompactWindowRejected: a Codex profile cannot carry a
// Claude Code window, for the same reason a Codex spawn cannot request one.
func TestProfile_CodexAutoCompactWindowRejected(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "codex-windowed", "harness": "codex", "auto_compact_window": "450k",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a Codex profile must reject an auto-compaction window; body=%s", rec.Body.String())
}
