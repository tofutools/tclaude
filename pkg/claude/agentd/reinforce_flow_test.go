package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-376: `POST /v1/templates/{name}/reinforce` — deploy a template's roster
// INTO an already-existing group instead of creating a fresh one (the
// reinforcements half of JOH-355). These flow tests drive the daemon's
// reinforce endpoint with the tmux/spawn simulators and assert at real
// surfaces: group membership before/after, the new members' inbox briefings,
// the UNCHANGED existing group row + owner set, the up-front collision /
// max-members / not-found refusals (nothing spawned), and rhythm/pattern
// scoping — plus a control proving the create-new instantiate path is
// unchanged.

// reinforceResult mirrors the JSON the reinforce endpoint returns — the
// instantiate shape plus the reinforce framing (reinforced + owner_note, and
// per-agent owner_dropped).
type reinforceResult struct {
	Group            string   `json:"group"`
	Template         string   `json:"template"`
	Spawned          int      `json:"spawned"`
	Failed           int      `json:"failed"`
	Reinforced       bool     `json:"reinforced"`
	OwnerNote        string   `json:"owner_note"`
	PatternDelivered int      `json:"pattern_delivered"`
	PatternErrors    []string `json:"pattern_errors"`
	RhythmsCreated   int      `json:"rhythms_created"`
	PendingWaves     int      `json:"pending_waves"`
	WavesTotal       int      `json:"waves_total"`
	Agents           []struct {
		Name         string   `json:"name"`
		FinalName    string   `json:"final_name"`
		ConvID       string   `json:"conv_id"`
		Owner        bool     `json:"owner"`
		OwnerDropped bool     `json:"owner_dropped"`
		Granted      []string `json:"granted"`
		Error        string   `json:"error"`
	} `json:"agents"`
}

// Scenario (happy path): a group already exists with one member. Reinforcing it
// with a 2-agent template spawns both INTO the group (membership grows from 1 to
// 3), the new members inherit the EXISTING group's context (not the template's),
// and the group row itself is left untouched.
func TestReinforce_IntoExistingGroup_AddsRosterAndInheritsGroupContext(t *testing.T) {
	f := newFlow(t)

	// A live group with a pre-existing member and its own distinctive context.
	f.HaveGroup("crew")
	const groupCtx = "CREW-HOUSE-RULES: ship small PRs."
	require.Equal(t, http.StatusOK,
		humanReq(t, f, http.MethodPatch, "/v1/groups/crew",
			map[string]any{"default_context": groupCtx}).Code, "set group context")
	const veteranConv = "11111111-aaaa-bbbb-cccc-000000000001"
	f.HaveMember("crew", veteranConv)
	require.Equal(t, 1, memberCount(t, "crew"), "one member before reinforcement")

	// A template whose OWN group-level context differs — it must be ignored in
	// reinforce mode (the roster is what's deployed, not the group-level fields).
	const tmplCtx = "TEMPLATE-CONTEXT-SHOULD-BE-IGNORED"
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":            "reinforcements",
			"default_context": tmplCtx,
			"agents": []templateAgentSpec{
				{Name: "scout", Role: "dev"},
				{Name: "sapper", Role: "dev"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew", "task": "hold the line"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res reinforceResult
	testharness.DecodeJSON(t, rec, &res)
	assert.True(t, res.Reinforced, "response framed as a reinforcement")
	assert.Equal(t, "crew", res.Group)
	assert.Equal(t, 2, res.Spawned, "both roster agents spawned")
	assert.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	// Membership grew by the roster; the veteran is still there.
	assert.Equal(t, 3, memberCount(t, "crew"), "1 veteran + 2 reinforcements")
	seen := map[string]bool{}
	for _, m := range f.ListGroupMembers("crew") {
		seen[m.ConvID] = true
	}
	assert.True(t, seen[veteranConv], "veteran member survives the reinforcement")

	// The new members' briefings carry the GROUP's context + the task, NOT the
	// template's group-level context.
	for _, a := range res.Agents {
		require.NotEmpty(t, a.ConvID, "agent %s spawned", a.Name)
		msgs, err := db.ListAgentMessagesForConv(a.ConvID, 100)
		require.NoError(t, err)
		joined := ""
		for _, m := range msgs {
			joined += m.Body + "\n"
		}
		assert.Contains(t, joined, groupCtx, "%s briefing carries the group's context", a.Name)
		assert.Contains(t, joined, "hold the line", "%s briefing carries the task", a.Name)
		assert.NotContains(t, joined, tmplCtx, "%s briefing ignores the template's group-level context", a.Name)
	}

	// The existing group ROW is untouched — reinforce never rewrites group-level
	// fields (its context stays the group's own, not overwritten with the task).
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, groupCtx, g.DefaultContext, "group default_context is NOT rewritten by reinforce")
	assert.Empty(t, g.SourceTemplate, "reinforce does not stamp source_template on the group")
}

// Scenario (collision refusal): a roster agent whose final "<group>-<agent>"
// name is already a live member 409s the WHOLE call up-front — nothing is
// spawned, membership is unchanged.
func TestReinforce_NameCollision_RefusesWholeCall(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	// A live member titled exactly "crew-scout" — the final name the roster's
	// "scout" agent would take.
	const collider = "22222222-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("crew", collider)
	f.HaveConvWithTitle(collider, "crew-scout")

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "scout", Role: "dev"}, // collides with crew-scout
				{Name: "sapper", Role: "dev"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusConflict, rec.Code, "collision should 409: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "crew-scout", "the colliding name is reported")

	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, memberCount(t, "crew"), "nothing spawned — only the pre-existing member remains")
}

// Scenario (max-members refusal): a roster that would push the group past its
// max_members cap 409s up-front — never partially spawned.
func TestReinforce_ExceedsMaxMembers_RefusesUpFront(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	const veteranConv = "33333333-aaaa-bbbb-cccc-000000000003"
	f.HaveMember("crew", veteranConv)
	// Cap at 2: 1 current + a 2-agent roster = 3 > 2 → refused.
	_, err := db.SetAgentGroupMaxMembers("crew", 2)
	require.NoError(t, err)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "scout", Role: "dev"},
				{Name: "sapper", Role: "dev"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusConflict, rec.Code, "over-cap should 409: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "max_members", "the refusal names the cap")

	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, memberCount(t, "crew"), "nothing spawned when the roster would exceed the cap")
}

// Scenario (owner-flag drop): reinforcing with a template that marks an agent as
// owner does NOT transfer ownership of the existing group — the flag is dropped
// (reported), and the group's owner set is unchanged.
func TestReinforce_OwnerFlagDropped_NeverTransfersOwnership(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	// A pre-existing owner of the group.
	const ownerConv = "44444444-aaaa-bbbb-cccc-000000000004"
	f.HaveMember("crew", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "captain", Role: "lead", IsOwner: true}, // template marks it owner
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res reinforceResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 1, res.Spawned)
	require.Len(t, res.Agents, 1)
	assert.False(t, res.Agents[0].Owner, "the new agent is NOT made an owner")
	assert.True(t, res.Agents[0].OwnerDropped, "the dropped-owner flag is reported per-agent")
	assert.NotEmpty(t, res.OwnerNote, "the response carries a human-renderable owner note")
	agentd.WaitForBackgroundForTest()

	// The group's owner set is unchanged — still just the original owner.
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	require.Len(t, owners, 1, "still exactly one owner")
	assert.Equal(t, ownerConv, owners[0].ConvID, "the original owner is still the sole owner")
	// The new captain is a member but not an owner.
	isOwner, err := db.IsAgentGroupOwner(g.ID, res.Agents[0].ConvID)
	require.NoError(t, err)
	assert.False(t, isOwner, "the reinforcement captain did not become an owner")
}

// Scenario (rhythm + pattern scoping): reinforce a template that carries a
// seeded rhythm and a kick-off work pattern. The rhythm materializes as a group
// cron job; the work-pattern step routes to the NEWLY-spawned member (the
// pre-existing member is not a template-roster name, so it is not targeted).
func TestReinforce_RhythmsAndWorkPattern_FireScopedToNewMembers(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	const veteranConv = "55555555-aaaa-bbbb-cccc-000000000005"
	f.HaveMember("crew", veteranConv)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "scout", Role: "dev"},
			},
			"work_pattern": []map[string]string{
				{"send_to": "scout", "value": "recon the {{task}}"},
			},
			"rhythms": []map[string]any{
				{"name": "standup", "interval": "1h", "body": "status?"},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew", "task": "north ridge"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res reinforceResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 1, res.Spawned)
	assert.Equal(t, 1, res.PatternDelivered, "the work-pattern step delivered")
	assert.Empty(t, res.PatternErrors, "no work-pattern errors")
	assert.Equal(t, 1, res.RhythmsCreated, "the seeded rhythm materialized")
	agentd.WaitForBackgroundForTest()

	scoutConv := res.Agents[0].ConvID
	require.NotEmpty(t, scoutConv)

	// The kick-off routed to the new member with {{task}} interpolated.
	msgs, err := db.ListAgentMessagesForConv(scoutConv, 100)
	require.NoError(t, err)
	joined := ""
	for _, m := range msgs {
		joined += m.Body + "\n"
	}
	assert.Contains(t, joined, "recon the north ridge", "work pattern reached the new member, interpolated")

	// The rhythm materialized as a group cron job named "<group>-<rhythm>".
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	found := false
	for _, j := range jobs {
		if j.Name == "crew-standup" {
			found = true
		}
	}
	assert.True(t, found, "the seeded rhythm became the group cron job crew-standup")
}

// Scenario (not-found): reinforcing a group that does not exist is a 404 (unlike
// instantiate/deploy, where a TAKEN name is the conflict).
func TestReinforce_MissingGroup_Is404(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "reinforcements", "agents": []templateAgentSpec{{Name: "scout", Role: "dev"}}}).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "no-such-group"})
	require.Equalf(t, http.StatusNotFound, rec.Code, "missing group should 404: %s", rec.Body.String())
}

// Scenario (cwd fallback): with no --cwd, the new members spawn in the group's
// OWN default_cwd. Proven via the resolve error path — a group whose default_cwd
// points at a non-existent directory makes a no-cwd reinforce fail with
// invalid_cwd (resolveSpawnCwd rejects the missing dir). If the handler did NOT
// fall back to g.DefaultCwd, an empty cwd would resolve to "" and succeed — so
// the 400 uniquely proves the fallback happened.
func TestReinforce_NoCwd_FallsBackToGroupDefaultCwd(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	require.Equal(t, http.StatusOK,
		humanReq(t, f, http.MethodPatch, "/v1/groups/crew",
			map[string]any{"default_cwd": "/nonexistent/joh376-reinforce-cwd"}).Code, "set a (non-existent) group default cwd")

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "reinforcements", "agents": []templateAgentSpec{{Name: "scout", Role: "dev"}}}).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"}) // no cwd → falls back to the group's default
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"no-cwd reinforce should resolve the GROUP's default cwd and reject the missing dir: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "does not exist", "the missing group default cwd is the rejected one")
}

// Scenario (single-wave reinforce IS allowed during a pending choreography):
// only a MULTI-wave reinforce is refused while a wave choreography is in flight
// (it would clobber the row); a single-wave reinforce writes no choreography, so
// it is fine — and the pending choreography is left intact.
func TestReinforce_SingleWaveWhileChoreographyPending_Allowed(t *testing.T) {
	f := newFlow(t)

	// A group with a pending choreography (two-wave deploy, wave 1 not yet up).
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "staged",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead", Wave: 0},
				{Name: "dev", Role: "dev", Wave: 1},
			},
		}).Code, "create staged template")
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/staged/deploy",
			map[string]any{"group_name": "raid", "mission": "m"}).Code, "deploy staged")
	agentd.WaitForBackgroundForTest()
	g, err := db.GetAgentGroupByName("raid")
	require.NoError(t, err)
	before, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, before, "raid has a pending choreography")

	// A single-wave reinforce is allowed alongside it.
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "one-shot", "agents": []templateAgentSpec{{Name: "medic", Role: "dev"}}}).Code)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/one-shot/reinforce",
		map[string]any{"group_name": "raid"})
	require.Equalf(t, http.StatusCreated, rec.Code, "single-wave reinforce should be allowed: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	// The original choreography is untouched — the single-wave reinforce wrote none.
	after, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, after, "the pending choreography survives a single-wave reinforce")
	assert.Equal(t, "staged", after.TemplateName, "still the original staged choreography, not clobbered")
}

// Scenario (multi-wave reinforce): a two-wave roster reinforces an existing
// group — wave 0 spawns synchronously, wave 1 via the background runner once
// wave 0 settles. Owner suppression holds across BOTH waves (carried on the
// choreography), and the deferred work pattern still routes only to the new
// members once the roster is whole.
func TestReinforce_MultiWave_SuppressesOwnerAcrossWavesAndDefersPattern(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("crew")
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	const ownerConv = "66666666-aaaa-bbbb-cccc-000000000006"
	f.HaveMember("crew", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "reinforcements",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead", IsOwner: true, Wave: 0},
				{Name: "dev", Role: "dev", Wave: 1},
			},
			// A step to the wave-1 dev proves the pattern is deferred until the
			// roster is whole.
			"work_pattern": []map[string]string{{"send_to": "dev", "value": "build it"}},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcements/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	var res reinforceResult
	testharness.DecodeJSON(t, rec, &res)
	assert.True(t, res.Reinforced)
	assert.Equal(t, 1, res.Spawned, "only wave 0 spawns synchronously")
	assert.Equal(t, 1, res.PendingWaves, "wave 1 deferred")
	require.Len(t, res.Agents, 1)
	assert.True(t, res.Agents[0].OwnerDropped, "wave-0 lead's owner flag dropped")
	agentd.WaitForBackgroundForTest()

	// Wave 0 (the lead) is up; the veteran + lead = 2 members so far.
	assert.Equal(t, 2, memberCount(t, "crew"))
	leadConv := memberByRole(t, "crew", "lead")
	require.NotEmpty(t, leadConv)

	// Settle wave 0 → the runner spawns wave 1 (the dev).
	settleWaveMember(t, f, leadConv)
	assert.Equal(t, 3, memberCount(t, "crew"), "veteran + lead + dev after wave 1")

	// Ownership never transferred across either wave — the original owner is
	// still the sole owner.
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	require.Len(t, owners, 1, "still exactly one owner after both waves")
	assert.Equal(t, ownerConv, owners[0].ConvID)

	// The deferred work pattern delivered to the new dev once the roster was whole.
	devConv := memberByRole(t, "crew", "dev")
	require.NotEmpty(t, devConv)
	msgs, err := db.ListAgentMessagesForConv(devConv, 100)
	require.NoError(t, err)
	joined := ""
	for _, m := range msgs {
		joined += m.Body + "\n"
	}
	assert.Contains(t, joined, "build it", "deferred work pattern reached the wave-1 dev")
}

// Scenario (in-flight guard): a multi-wave reinforce is refused while the group
// already has a pending wave choreography — the choreography table is keyed by
// group, so a second one would clobber the first.
func TestReinforce_MultiWaveWhileChoreographyPending_Refused(t *testing.T) {
	f := newFlow(t)

	// Deploy a two-wave template to create a fresh group WITH a pending
	// choreography (wave 1 not yet spawned).
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "staged",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead", Wave: 0},
				{Name: "dev", Role: "dev", Wave: 1},
			},
		}).Code, "create staged template")
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/staged/deploy",
			map[string]any{"group_name": "raid", "mission": "m"}).Code, "deploy staged")
	agentd.WaitForBackgroundForTest()
	g, err := db.GetAgentGroupByName("raid")
	require.NoError(t, err)
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, c, "raid has a pending choreography")

	// Now try to reinforce raid with ANOTHER multi-wave template → refused.
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "more-waves",
			"agents": []templateAgentSpec{
				{Name: "scout", Role: "dev", Wave: 0},
				{Name: "sapper", Role: "dev", Wave: 1},
			},
		}).Code, "create second multi-wave template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/more-waves/reinforce",
		map[string]any{"group_name": "raid"})
	require.Equalf(t, http.StatusConflict, rec.Code, "multi-wave reinforce over pending choreography should 409: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "staged deploy still in flight", "the refusal explains the in-flight choreography")

	// The original choreography is intact — not clobbered.
	c2, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, c2, "the original choreography survives the refused reinforce")
	assert.Equal(t, "staged", c2.TemplateName, "still the original staged choreography")
}

// Scenario (control — create-new path unchanged): a plain instantiate still
// creates a FRESH group and applies the template owner flag (suppressOwner is
// false on that path). Proves the reinforce fork did not alter create-new
// behaviour at the same surfaces.
func TestReinforce_CreateNewPath_Unchanged(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":            "fresh-force",
			"default_context": "FRESH-CTX",
			"agents": []templateAgentSpec{
				{Name: "boss", Role: "lead", IsOwner: true},
			},
		}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/fresh-force/instantiate",
		map[string]any{"group_name": "brandnew", "task": "kickoff"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res reinforceResult
	testharness.DecodeJSON(t, rec, &res)
	assert.False(t, res.Reinforced, "instantiate response is NOT framed as a reinforcement")
	assert.Empty(t, res.OwnerNote, "no owner-drop note on the create-new path")
	require.Len(t, res.Agents, 1)
	assert.True(t, res.Agents[0].Owner, "the template owner flag IS applied on create-new")
	assert.False(t, res.Agents[0].OwnerDropped, "nothing dropped on create-new")
	agentd.WaitForBackgroundForTest()

	// A fresh group was created and carries the template's context.
	g, err := db.GetAgentGroupByName("brandnew")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Contains(t, g.DefaultContext, "FRESH-CTX", "create-new stores the template's context on the fresh group")
	assert.Equal(t, "fresh-force", g.SourceTemplate, "create-new stamps source_template")
}
