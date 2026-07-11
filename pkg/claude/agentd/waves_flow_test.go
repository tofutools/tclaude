package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-244 staged-spawn "waves" + seeded "rhythms". These flow tests drive the
// deploy endpoint with the spawn/tmux simulators and the synchronous wave
// runner (SweepWaveChoreographiesForTest), asserting at real surfaces: the
// deploy response, group membership (conv.ListSessions' backing table), the
// persisted choreography row, the materialized cron jobs, and the delivered
// agent_messages.

// waveDeployResult mirrors the deploy/instantiate response plus the JOH-244
// staged-spawn deferral fields.
type waveDeployResult struct {
	Group            string   `json:"group"`
	Spawned          int      `json:"spawned"`
	Failed           int      `json:"failed"`
	PatternDelivered int      `json:"pattern_delivered"`
	PatternErrors    []string `json:"pattern_errors"`
	RhythmsCreated   int      `json:"rhythms_created"`
	WavesTotal       int      `json:"waves_total"`
	PendingWaves     int      `json:"pending_waves"`
	PendingAgents    int      `json:"pending_agents"`
	ChoreographyNote string   `json:"choreography_note"`
	Agents           []struct {
		Name   string `json:"name"`
		ConvID string `json:"conv_id"`
	} `json:"agents"`
}

// memberByRole returns the live member of a group whose role matches, or "".
func memberByRole(t *testing.T, groupName, role string) string {
	t.Helper()
	g, err := db.GetAgentGroupByName(groupName)
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	for _, m := range members {
		if m.Role == role {
			return m.ConvID
		}
	}
	return ""
}

func memberCount(t *testing.T, groupName string) int {
	t.Helper()
	g, err := db.GetAgentGroupByName(groupName)
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	return len(members)
}

// settleWaveMember drives a wave member's live status through
// working → idle: the observed-working-then-idle signal the gate releases on
// (a freshly-spawned agent is already idle, so the gate must see it work
// first). One sweep per status flip advances the runner.
func settleWaveMember(t *testing.T, f *testharness.Flow, conv string) {
	t.Helper()
	f.SetSessionStatus(conv, session.StatusWorking)
	agentd.SweepWaveChoreographiesForTest() // observe "working" → mark activated
	f.SetSessionStatus(conv, session.StatusIdle)
	agentd.SweepWaveChoreographiesForTest() // observe activated + idle → release + spawn next
	agentd.WaitForBackgroundForTest()
}

// Scenario A: a template whose every agent is wave 0 spawns in ONE synchronous
// pass — no choreography row, work pattern delivered inline. The "zero behavior
// change for existing templates" guarantee.
func TestWaves_AllWaveZero_IsSynchronousToday(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "flat",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"work_pattern": []map[string]string{{"send_to": "all", "value": "kickoff"}},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/flat/deploy",
		map[string]any{"group_name": "flatteam", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, 2, res.Spawned, "both agents spawned in the single pass")
	assert.Equal(t, 0, res.PendingWaves, "no deferred waves")
	assert.Equal(t, 0, res.WavesTotal, "single-wave deploy reports no wave framing")
	assert.Equal(t, 2, res.PatternDelivered, "work pattern delivered inline (roster whole)")
	assert.Equal(t, 2, memberCount(t, "flatteam"), "both members present immediately")

	g, _ := db.GetAgentGroupByName("flatteam")
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c, "no choreography persisted for an all-wave-0 deploy")
}

// Scenario A2: a zero-agent template deploys without panicking (partitionWaves
// returns no waves — the empty-roster guard replaces the old for-range's
// graceful spawned:0).
func TestWaves_EmptyRoster_NoPanic(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "empty", "agents": []templateAgentSpec{}}).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/empty/deploy",
		map[string]any{"group_name": "hollow", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 0, res.Spawned, "no agents to spawn")
	assert.Equal(t, 0, memberCount(t, "hollow"), "group created, no members")

	g, _ := db.GetAgentGroupByName("hollow")
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c, "no choreography for an empty roster")
}

// Scenario B: a two-wave deploy spawns wave 0 synchronously, reports the
// deferral, holds wave 1 until wave 0 settles, then spawns wave 1 and delivers
// the (deferred) work pattern.
func TestWaves_TwoWave_DefersUntilPriorWaveIdle(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "staged",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true, Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
		// A work-pattern step to the dev proves the pattern is DEFERRED until
		// the dev (wave 1) is up — it can't deliver while the roster is partial.
		"work_pattern": []map[string]string{{"send_to": "dev", "value": "start building"}},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/staged/deploy",
		map[string]any{"group_name": "raid", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()

	// The response reports only wave 0 + the deferral summary.
	assert.Equal(t, 1, res.Spawned, "only wave 0 spawned synchronously")
	assert.Equal(t, 2, res.WavesTotal)
	assert.Equal(t, 1, res.PendingWaves)
	assert.Equal(t, 1, res.PendingAgents)
	assert.Equal(t, 0, res.PatternDelivered, "work pattern deferred — roster not whole")
	assert.NotEmpty(t, res.ChoreographyNote)

	assert.Equal(t, 1, memberCount(t, "raid"), "only the lead is up")
	assert.Empty(t, memberByRole(t, "raid", "dev"), "the dev has not spawned yet")

	leadConv := memberByRole(t, "raid", "lead")
	require.NotEmpty(t, leadConv)

	// A sweep BEFORE the lead has worked must NOT release the gate (a fresh
	// agent is idle-since-spawn — no beat yet).
	agentd.SweepWaveChoreographiesForTest()
	agentd.WaitForBackgroundForTest()
	assert.Empty(t, memberByRole(t, "raid", "dev"), "gate holds until the lead has its beat")

	// Drive the lead working → idle: the gate releases and wave 1 spawns.
	settleWaveMember(t, f, leadConv)

	devConv := memberByRole(t, "raid", "dev")
	require.NotEmpty(t, devConv, "wave 1 spawned once wave 0 settled")
	assert.Equal(t, 2, memberCount(t, "raid"))

	// The choreography row is gone (last wave landed).
	g, _ := db.GetAgentGroupByName("raid")
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c, "choreography deleted after the final wave")

	// The deferred work pattern delivered to the dev once it was up.
	msgs, err := db.ListAgentMessagesForConv(devConv, 100)
	require.NoError(t, err)
	joined := ""
	for _, m := range msgs {
		joined += m.Body + "\n"
	}
	assert.Contains(t, joined, "start building", "deferred work-pattern step delivered after wave 1")
}

// Scenario C: the max-wait cap releases the gate even if wave 0 never goes
// idle — a crashed/hung lead can't wedge the force forever.
func TestWaves_MaxWaitFallback_SpawnsNextWave(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "capped",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/capped/deploy",
		map[string]any{"group_name": "capgroup", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
	require.Empty(t, memberByRole(t, "capgroup", "dev"), "dev not up before the cap")

	// Force the current gate's deadline into the past (the lead is left in its
	// as-spawned state — never observed working), then sweep.
	g, _ := db.GetAgentGroupByName("capgroup")
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, c)
	c.WaveDeadline = time.Now().Add(-time.Minute)
	require.NoError(t, db.UpsertWaveChoreography(c))

	agentd.SweepWaveChoreographiesForTest()
	agentd.WaitForBackgroundForTest()

	assert.NotEmpty(t, memberByRole(t, "capgroup", "dev"), "max-wait released the gate; wave 1 spawned")
	c2, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c2, "choreography complete after the final wave")
}

// Scenario D: a dead (exited) wave-0 member does NOT wedge the gate — dead ≠
// busy, so the next wave proceeds without waiting on it.
func TestWaves_DeadMember_DoesNotWedgeGate(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "doomedlead",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/doomedlead/deploy",
		map[string]any{"group_name": "wreck", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	leadConv := memberByRole(t, "wreck", "lead")
	require.NotEmpty(t, leadConv)

	// The lead dies before ever going idle. The gate must treat it as settled.
	f.SetSessionStatus(leadConv, session.StatusExited)
	agentd.SweepWaveChoreographiesForTest()
	agentd.WaitForBackgroundForTest()

	assert.NotEmpty(t, memberByRole(t, "wreck", "dev"), "wave 1 spawned; a dead member didn't wedge the gate")
}

// A queued wave persists both the profile's display name and durable ID. If
// the profile is renamed while wave 0 is settling, wave 1 must resolve launch
// fields AND post-spawn owner/permission access through the ID; the stale name
// snapshot must not produce a partially configured agent.
func TestWaves_QueuedProfileAccessSurvivesRename(t *testing.T) {
	f := newFlow(t)
	owner := true
	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "before", Model: "haiku", IsOwner: &owner,
		PermissionOverrides: map[string]string{agentd.PermGroupsSpawn: db.PermEffectGrant},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "rename-between-waves",
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "wave": 0},
			{"name": "dev", "role": "dev", "wave": 1, "spawn_profile": "before"},
		},
	}).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/rename-between-waves/deploy",
		map[string]any{"group_name": "renamed-wave", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
	leadConv := memberByRole(t, "renamed-wave", "lead")
	require.NotEmpty(t, leadConv)

	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{
		ID: profileID, Name: "after", Model: "haiku", IsOwner: &owner,
		PermissionOverrides: map[string]string{agentd.PermGroupsSpawn: db.PermEffectGrant},
	}))
	settleWaveMember(t, f, leadConv)

	devConv := memberByRole(t, "renamed-wave", "dev")
	require.NotEmpty(t, devConv)
	spawnModel, ok := f.World.SpawnModel(devConv)
	require.True(t, ok)
	assert.Equal(t, "haiku", spawnModel, "queued launch resolved the renamed profile by ID")
	g, err := db.GetAgentGroupByName("renamed-wave")
	require.NoError(t, err)
	assert.True(t, ownsGroup(t, g.ID, devConv), "profile owner default survived the rename")
	overrides, err := db.ListAgentPermissionOverridesForConv(devConv)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, overrides[agentd.PermGroupsSpawn],
		"profile permission override survived the rename")
}

// Scenario F: restart idempotency — if the daemon crashed after spawning a wave
// but before persisting the advanced cursor, the persisted row still points at
// the just-spawned wave. The next sweep must NOT double-spawn it: an agent
// whose name is already a live member is reused, not re-spawned.
func TestWaves_RestartIdempotency_NoDuplicateWave(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "resume",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/resume/deploy",
		map[string]any{"group_name": "resumed", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	leadConv := memberByRole(t, "resumed", "lead")
	require.NotEmpty(t, leadConv)

	// Advance wave 1 (dev) up normally.
	settleWaveMember(t, f, leadConv)
	require.NotEmpty(t, memberByRole(t, "resumed", "dev"), "wave 1 spawned")
	require.Equal(t, 2, memberCount(t, "resumed"))

	// Simulate the crash-in-window: re-persist a choreography row that still
	// points at wave 1 as the pending wave (as if the advance were lost), with
	// its gate already released (past deadline). A naive runner would re-spawn
	// the dev; the idempotency guard must recognise the existing member.
	g, _ := db.GetAgentGroupByName("resumed")
	crashed := &db.WaveChoreography{
		GroupID:      g.ID,
		GroupName:    "resumed",
		TemplateName: "resume",
		Waves: []db.WaveGroup{
			{Wave: 0, Agents: []db.GroupTemplateAgent{{Name: "lead", Role: "lead", Wave: 0}}},
			{Wave: 1, Agents: []db.GroupTemplateAgent{{Name: "dev", Role: "dev", Wave: 1}}},
		},
		NextWave:     1,
		GatingConvs:  []string{leadConv},
		Activated:    []string{leadConv},
		SpawnedConvs: map[string]string{"lead": leadConv},
		SpawnedOrder: []string{leadConv},
		WaveDeadline: time.Now().Add(-time.Minute),
	}
	require.NoError(t, db.UpsertWaveChoreography(crashed))

	agentd.SweepWaveChoreographiesForTest()
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, 2, memberCount(t, "resumed"),
		"the wave was NOT re-spawned — the existing dev member was reused (no duplicate)")
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c, "choreography completes after the idempotent re-run")
}

// Scenario E: deleting a group cancels its pending choreography — the deferred
// waves never spawn.
func TestWaves_GroupDelete_CancelsPendingWaves(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "cancelme",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/cancelme/deploy",
		map[string]any{"group_name": "goner", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	g, _ := db.GetAgentGroupByName("goner")
	require.NotNil(t, g)
	c, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, c, "a choreography is pending")

	require.NoError(t, db.DeleteAgentGroup("goner"), "delete group")

	c2, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, c2, "group delete cancelled the pending choreography")

	// A sweep after the delete is a graceful no-op (self-healing).
	agentd.SweepWaveChoreographiesForTest()
	agentd.WaitForBackgroundForTest()
}
