package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Regression coverage for the group-template dir-trust drop: a Codex agent
// spawned from a template that references a spawn profile with trust_dir=true
// must thread `--trust-dir` to the spawner, exactly as a plain group spawn does
// (handleGroupSpawn/applyDefaultProfile). Before the fix, resolveTemplateAgentLaunch
// carried only the five string launch fields, so the referenced profile's
// TrustDir *bool was dropped and the detached Codex pane froze on the
// trust-folder modal waiting for a human who was never there.
//
// The simulator spawns in-process (no real ~/.codex/config.toml write — that is
// the harness package's editor unit tests); what these scenarios pin is the
// PLUMBING through the template instantiator, captured via World.SpawnTrustDir.

// Scenario: a template whose Codex agent references a trust_dir=true profile
// instantiates so the agent is spawned WITH dir-trust threaded — the core fix.
func TestGroupTemplate_TrustDirFromProfileThreads(t *testing.T) {
	f := newFlow(t)

	// A Codex profile that opts into pre-trusting the launch dir.
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{
			"name": "trusting", "harness": "codex", "trust_dir": true,
		}).Code, "create profile")

	createBody := map[string]any{
		"name": "codex-team",
		"agents": []map[string]any{
			// Blank inline harness → adopts the profile's codex harness; the
			// profile's trust_dir must ride along.
			{"name": "cdx", "spawn_profile": "trusting"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/codex-team/instantiate",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned, "the codex agent spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	conv := res.Agents[0].ConvID
	require.NotEmpty(t, conv, "codex agent conv-id")
	got, ok := f.World.SpawnTrustDir(conv)
	require.Truef(t, ok, "no spawn recorded for conv %s", conv)
	assert.True(t, got, "a template profile's trust_dir=true must thread --trust-dir to the spawner")
}

// Scenario: the default — a template Codex agent whose referenced profile does
// NOT set trust_dir is never auto-trusted. Pins "never auto-defaulted" for the
// template path, the same guarantee the plain-spawn flow test makes.
func TestGroupTemplate_TrustDirOffByDefault(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "plain", "harness": "codex"}).Code, "create profile")

	createBody := map[string]any{
		"name":   "codex-team",
		"agents": []map[string]any{{"name": "cdx", "spawn_profile": "plain"}},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/codex-team/instantiate",
		map[string]any{"group_name": "atlas"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()

	conv := res.Agents[0].ConvID
	require.NotEmpty(t, conv)
	got, ok := f.World.SpawnTrustDir(conv)
	require.Truef(t, ok, "no spawn recorded for conv %s", conv)
	assert.False(t, got, "a template agent whose profile omits trust_dir must default off")
}
