package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A template-deployed member that pins nothing can be put on the Copilot API
// drive by its GROUP DEFAULT profile. The drive is unverified, and the operator's
// standing rule is that no agent acquires it silently — so the deploy result has
// to say so, and has to name the tier that decided.
//
// This became reachable more often with TCL-1090: before it, a from-group
// snapshot pinned an un-chosen `copilot_api: false` into the template-local
// profile, which outranks the group tier and suppressed exactly this. Removing
// that spurious suppression is correct and is what makes the disclosure load-
// bearing rather than theoretical.
//
// Scope note: the template deploy result carries NO resolved-field echo for any
// field (TCL-1097) — the direct spawn path renders one and this path does not.
// The note asserted here is the narrow stopgap for the one field where silence
// is unacceptable, and should be subsumed when that general channel lands.
//
// Written against the real endpoints with a positive control on the durable
// record, because the investigation behind it produced two confident wrong
// answers from setups that never reproduced the scenario: setting a group
// default profile before the group exists returns no error and does nothing.
func TestTemplateDeploy_GroupDefaultCopilotAPI_IsDisclosedAndNamesTheTier(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api-on", "harness": "copilot", "copilot_api": true,
	}).Code, "create the profile the group will default to")

	// The group must EXIST before it can carry a default profile.
	f.HaveGroup("phoenix")
	updated, err := db.SetAgentGroupDefaultProfile("phoenix", "copilot-api-on")
	require.NoError(t, err)
	require.Equal(t, int64(1), updated,
		"the group default profile must actually be set, or this test proves nothing")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "team",
			// Pins no copilot_api of its own: the group default is the only tier
			// that can speak for it.
			"agents": []map[string]any{{"name": "worker", "role": "dev", "harness": "copilot"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Len(t, res.Agents, 1)
	got := res.Agents[0]
	require.Empty(t, got.Error)

	// POSITIVE CONTROL FIRST. Without it, an absent note is indistinguishable
	// from a scenario where the group default never applied — which is exactly
	// how this investigation went wrong twice.
	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.CopilotAPI)
	require.True(t, *profile.CopilotAPI,
		"the member must actually have landed on the API drive, or there is nothing to disclose")
	require.NotNil(t, profile.CopilotAPISource)
	require.Equal(t, `group default profile "copilot-api-on"`, *profile.CopilotAPISource)

	assert.Contains(t, got.Notes, `copilot_api: api (group default profile "copilot-api-on")`,
		"an agent acquiring the UNVERIFIED API drive from a tier nobody typed at this deploy "+
			"must be disclosed, and must name the tier — a note that says only that something "+
			"happened leaves the operator with nowhere to go turn it off")
}

// A template that PINS the drive (via a referenced Copilot profile) must be
// disclosed too, and must name that profile.
//
// Found by cold review, and it is the more dangerous half: the group-default
// tier reaches the spawn boundary with its Set bit clear, so the boundary
// resolves it and produces a real source. A template-tier pin arrives with the
// Set bit already true, which the boundary reads as an EXPLICIT request — so
// before this was fixed the durable record blamed "explicit" (a tier nobody can
// go turn off) and the disclosure suppressed itself, because explicit means
// "the caller asked for this and does not need telling".
func TestTemplateDeploy_TemplatePinnedCopilotAPI_IsDisclosedAndNamesTheProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api-pin", "harness": "copilot", "copilot_api": true,
	}).Code, "create the profile the template references")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "team",
			"agents": []map[string]any{
				{"name": "worker", "role": "dev", "spawn_profile": "copilot-api-pin"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	got := res.Agents[0]
	require.Empty(t, got.Error)

	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.CopilotAPI)
	require.True(t, *profile.CopilotAPI, "control: the member is on the API drive")
	require.NotNil(t, profile.CopilotAPISource)
	assert.Equal(t, `profile "copilot-api-pin"`, *profile.CopilotAPISource,
		"the record must name the profile that chose the drive, not 'explicit' — an "+
			"attribution nobody can act on is the same as no attribution")

	assert.Contains(t, got.Notes, `copilot_api: api (profile "copilot-api-pin")`,
		"a template-pinned drive is still an acquisition the deploying operator did not type")
}

// A template-deployed member whose ssh_workaround is purely the harness default
// must not be recorded as having CHOSEN it.
//
// Also from cold review. resolveTemplateAgentLaunch force-sets the Set bit for
// the harness default, and the spawn boundary reads an already-set field as an
// explicit request — so without the attribution threaded alongside, every
// template-deployed agent positively asserted a decision nobody made. That is
// worse than the ambiguity this ticket set out to remove.
func TestTemplateDeploy_DefaultedSSHWorkaroundIsNotRecordedAsChosen(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev", "harness": "codex"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	require.Empty(t, res.Agents[0].Error)

	profile, err := db.AgentRelaunchProfileForConv(res.Agents[0].ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.SSHWorkaroundSource)
	assert.Equal(t, "harness default", *profile.SSHWorkaroundSource,
		"nobody chose this member's ssh_workaround; recording 'explicit' would make a "+
			"later from-group snapshot pin it as a curated decision")
}

// The other arm: an ordinary send-keys deploy says nothing. Without this, a note
// emitted unconditionally would satisfy the test above while training the
// operator to ignore the one that matters.
func TestTemplateDeploy_SendKeysMemberIsNotDisclosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev", "harness": "copilot"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	got := res.Agents[0]
	require.Empty(t, got.Error)

	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.CopilotAPI)
	require.False(t, *profile.CopilotAPI, "control: this member is on send-keys")

	for _, note := range got.Notes {
		assert.NotContains(t, note, "copilot_api",
			"send-keys is the known-good path and the default; disclosing it on every "+
				"ordinary deploy is how the disclosure that matters gets skipped")
	}
}
