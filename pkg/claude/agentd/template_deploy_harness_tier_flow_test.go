package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// deployOneMember creates a single-member template and reinforces it into the
// group, returning that member's result. The roster is deliberately one agent:
// every scenario here is about which TIER decided a field, and a second member
// would only add rows to read past.
func deployOneMember(t *testing.T, f *testharness.Flow, group string, spec map[string]any) instantiateMemb {
	t.Helper()
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "team", "agents": []map[string]any{spec}}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": group})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	require.Emptyf(t, res.Agents[0].Error,
		"this helper is for arms about a member that LAUNCHES; a refusal here means the "+
			"scenario never reached the thing under test (kind=%q)", res.Agents[0].ErrorKind)
	require.NotNil(t, res.Agents[0].Resolved, "the deploy result must carry a launch echo at all")
	return res.Agents[0]
}

// TCL-1110. A template member that pins no harness must take the group default
// profile's, exactly as a direct spawn into that group does.
//
// Before this, resolveTemplateAgentLaunch always returned a non-blank harness
// (its own chain, falling back to `claude`), and applyDefaultProfile only fills
// the harness when the incoming one is BLANK — so the group tier was dead code
// on the deploy path. Two things went wrong at once and only one of them is
// about disclosure: the member launched on a different VENDOR than a direct
// spawn into the same group, and the echo said "harness default" — "nobody
// decided" — about a launch a group tier had been sitting right there for.
//
// The model assertion is not decoration: the harness gates every other field's
// validation, so while the deploy believed it was on Claude the Codex profile's
// model was skipped as foreign and the member launched with no model at all.
// Fixing the harness is what lets the rest of that profile through.
func TestTemplateDeploy_HarnessComesFromTheGroupDefaultProfile(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-high", "harness": "codex", "model": "gpt-5-codex", "effort": "high",
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "codex-high").Code,
		"the group default profile must actually be set, or this test proves nothing")

	// Pins nothing at all: the group default profile is the only tier that can
	// speak for this member.
	got := deployOneMember(t, f, "phoenix", map[string]any{"name": "worker", "role": "dev"})

	// POSITIVE CONTROL FIRST, on the durable record rather than the echo. An echo
	// naming a tier is worth nothing if that tier never reached the launch, and
	// "the deploy still made a Claude agent but now describes it as Codex" is a
	// worse outcome than the bug being fixed.
	rows, err := db.FindSessionsByConvID(got.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "no session row for the deployed member")
	assert.Equal(t, "codex", rows[0].Harness,
		"control: the member must actually have launched on the group profile's vendor")
	model, ok := f.World.SpawnModel(got.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", got.ConvID)
	assert.Equal(t, "gpt-5-codex", model,
		"control: with the harness right, the same profile's model is no longer skipped as foreign")

	assert.Equal(t, agent.ResolvedField{
		Value: "codex", Source: `group default profile "codex-high"`,
	}, got.Resolved.Harness,
		"the tier that chose the vendor is the one the operator can go and edit; "+
			"'harness default' names nobody")
	assert.Equal(t, "gpt-5-codex", got.Resolved.Model.Value,
		"the harness fix is what unblocks the same profile's MODEL: a codex member with "+
			"no model is the bug half-fixed, and it would look like a pass")
	assert.Equal(t, `group default profile "codex-high"`, got.Resolved.Model.Source,
		"and the model must be credited to the tier that supplied it, not to the "+
			"re-resolution that found it already set")

	// The disclosure condition on this change: a deploy that quietly moves a
	// member to another vendor has to say so on the surfaces an operator reads,
	// and AmbientDecisions is the filter both the CLI and the dashboard apply.
	assert.Contains(t, got.Resolved.AmbientDecisions(),
		`harness: codex (group default profile "codex-high")`,
		"a vendor chosen by a tier nobody typed at this deploy must survive onto the "+
			"rendered surfaces, or the change is silent where it matters")
}

// The parity claim itself, in one group: deploy and direct spawn must agree
// about the vendor. This is the assertion the ticket was opened on — the two
// paths producing different vendors from the same configuration — and it is
// deliberately written as an equality between the two echoes rather than as two
// separate expectations of "codex", so it keeps holding if the group tier's
// answer ever changes.
func TestTemplateDeploy_HarnessAgreesWithDirectSpawnIntoTheSameGroup(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-high", "harness": "codex", "model": "gpt-5-codex", "effort": "high",
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "codex-high").Code, "set group profile")

	deployed := deployOneMember(t, f, "phoenix", map[string]any{"name": "worker", "role": "dev"})

	// The same group, the same absence of any harness request, the other surface.
	spawned, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "phoenix", Name: "directly"})

	// The equality needs a floor under it. If the group tier ever stops reaching
	// BOTH surfaces, both echoes read claude/harness-default, the equality still
	// holds, and this arm goes on passing while demonstrating no parity at all —
	// the same vacuous pass a seed bug once produced in
	// TestComposeAgentRelaunchProfileCoversEveryField. So pin that the shared
	// answer is not the fallback before comparing the two. (Cold review of
	// TCL-1110, finding 4.)
	require.Equal(t, "codex", spawned.Resolved.Harness.Value,
		"control: the shared answer must be the group tier's, not the fallback both "+
			"surfaces would agree on if the tier reached neither")
	require.NotEqual(t, agent.ProvHarnessDefault, spawned.Resolved.Harness.Source,
		"and it must be credited to a tier, since 'harness default' on both sides is "+
			"exactly the agreement this equality would report while proving nothing")

	assert.Equal(t, spawned.Resolved.Harness.Value, deployed.Resolved.Harness.Value,
		"a group whose members' vendor depends on which surface created them is the defect; "+
			"deploy and spawn resolve the same chain or they disagree in production")
	assert.Equal(t, spawned.Resolved.Harness.Source, deployed.Resolved.Harness.Source,
		"and they must credit the same tier, or one of them is lying about who decided")
}

// The global default profile is the second ambient tier, and it reaches the
// deploy for the same reason. Covered separately because the two tiers are
// consulted by separate lookups: a fix that wired up only the group's would pass
// every assertion above and still leave a workspace-wide default unreachable
// from a deploy.
func TestTemplateDeploy_HarnessComesFromTheGlobalDefaultProfile(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-global", "harness": "codex", "model": "gpt-5-codex",
	}).Code, "create the profile the workspace will default to")
	require.Equalf(t, http.StatusOK, setGlobalProfile(t, f, "codex-global").Code, "set global profile")
	// No group default profile: the global tier is the only ambient one present.
	f.HaveGroup("phoenix")

	got := deployOneMember(t, f, "phoenix", map[string]any{"name": "worker", "role": "dev"})

	rows, err := db.FindSessionsByConvID(got.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "no session row for the deployed member")
	assert.Equal(t, "codex", rows[0].Harness, "control: the member launched on the global profile's vendor")

	assert.Equal(t, agent.ResolvedField{
		Value: "codex", Source: `global default profile "codex-global"`,
	}, got.Resolved.Harness,
		"the GLOBAL tier is a separate lookup from the group's: a fix wiring up only the "+
			"group tier passes every group arm in this file and leaves this one dead")
	assert.Contains(t, got.Resolved.AmbientDecisions(),
		`harness: codex (global default profile "codex-global")`,
		"and a workspace-wide default deciding a member's vendor must reach the rendered "+
			"surfaces exactly as a group-wide one does — an arm that exists to catch a "+
			"PARTIAL fix has to say which half is missing")
}

// The consequence worth writing down rather than discovering. executeSpawn has
// always run the operator's cross-harness spawn matrix over the RESOLVED harness
// for non-HTTP callers — it just never had a cross-vendor harness to check on
// this path, because the deploy always arrived holding `claude`. Now that the
// group tier can name another vendor, an AGENT-initiated deploy into a group
// whose policy forbids that edge is refused where it previously produced a
// Claude agent nobody asked for.
//
// That is the right outcome and the same one a direct spawn gets
// (TestSpawnHarnessPolicyChecksHarnessResolvedByDefaultProfile), but it turns a
// silent wrong-vendor success into a visible failure, so it is guarded here
// rather than left to be found in production. The refusal is per-member: the
// deploy itself still returns 201 and the rest of a roster is unaffected.
//
// A human deploy is deliberately NOT affected — the matrix exists to constrain
// agents holding groups.spawn and bypasses a caller with no conv-id — which is
// why every other test in this file, all human-initiated, deploys Codex happily.
func TestTemplateDeploy_GroupTierVendorIsSubjectToTheHarnessPolicy(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("phoenix")
	const lead = "lead-deploy-harness-policy-777777777777"
	f.HaveMember(g.Name, lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermTemplatesUse, "test"))

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-high", "harness": "codex", "model": "gpt-5-codex",
	}).Code, "create the profile the group will default to")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "codex-high").Code, "set group profile")
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: db.SpawnHarnessDeny, Reason: "profile may not cross the vendor boundary",
	}}))

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev"}},
		}).Code, "create template")

	// An agent caller must prove it can write the deploy cwd before the daemon
	// will launch anything there; Flow's own spawn helpers answer that challenge
	// transparently, and this raw agent-peer request has to do the same.
	body := map[string]any{"group_name": "phoenix", "cwd": t.TempDir()}
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/templates/team/reinforce", body), lead))
	if rec.Code == http.StatusForbidden {
		var challenge struct {
			WriteProof struct {
				Dirs     []string `json:"dirs"`
				Filename string   `json:"filename"`
				Token    string   `json:"token"`
			} `json:"write_proof"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &challenge), "body=%s", rec.Body.String())
		require.NotEmpty(t, challenge.WriteProof.Token, "unexpected 403: %s", rec.Body.String())
		for _, dir := range challenge.WriteProof.Dirs {
			require.NoError(t, os.WriteFile(filepath.Join(dir, challenge.WriteProof.Filename), nil, 0o600))
		}
		body["write_proof_token"] = challenge.WriteProof.Token
		rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/templates/team/reinforce", body), lead))
	}
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)
	assert.Equal(t, 1, res.Failed, "the member must be refused, not quietly launched on Claude")
	// WHICH refusal, not merely that one happened. Measured, not assumed: with the
	// ambient harness walk mutated out, this member still fails — on an unrelated
	// "no recorded launch approval posture" — so a bare Failed==1 would go red
	// under the mutation while proving nothing about the policy. The typed kind is
	// what makes the red mean what this test says it means.
	assert.Equalf(t, "cross_harness_spawn_denied", res.Agents[0].ErrorKind,
		"the refusal has to be the POLICY's: once the vendor stops crossing this member "+
			"still fails, on something else entirely, so a red here that does not name the "+
			"kind is a red about the wrong refusal (error=%q)", res.Agents[0].Error)
	assert.Contains(t, res.Agents[0].Error, "claude → codex",
		"the refusal has to name the edge the policy denied, or the operator cannot tell "+
			"WHICH vendor the group tier was trying to reach")
	assert.Contains(t, res.Agents[0].Error, "profile may not cross the vendor boundary",
		"the operator's OWN denial reason must reach the member that was refused, not "+
			"only the edge that refused it: a refusal quoting the policy back is one an "+
			"operator can act on, and an anonymous one sends them reading source")
}

// The other direction, which is what keeps this fix from being an override. A
// template member that names its own harness must KEEP it: the ambient tiers are
// the bottom of the chain, not the top. Without this, "the deploy now honours
// the group default profile" could be implemented as "the group default profile
// now wins", and every assertion above would still pass.
func TestTemplateDeploy_TemplatePinnedHarnessOutranksTheGroupDefaultProfile(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-high", "harness": "codex", "model": "gpt-5-codex", "effort": "high",
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "codex-high").Code, "set group profile")

	got := deployOneMember(t, f, "phoenix",
		map[string]any{"name": "worker", "role": "dev", "harness": "claude"})

	rows, err := db.FindSessionsByConvID(got.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "no session row for the deployed member")
	assert.Equal(t, "claude", rows[0].Harness,
		"control: the member the template pinned to Claude actually launched on Claude")

	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: agent.ProvExplicit},
		got.Resolved.Harness,
		"a harness the template names is a decision somebody typed into the template; "+
			"an ambient tier below it may not overturn it, and may not be credited for it")
	assert.NotContains(t, got.Resolved.AmbientDecisions(), "harness: claude",
		"and it is not an ambient decision, so it must not be announced as one")
}

// The failure this change makes reachable, and the disclosure it needs.
//
// Cold review of TCL-1110 reproduced it: a stored template that pins an inline
// Claude-only value — model, effort, sandbox mode, approval mode — no longer
// merely changes vendor in a Codex-defaulting group. It fails. Inline values go
// through resolveStringLaunchField's EXPLICIT branch, which is a hard 400 rather
// than the polite skip-with-note a referenced or role profile tier gets, so the
// roster loses that member outright. On the human path too.
//
// The message an operator got was `"opus" is a Claude Code model` about a
// template whose author never mentioned Codex, with nothing pointing at the
// group's default profile — because the resolver-failure branch recorded the
// message and no echo at all. That is this PR's own subject matter failing on
// the one path it did not cover: produced everywhere, rendered nowhere.
//
// Driven through the real endpoint rather than by handing a spawnFailure to the
// renderer. A test of the renderer alone would never execute the branch in
// waves.go that has to attach the echo, and would pass while the fix did nothing.
func TestTemplateDeploy_ResolverFailureNamesTheTierThatChoseTheHarness(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-high", "harness": "codex", "model": "gpt-5-codex",
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "codex-high").Code, "set group profile")

	// Pins a Claude model and NO harness: valid in this template until the group
	// tier started deciding the vendor.
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "team",
			"agents": []map[string]any{{"name": "worker", "role": "dev", "model": "opus"}},
		}).Code, "create template")

	// A worktree is configured deliberately. Without one, res.WorktreePath is
	// blank whatever the code does, and the assertion at the bottom of this test
	// would pass while the line it guards was deleted — measured, not assumed: a
	// mutation removing that line left this test green before the worktree was
	// added here.
	sharedWorktree := t.TempDir()
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/reinforce",
		map[string]any{"group_name": "phoenix", "worktree_path": sharedWorktree})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Len(t, res.Agents, 1)

	// POSITIVE CONTROL: the member really was refused, and refused for the reason
	// this test is about. Without it the echo assertions below could be describing
	// a member that spawned fine.
	require.Equal(t, 0, res.Spawned,
		"the inline Claude model must be REFUSED against the ambient tier's codex, not "+
			"quietly accepted — this arm is about a member the roster loses")
	require.Equal(t, 1, res.Failed,
		"and the loss must be reported per member rather than swallowed")
	got := res.Agents[0]
	require.Equal(t, "invalid_model", got.ErrorKind, "error=%q", got.Error)
	require.Contains(t, got.Error, "opus",
		"the refusal must quote the value it refused, or the operator cannot tell which "+
			"of a roster's pins is the one Codex will not take")

	require.NotNil(t, got.Resolved,
		"a member REFUSED because of an ambient tier is the case that most needs the "+
			"attribution, and it was the one case that had none")
	assert.Equal(t, agent.ResolvedField{
		Value: "codex", Source: `group default profile "codex-high"`,
	}, got.Resolved.Harness,
		"the failure says the model is wrong for codex; only the echo can say WHY this "+
			"deploy is on codex, and without it the operator is looking at a template that "+
			"never mentions Codex at all")
	assert.Contains(t, got.Resolved.AmbientDecisions(),
		`harness: codex (group default profile "codex-high")`,
		"and it must survive onto the rendered surfaces, or the explanation is on the "+
			"wire and on no screen")

	// A member that never spawned must not be described as owning a worktree.
	assert.Empty(t, got.WorktreePath,
		"a failed member has no agent, so a path attributed to it asserts a relationship "+
			"that was never formed — and %q was configured for this deploy, so a blank here "+
			"is the code choosing not to claim it rather than there being nothing to claim "+
			"(the worktree LEAK itself is TCL-1115)", sharedWorktree)
	assert.Empty(t, got.WorktreeBranch)
}

// The other thing this change newly opens, and the operator's standing rule
// says it can never be silent: an ambient profile can now put template-deployed
// members on the UNVERIFIED Copilot API drive.
//
// Before TCL-1110 the deploy resolved claude regardless, so a `harness: copilot`
// group default profile was a foreign default tier and its copilot_api was
// skipped. Now the harness comes from that tier and the drive follows from the
// same profile. The existing TCL-1090 guard pins `harness: copilot` in the
// TEMPLATE, so it does not cover this combination — the template here pins
// NOTHING, which is the whole point: the drive has to arrive because the AMBIENT
// tier named copilot.
//
// Durable record FIRST, echo second, deliberately. An echo crediting a tier that
// never reached the launch is worse than a missing one: a wrong attribution is a
// fabricated fact that survives review by looking like work. Echo-first would
// certify the disclosure rather than the thing disclosed.
func TestTemplateDeploy_AmbientTierCanAcquireTheCopilotAPIDrive(t *testing.T) {
	f := newFlow(t)
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-api-on", "harness": "copilot", "copilot_api": true,
	}).Code, "create the profile the group will default to")
	f.HaveGroup("phoenix")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "phoenix", "copilot-api-on").Code,
		"set group profile")

	got := deployOneMember(t, f, "phoenix", map[string]any{"name": "worker", "role": "dev"})

	profile, err := db.AgentRelaunchProfileForConv(got.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile, "no durable launch record for the deployed member at all")
	require.NotNil(t, profile.CopilotAPI,
		"the drive must be RECORDED as a decision: an unrecorded drive is one no relaunch "+
			"can reproduce and no snapshot can carry")
	require.True(t, *profile.CopilotAPI,
		"control: the member must actually be ON the unverified API drive — a disclosure "+
			"about an acquisition that did not happen certifies nothing")
	require.NotNil(t, profile.CopilotAPISource,
		"and recorded WITH its attribution, or the record says the drive was chosen and "+
			"cannot say by whom")
	assert.Equal(t, `group default profile "copilot-api-on"`, *profile.CopilotAPISource,
		"and the DURABLE record must name the tier that opened the door, since that record "+
			"is what a later snapshot and every relaunch read — an attribution wrong here "+
			"outlives the launch that made it")

	assert.Equal(t, agent.ResolvedField{
		Value: "copilot", Source: `group default profile "copilot-api-on"`,
	}, got.Resolved.Harness, "the ambient tier chose the vendor")
	assert.Equal(t, agent.ResolvedField{
		Value: "api", Source: `group default profile "copilot-api-on"`,
	}, got.Resolved.CopilotAPI,
		"and the drive rode in with it; no agent acquires the unverified drive silently, "+
			"whichever tier opened the door")
	assert.Contains(t, got.Resolved.AmbientDecisions(),
		`copilot drive: api (group default profile "copilot-api-on")`)
}
