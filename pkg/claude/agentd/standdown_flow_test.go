package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-345 wind-down symmetry. These flow tests drive the two connected surfaces
// with the spawn/tmux simulators and assert at real surfaces (group membership,
// cron-job rows, wave choreography rows, the group row):
//
//   - task-force stand-down (POST /v1/groups/{name}/stand-down): retire the
//     roster + sweep the deploy-seeded rhythms and pending waves, KEEP the group;
//   - the retire leak fix: a retire that empties a group auto-DISABLES its
//     group-target rhythms (leaving a human-disabled job untouched), and a
//     resume re-enables exactly those.

// standDownResult mirrors the POST /v1/groups/{name}/stand-down response without
// importing the unexported daemon type.
type standDownResult struct {
	Group   string `json:"group"`
	Action  string `json:"action"`
	Members []struct {
		ConvID string `json:"conv_id"`
		Title  string `json:"title"`
		Action string `json:"action"`
		Detail string `json:"detail"`
	} `json:"members"`
	RhythmsRemoved int      `json:"rhythms_removed"`
	WavesCancelled int      `json:"waves_cancelled"`
	Warnings       []string `json:"warnings"`
}

// groupTargetCronCount returns how many group-target cron jobs currently target
// groupID — the surface a Cron-tab reader (and the rhythm scheduler) sees.
func groupTargetCronCount(t *testing.T, groupID int64) int {
	t.Helper()
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	n := 0
	for _, j := range jobs {
		if j.IsGroupTarget() && j.GroupID == groupID {
			n++
		}
	}
	return n
}

// cronJobByName returns the single cron job with the given name, or nil.
func cronJobByName(t *testing.T, name string) *db.AgentCronJob {
	t.Helper()
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

// Scenario: stand-down is the mirror of deploy. Deploy a force with a rhythm and
// a deferred wave (so both a group-target cron job AND a pending choreography row
// exist), stand it down, and assert: the roster is retired, the rhythm cron job
// is GONE, the pending choreography is GONE, and the group row SURVIVES with its
// mission intact (a dormant record, not a delete).
func TestStandDown_RetiresSweepsAndKeepsGroup(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name":  "wind-crew",
		"descr": "a lead and a dev",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true, Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
		"rhythms": []map[string]any{
			{"name": "checkin", "target_role": "lead", "interval": "30m", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	const mission = "Ship the passwordless-login epic."
	depRec := humanReq(t, f, http.MethodPost, "/v1/templates/wind-crew/deploy",
		map[string]any{"group_name": "gale", "mission": mission})
	require.Equalf(t, http.StatusCreated, depRec.Code, "deploy: %s", depRec.Body.String())
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("gale")
	require.NoError(t, err)
	require.NotNil(t, g)

	// Pre-conditions: the lead (wave 0) is up, the rhythm materialized as a
	// group-target cron job, and a pending choreography row holds wave 1.
	require.Equal(t, 1, memberCount(t, "gale"), "only the lead (wave 0) is up")
	require.Equal(t, 1, groupTargetCronCount(t, g.ID), "the rhythm materialized as a group cron job")
	choreo, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, choreo, "a pending wave choreography row exists")
	require.GreaterOrEqual(t, choreo.PendingWaves(), 1, "wave 1 is pending")

	// Stand down the force.
	rec := humanReq(t, f, http.MethodPost, "/v1/groups/gale/stand-down", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "stand-down: %s", rec.Body.String())
	var sd standDownResult
	testharness.DecodeJSON(t, rec, &sd)
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, "gale", sd.Group)
	assert.Equal(t, "stand-down", sd.Action)
	assert.Equal(t, 1, sd.RhythmsRemoved, "the rhythm cron job was swept")
	assert.GreaterOrEqual(t, sd.WavesCancelled, 1, "the pending wave was cancelled")
	require.Len(t, sd.Members, 1, "the one live member is reported")
	assert.Equal(t, "retired", sd.Members[0].Action, "the lead was retired")

	// Post-conditions at real surfaces: no members, no group-target cron jobs,
	// no choreography row — but the group row SURVIVES with its mission.
	assert.Equal(t, 0, memberCount(t, "gale"), "the roster is retired")
	assert.Equal(t, 0, groupTargetCronCount(t, g.ID), "the rhythm cron job is gone")
	choreoAfter, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	assert.Nil(t, choreoAfter, "the pending choreography is gone")

	survivor, err := db.GetAgentGroupByName("gale")
	require.NoError(t, err)
	require.NotNil(t, survivor, "the group row survives a stand-down (dormant record)")
	assert.Equal(t, mission, survivor.Mission, "the mission is preserved")
	assert.Equal(t, "wind-crew", survivor.SourceTemplate, "the provenance is preserved")

	commands, err := db.ListAuditLog(db.AuditLogFilter{Verb: "group.stand-down"})
	require.NoError(t, err)
	require.Len(t, commands, 1, "stand-down has one canonical command audit row")
	require.NotEmpty(t, commands[0].EventID)
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET created_at = ? WHERE conv_id = ?`,
		time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano), sd.Members[0].ConvID)
	require.NoError(t, err)
	_ = agentd.RunReaperTickForTest(time.Now())
	exits, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, exits, 1, "stand-down's resulting managed pane exit is audited exactly once")
	assert.Equal(t, commands[0].EventID, exits[0].RelatedEventID)
	assert.Equal(t, db.AgentExitActionRetire, exits[0].LifecycleAction)
}

// Scenario: standing down a plain hand-built group (no template, no rhythms, no
// waves) is a clean retire-with-nothing-to-sweep — it is not over-gated on
// "is a force".
func TestStandDown_PlainGroupJustRetires(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("hand-built")
	rec := humanReq(t, f, http.MethodPost, "/v1/groups/hand-built/stand-down", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "stand-down of a plain group: %s", rec.Body.String())
	var sd standDownResult
	testharness.DecodeJSON(t, rec, &sd)
	assert.Equal(t, "stand-down", sd.Action)
	assert.Equal(t, 0, sd.RhythmsRemoved, "nothing to sweep")
	assert.Equal(t, 0, sd.WavesCancelled, "nothing to cancel")

	// The group survives as a dormant record.
	survivor, err := db.GetAgentGroupByName("hand-built")
	require.NoError(t, err)
	assert.NotNil(t, survivor, "the plain group survives too")
}

// Scenario: a retire that leaves the group with NO live members auto-disables
// its group-target rhythms — and leaves a job the human disabled by hand
// untouched (so a later resume won't silently re-enable it).
func TestRetire_EmptiesGroup_AutoDisablesRhythms(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "beat-crew",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"rhythms": []map[string]any{
			{"name": "standup", "target_role": "all", "interval": "30m", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/beat-crew/deploy",
			map[string]any{"group_name": "combo", "mission": "m"}).Code, "deploy")
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("combo")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, 2, memberCount(t, "combo"), "both agents are up (all wave 0)")

	// The materialized rhythm — enabled at deploy.
	rhythm := cronJobByName(t, "combo-standup")
	require.NotNil(t, rhythm, "the rhythm materialized")
	require.True(t, rhythm.Enabled, "the rhythm starts enabled")

	// A SECOND group-target job the human paused by hand: enabled=0, no marker.
	handID, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            "combo-hand-paused",
		TargetKind:      db.CronTargetGroup,
		GroupID:         g.ID,
		IntervalSeconds: 600,
		Body:            "manual",
		Enabled:         false,
	})
	require.NoError(t, err)

	// Retire the whole group — leaves it with no live members.
	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, "combo", "")
	require.Equalf(t, http.StatusOK, code, "retire")
	assert.NotEmpty(t, firstRetiredConv(resp), "at least one member retired")
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 0, memberCount(t, "combo"), "the group is now empty")

	// The rhythm was auto-disabled with the group-retired marker.
	rhythm = cronJobByName(t, "combo-standup")
	require.NotNil(t, rhythm)
	assert.False(t, rhythm.Enabled, "the rhythm was auto-disabled")
	assert.Equal(t, db.CronDisabledReasonGroupRetired, rhythm.DisabledReason,
		"stamped as auto-disabled")

	// The human-paused job is untouched — still disabled, still no marker.
	hand, err := db.GetAgentCronJob(handID)
	require.NoError(t, err)
	require.NotNil(t, hand)
	assert.False(t, hand.Enabled, "the human-paused job stays disabled")
	assert.Empty(t, hand.DisabledReason, "the human-paused job is NOT stamped auto-disabled")
}

// resumeGroup fires POST /v1/groups/{group}/resume as the human and asserts 200.
func resumeGroup(t *testing.T, f *testharness.Flow, group string) {
	t.Helper()
	rec := testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/"+group+"/resume", nil)))
	require.Equalf(t, http.StatusOK, rec.Code, "resume %s: %s", group, rec.Body.String())
	agentd.WaitForBackgroundForTest()
}

// Scenario: resume on a still-EMPTY dormant group does NOT re-enable the
// auto-disabled rhythms — retire removed the membership, so a resume can't
// repopulate the group, and re-enabling would just re-create the "firing to
// nobody" leak the auto-disable prevents.
func TestResume_EmptyGroup_LeavesRhythmsDisabled(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "still-crew",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"rhythms": []map[string]any{
			{"name": "standup", "target_role": "all", "interval": "30m", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/still-crew/deploy",
			map[string]any{"group_name": "quiet", "mission": "m"}).Code, "deploy")
	agentd.WaitForBackgroundForTest()

	// Retire empties the group and auto-disables the rhythm.
	code, _ := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, "quiet", "")
	require.Equalf(t, http.StatusOK, code, "retire")
	agentd.WaitForBackgroundForTest()
	require.Equal(t, 0, memberCount(t, "quiet"), "retire emptied the group")
	require.False(t, cronJobByName(t, "quiet-standup").Enabled, "rhythm auto-disabled")

	// Resume the (still empty) group — nobody to resume, so the rhythm stays
	// disabled rather than firing to an empty group again.
	resumeGroup(t, f, "quiet")

	rhythm := cronJobByName(t, "quiet-standup")
	require.NotNil(t, rhythm)
	assert.False(t, rhythm.Enabled, "resume on an empty group leaves the rhythm disabled")
	assert.Equal(t, db.CronDisabledReasonGroupRetired, rhythm.DisabledReason,
		"the marker is preserved for a later repopulate")
}

// Scenario: once the force is back — a member re-spawned into the dormant group
// before a resume — resume re-enables EXACTLY the auto-disabled rhythms, and
// never a job the human disabled by hand.
func TestResume_RepopulatedGroup_ReenablesOnlyAutoDisabledRhythms(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "loop-crew",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"rhythms": []map[string]any{
			{"name": "standup", "target_role": "all", "interval": "30m", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/loop-crew/deploy",
			map[string]any{"group_name": "cadence", "mission": "m"}).Code, "deploy")
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("cadence")
	require.NoError(t, err)
	require.NotNil(t, g)

	// A human-paused group-target job, alongside the auto-disable-able rhythm.
	handID, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name:            "cadence-hand-paused",
		TargetKind:      db.CronTargetGroup,
		GroupID:         g.ID,
		IntervalSeconds: 600,
		Body:            "manual",
		Enabled:         false,
	})
	require.NoError(t, err)

	// Retire empties the group and auto-disables the rhythm.
	code, _ := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, "cadence", "")
	require.Equalf(t, http.StatusOK, code, "retire")
	agentd.WaitForBackgroundForTest()
	require.False(t, cronJobByName(t, "cadence-standup").Enabled, "rhythm auto-disabled")

	// Bring the force back: spawn a fresh live member into the dormant group.
	f.Spawn("cadence", "reviver")
	require.GreaterOrEqual(t, memberCount(t, "cadence"), 1, "the group has a live member again")

	// Resume the now-repopulated group.
	resumeGroup(t, f, "cadence")

	// The rhythm is re-enabled and its marker cleared.
	rhythm := cronJobByName(t, "cadence-standup")
	require.NotNil(t, rhythm)
	assert.True(t, rhythm.Enabled, "the auto-disabled rhythm was re-enabled once the force returned")
	assert.Empty(t, rhythm.DisabledReason, "the marker was cleared")

	// The human-paused job stays disabled — resume never touches it.
	hand, err := db.GetAgentCronJob(handID)
	require.NoError(t, err)
	require.NotNil(t, hand)
	assert.False(t, hand.Enabled, "the human-paused job is left disabled by resume")
	assert.Empty(t, hand.DisabledReason)
}

// Scenario: stand-down gating mirrors retire — the human always passes, a group
// owner passes structurally, and a plain member without groups.retire is refused.
func TestStandDown_Gating(t *testing.T) {
	f := newFlow(t)

	res := deployStrikeTeam(t, f, "Harden auth")
	leadConv := convForAgent(res, "lead") // the owner
	devConv := convForAgent(res, "dev")   // a plain member
	require.NotEmpty(t, leadConv)
	require.NotEmpty(t, devConv)

	// A plain member without groups.retire is refused — nothing changes.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/raid/stand-down", nil), devConv))
	require.Equalf(t, http.StatusForbidden, rec.Code,
		"non-owner without groups.retire should be 403; body=%s", rec.Body.String())
	assert.Equal(t, 2, memberCount(t, "raid"), "a refused stand-down changed nothing")

	// The owner (the lead) passes via the structural owner bypass — this stand
	// down winds the force down. The owner is the CALLER, so it is skipped
	// (skipped:self — an agent never retires itself); only the dev retires, so
	// the lead survives as the sole member.
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/raid/stand-down", nil), leadConv))
	require.Equalf(t, http.StatusOK, rec.Code, "owner stand-down should pass; body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, memberCount(t, "raid"), "the dev retired; the owner-caller is skipped:self")

	// The human always passes — a fresh force stood down via the human peer.
	res2 := deployStrikeTeamAs(t, f, "squad", "Second mission")
	require.NotEmpty(t, res2.Group)
	humanRec := humanReq(t, f, http.MethodPost, "/v1/groups/squad/stand-down", nil)
	require.Equalf(t, http.StatusOK, humanRec.Code, "human stand-down should pass; body=%s", humanRec.Body.String())
}

// firstRetiredConv returns the conv-id of the first retired member in a retire
// response — a small helper so the auto-disable test can assert one member was
// actually retired without hard-coding a conv-id.
func firstRetiredConv(resp groupRetireResp) string {
	for _, m := range resp.Members {
		if m.Action == "retired" {
			return m.ConvID
		}
	}
	return ""
}

// deployStrikeTeamAs deploys the (already-created) strike-team template to a new
// group name — used to stand up a second force in one test without re-creating
// the template.
func deployStrikeTeamAs(t *testing.T, f *testharness.Flow, group, mission string) deployResult {
	t.Helper()
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/strike-team/deploy",
		map[string]any{"group_name": group, "mission": mission})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy %s: %s", group, rec.Body.String())
	var res deployResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()
	return res
}
