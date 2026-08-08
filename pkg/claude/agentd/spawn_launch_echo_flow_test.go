package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A value the safety-net overlay resolved differently from the handler's
// snapshot must be reported with the TIER THAT DECIDED IT, not with the
// anonymous "default profile (applied at launch)" label.
//
// Found by cold review. `fast_mode: "inherit"` is the reachable case — the
// dashboard sends it on every Codex spawn whose Fast-mode row is left alone —
// because the spelling normalizes to "" with the Set bit CLEAR, so the overlay
// re-reads the profile tiers and a group default legitimately wins. Whatever the
// cause of the difference, an operator told "a default profile did it" with no
// name has nowhere to go and turn it off.
//
// (That the explicit `inherit` is overridden at all is a separate, pre-existing
// launch bug — TCL-1109. This guard is about what the response SAYS, and it is
// written so it keeps passing when that bug is fixed: a fixed launch reports the
// handler's own attribution and never enters the relabel branch.)
func TestSpawnEcho_LateFillNamesTheTierRatherThanAnonymousLaunchDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-fast", "harness": "codex", "fast_mode": true,
	}).Code, "create the profile the group will default to")
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "crew", "codex-fast").Code)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "fast_mode": "inherit",
	})
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

	var wire struct {
		Resolved agent.ResolvedLaunch `json:"resolved"`
	}
	require.NoError(t, json.Unmarshal(resp.Raw, &wire))

	// POSITIVE CONTROL: the echo has to be describing the launch that happened.
	profile, err := db.AgentRelaunchProfileForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	if profile.FastMode == nil || !*profile.FastMode {
		t.Skip("the group default no longer reaches this launch — TCL-1109 fixed; " +
			"the relabel branch this guards is unreachable for fast_mode")
	}

	assert.Equal(t, "on", wire.Resolved.FastMode.Value,
		"the echo must report the value that actually reached the spawner")
	assert.NotEqual(t, agent.ProvLaunchDefault, wire.Resolved.FastMode.Source,
		"an anonymous 'applied at launch' names no tier the operator can edit")
	assert.Equal(t, agent.ProvGroupProfileSource("codex-fast"), wire.Resolved.FastMode.Source,
		"it must name the profile that decided")
}

// A group default profile whose value is SKIPPED because it targets another
// harness must be disclosed on the deploy path, exactly as it already is on the
// direct spawn path.
//
// Found by cold review: applyDefaultProfile discarded every resolver note, so a
// deployed member reported "harness default" for a model its group default
// profile had supplied and had been rejected for — indistinguishable, from the
// outside, from a tier that never spoke at all.
func TestTemplateDeploy_EchoDisclosesASkippedDefaultProfileValue(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-kit", "harness": "codex", "model": "gpt-5.1-codex",
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	updated, err := db.SetAgentGroupDefaultProfile("phoenix", "codex-kit")
	require.NoError(t, err)
	require.Equal(t, int64(1), updated, "the default profile must actually be set")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "team",
			// A Claude member: the Codex profile's model cannot apply to it, so the
			// group tier is consulted, rejected, and must say so.
			"agents": []map[string]any{{"name": "worker", "role": "dev", "harness": "claude"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	got := res.Agents[0]
	require.Empty(t, got.Error)
	require.NotNil(t, got.Resolved)

	// POSITIVE CONTROL: the skip really happened — the member launched with no
	// model, not with the Codex one.
	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.ModelID)
	require.Empty(t, *profile.ModelID,
		"control: the foreign model must have been skipped, or there is nothing to disclose")

	require.NotEmpty(t, got.Resolved.Notes,
		"a consulted-and-rejected group default profile must not resolve to silence")
	joined := ""
	for _, note := range got.Resolved.Notes {
		joined += note + "\n"
	}
	assert.Contains(t, joined, `group default profile "codex-kit"`,
		"the disclosure must name the tier that was consulted")
	assert.Contains(t, joined, "model",
		"and the field it was consulted for")
}

// Sandbox warnings must arrive in the echo's Warnings channel, not flattened
// into Notes.
//
// Also cold review. The deploy resolver flattened both into Notes because it had
// nowhere else to put them; with an echo it does, and the renderer this ticket
// added would otherwise print "this agent runs commands unattended with no OS
// sandbox" under a `note:` label — styled as one more provenance footnote.
func TestTemplateDeploy_SandboxWarningsRideTheirOwnChannel(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev", "harness": "claude"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	require.Empty(t, res.Agents[0].Error)
	require.NotNil(t, res.Agents[0].Resolved)
	echo := res.Agents[0].Resolved

	// CONTROL: this deploy must actually produce a warning, or the assertion
	// below passes on an empty set and proves nothing.
	require.NotEmpty(t, echo.Warnings,
		"an unattended Claude launch with no OS sandbox must warn — if this is empty "+
			"the posture check no longer fires here and this guard is inert")
	for _, note := range echo.Notes {
		assert.NotContains(t, note, "unattended",
			"a blast-radius warning in the Notes channel gets rendered as a provenance "+
				"footnote: %q", note)
	}
}
