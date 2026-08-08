package agentd_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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
// TCL-1097 subsumed the narrow copilot_api-only note this used to assert into
// the general per-agent launch echo, so the assertion moved to the structured
// field rather than a string. The scenario, the positive control and the reason
// the disclosure has to name a tier are unchanged — only the channel is.
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

	require.NotNil(t, got.Resolved, "the deploy result must carry a launch echo at all")
	assert.Equal(t, agent.ResolvedField{
		Value: "api", Source: `group default profile "copilot-api-on"`,
	}, got.Resolved.CopilotAPI,
		"an agent acquiring the UNVERIFIED API drive from a tier nobody typed at this deploy "+
			"must be disclosed, and must name the tier — a disclosure that says only that "+
			"something happened leaves the operator with nowhere to go turn it off")
	assert.Contains(t, got.Resolved.AmbientDecisions(),
		`copilot drive: api (group default profile "copilot-api-on")`,
		"and it must survive the filter both renderers apply, or it is on the wire and on "+
			"no screen")
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

	require.NotNil(t, got.Resolved)
	assert.Equal(t, agent.ResolvedField{
		Value: "api", Source: `profile "copilot-api-pin"`,
	}, got.Resolved.CopilotAPI,
		"a template-pinned drive is still an acquisition the deploying operator did not "+
			"type, and the echo must name the PROFILE rather than the 'explicit' the spawn "+
			"boundary's re-resolution would otherwise stamp on it")
}

// The echo must attribute a template-tier value to that tier for EVERY field it
// renders, not just the Copilot drive.
//
// This is the general form of the bug above. The deploy resolves the template /
// role / referenced-profile tiers itself and then hands the resolved values to
// the spawn boundary as ordinary params; the boundary re-resolves them, finds
// them already set, and reports "explicit". Copilot's drive threaded its
// attribution through as a stopgap; model and effort did not, so a member whose
// model was chosen by a role's profile would report a tier the operator cannot
// go and change.
func TestTemplateDeploy_EchoNamesTheTemplateTierForEveryField(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "sonnet-high", "harness": "claude", "model": "sonnet", "effort": "high",
	}).Code, "create the profile the template references")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "team",
			"agents": []map[string]any{
				{"name": "worker", "role": "dev", "spawn_profile": "sonnet-high"},
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
	require.NotNil(t, got.Resolved)

	// POSITIVE CONTROL: the profile's values really did take effect. Without it
	// an echo naming the profile could be describing a tier that never applied.
	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.ModelID)
	require.Equal(t, "sonnet", *profile.ModelID, "control: the profile's model is the launch's")

	assert.Equal(t, agent.ResolvedField{Value: "sonnet", Source: `profile "sonnet-high"`},
		got.Resolved.Model, "the tier that chose the model is the one the operator can edit")
	assert.Equal(t, agent.ResolvedField{Value: "high", Source: `profile "sonnet-high"`},
		got.Resolved.Effort)
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: `profile "sonnet-high"`},
		got.Resolved.Harness, "the harness resolves on its own chain and must be attributed too")
}

// Every field the echo renders must name SOME tier. A blank source reads as
// "unknown" on a field that is always decided by somebody, and it is what a
// field added to ResolvedLaunch without a matching fill in resolveLaunchProvenance
// would produce — the failure this walks the struct to catch rather than
// re-listing the fields by hand, which is how the gap arose in the first place.
func TestTemplateDeploy_EveryEchoedFieldNamesATier(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("phoenix")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	require.Empty(t, res.Agents[0].Error)
	require.NotNil(t, res.Agents[0].Resolved)

	// Nobody chose this member's harness — not the template, not a profile. The
	// spawn boundary sees a resolved harness arriving in the params and would
	// call that "explicit", crediting an operator who typed nothing; the deploy
	// says "harness default" because it knows its own tiers came up empty.
	//
	// This RECORDS today's answer, it does not bless it: a deploy also bypasses
	// the group/global default profile's harness entirely (TCL-1110), so on a
	// group whose default profile pins Codex this reads "harness default" while a
	// direct spawn into the same group launches Codex. Fixing that changes which
	// vendor a deploy produces and belongs to that ticket; when it lands, this
	// expectation becomes the group tier.
	assert.Equal(t, agent.ProvHarnessDefault, res.Agents[0].Resolved.Harness.Source,
		"an attribution that names no tier the operator can change is worse than useless "+
			"— it is a false statement about who decided")

	echo := reflect.ValueOf(*res.Agents[0].Resolved)
	fieldType := reflect.TypeOf(agent.ResolvedField{})
	checked := 0
	for i := range echo.NumField() {
		if echo.Type().Field(i).Type != fieldType {
			continue
		}
		checked++
		field := echo.Field(i).Interface().(agent.ResolvedField)
		assert.NotEmptyf(t, field.Source,
			"%s is echoed with no tier: resolveLaunchProvenance has no attribution for it",
			echo.Type().Field(i).Name)
	}
	require.NotZero(t, checked, "the walk found no ResolvedField at all — it proves nothing")
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

	require.NotNil(t, got.Resolved)
	assert.Empty(t, got.Resolved.CopilotAPI.Value,
		"a send-keys launch has no drive to announce")
	for _, decision := range got.Resolved.AmbientDecisions() {
		assert.NotContains(t, decision, "copilot",
			"send-keys is the known-good path and the default; announcing it on every "+
				"ordinary deploy is how the announcement that matters gets skipped")
	}
}
