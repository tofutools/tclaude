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

// JOH-346 force read-back. `tclaude agent task-force ls` + `status` are CLI-side
// compositions of existing group reads; these flow tests drive the daemon
// endpoints they compose (with the spawn/tmux sims) and assert the wire shapes
// carry the fields the two verbs render — the "CLI-shape" level:
//
//   - /v1/groups        → deploy provenance (mission + source_template) — the
//     force filter + the ls MISSION/TEMPLATE columns; a plain group carries
//     neither and is excluded;
//   - /v1/groups/{n}/process → phase + phase map + transitions (ls PHASE, the
//     status phase block);
//   - /v1/groups/{n}/waves   → pending staged-spawn waves (ls WAVES, status);
//   - /v1/groups/{n}/context → per-member settled status + online + context %%
//     (the status per-role liveness rollup);
//   - /v1/cron               → the group's rhythm jobs, incl. the group-retired
//     auto-disable marker (status Rhythms).

// forceGroupRow mirrors the /v1/groups summary fields the read-back verbs read.
type forceGroupRow struct {
	Name           string `json:"name"`
	Members        int    `json:"members"`
	Online         int    `json:"online"`
	Mission        string `json:"mission"`
	SourceTemplate string `json:"source_template"`
}

// forceCtxRow mirrors one /v1/groups/{n}/context entry — the liveness inputs.
type forceCtxRow struct {
	ConvID     string  `json:"conv_id"`
	Role       string  `json:"role"`
	Online     bool    `json:"online"`
	Status     string  `json:"status"`
	ContextPct float64 `json:"context_pct"`
}

// forceWavesRow mirrors the /v1/groups/{n}/waves status.
type forceWavesRow struct {
	PendingWaves int `json:"pending_waves"`
	TotalWaves   int `json:"total_waves"`
}

// forceCronRow mirrors the /v1/cron job fields the status Rhythms block reads.
type forceCronRow struct {
	Name            string `json:"name"`
	TargetKind      string `json:"target_kind"`
	GroupName       string `json:"group_name"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Enabled         bool   `json:"enabled"`
	DisabledReason  string `json:"disabled_reason"`
}

// groupRowByName GETs /v1/groups and returns the row for name (or nil).
func groupRowByName(t *testing.T, f *testharness.Flow, name string) *forceGroupRow {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/groups", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "GET /v1/groups: %s", rec.Body.String())
	var rows []forceGroupRow
	testharness.DecodeJSON(t, rec, &rows)
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// reconTemplateBody is a full-featured force template: a staged roster (lead in
// wave 0, dev deferred to wave 1 → a pending choreography), a 2-phase process,
// and a rhythm cron job. Everything the read-back verbs surface at once.
func reconTemplateBody(name string) map[string]any {
	return map[string]any{
		"name":            name,
		"descr":           "a lead and a dev",
		"default_context": "HOUSE RULES",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true, Wave: 0},
			{Name: "dev", Role: "dev", Wave: 1},
		},
		"process": []map[string]any{
			{"name": "recon", "roles": []string{"lead"}, "criteria": "targets mapped"},
			{"name": "strike", "roles": []string{"dev", "all"}, "criteria": "shipped"},
		},
		"rhythms": []map[string]any{
			{"name": "checkin", "target_role": "lead", "interval": "30m", "body": "status?"},
		},
	}
}

// Scenario: deploy a full-featured force, then assert every endpoint the two
// read-back verbs compose serves the fields they render.
func TestTaskForceReadback_DeployedForceExposesAllFields(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", reconTemplateBody("recon")).Code, "create template")

	const mission = "Neutralise the auth backlog."
	dep := humanReq(t, f, http.MethodPost, "/v1/templates/recon/deploy",
		map[string]any{"group_name": "raid", "mission": mission})
	require.Equalf(t, http.StatusCreated, dep.Code, "deploy: %s", dep.Body.String())
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("raid")
	require.NoError(t, err)
	require.NotNil(t, g)

	// 1. /v1/groups carries the deploy provenance — the ls force filter + columns.
	row := groupRowByName(t, f, "raid")
	require.NotNil(t, row, "the force is listed")
	assert.Equal(t, mission, row.Mission, "mission surfaced for the MISSION column")
	assert.Equal(t, "recon", row.SourceTemplate, "source template surfaced (the force filter)")
	assert.GreaterOrEqual(t, row.Members, 1, "at least the wave-0 lead is a member")
	assert.GreaterOrEqual(t, row.Online, 1, "the wave-0 lead is live")

	// 2. /v1/groups/{n}/process — phase + map + transitions.
	pr := humanReq(t, f, http.MethodGet, "/v1/groups/raid/process", nil)
	require.Equalf(t, http.StatusOK, pr.Code, "GET process: %s", pr.Body.String())
	var st processStateResult
	testharness.DecodeJSON(t, pr, &st)
	assert.Equal(t, "recon", st.CurrentPhase, "starts at the first phase")
	assert.Equal(t, 2, st.PhaseCount)
	require.GreaterOrEqual(t, len(st.Transitions), 1, "an initial transition is recorded")

	// 3. /v1/groups/{n}/waves — the dev is deferred to wave 1, so a wave pends.
	wr := humanReq(t, f, http.MethodGet, "/v1/groups/raid/waves", nil)
	require.Equalf(t, http.StatusOK, wr.Code, "GET waves: %s", wr.Body.String())
	var wv forceWavesRow
	testharness.DecodeJSON(t, wr, &wv)
	assert.GreaterOrEqual(t, wv.PendingWaves, 1, "wave 1 (the dev) is pending")

	// 4. /v1/groups/{n}/context — per-member liveness inputs (status + online).
	cr := humanReq(t, f, http.MethodGet, "/v1/groups/raid/context", nil)
	require.Equalf(t, http.StatusOK, cr.Code, "GET context: %s", cr.Body.String())
	var ctx []forceCtxRow
	testharness.DecodeJSON(t, cr, &ctx)
	require.GreaterOrEqual(t, len(ctx), 1, "the lead is in the roster")
	var lead *forceCtxRow
	for i := range ctx {
		if ctx[i].Role == "lead" {
			lead = &ctx[i]
		}
	}
	require.NotNil(t, lead, "the lead member is present with its role")
	assert.True(t, lead.Online, "the wave-0 lead is online")
	assert.NotEmpty(t, lead.Status, "an online member carries a settled status for the liveness rollup")

	// 5. /v1/cron — the rhythm materialised, enabled, group-targeted at raid.
	rhythm := cronJobByName(t, "raid-checkin")
	require.NotNil(t, rhythm, "the rhythm materialised as a cron job")
	cronRows := forceCronRowsForGroup(t, f, "raid")
	require.Len(t, cronRows, 1, "one group-target rhythm for raid")
	assert.Equal(t, "group", cronRows[0].TargetKind)
	assert.Equal(t, "raid", cronRows[0].GroupName)
	assert.Equal(t, int64(1800), cronRows[0].IntervalSeconds, "30m interval")
	assert.True(t, cronRows[0].Enabled, "the rhythm starts enabled")
	assert.Empty(t, cronRows[0].DisabledReason, "an enabled rhythm carries no disabled marker")
}

// forceCronRowsForGroup GETs /v1/cron and returns the group-target rows for group.
func forceCronRowsForGroup(t *testing.T, f *testharness.Flow, group string) []forceCronRow {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/cron", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "GET /v1/cron: %s", rec.Body.String())
	var resp struct {
		Jobs []forceCronRow `json:"jobs"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	out := []forceCronRow{}
	for _, j := range resp.Jobs {
		if j.TargetKind == "group" && j.GroupName == group {
			out = append(out, j)
		}
	}
	return out
}

// Scenario: a plain hand-built group carries no source template, so it is not a
// force — `ls` excludes it and `status` refuses it.
func TestTaskForceReadback_PlainGroupIsNotAForce(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("hand-built")
	row := groupRowByName(t, f, "hand-built")
	require.NotNil(t, row, "the plain group is still listed by /v1/groups")
	assert.Empty(t, row.SourceTemplate, "a plain group has no source template — excluded from the force filter")
	assert.Empty(t, row.Mission, "and no mission")
}

// Scenario: a stood-down force reads as a dormant record — 0 members, its
// rhythms swept — but its mission, provenance and process history survive, so
// `status` still renders the force block.
func TestTaskForceReadback_StoodDownForceIsDormantButReadable(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", reconTemplateBody("recon2")).Code, "create template")
	const mission = "Wind me down."
	dep := humanReq(t, f, http.MethodPost, "/v1/templates/recon2/deploy",
		map[string]any{"group_name": "dusk", "mission": mission})
	require.Equalf(t, http.StatusCreated, dep.Code, "deploy: %s", dep.Body.String())
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("dusk")
	require.NoError(t, err)
	require.NotNil(t, g)

	// Stand it down.
	sd := humanReq(t, f, http.MethodPost, "/v1/groups/dusk/stand-down", nil)
	require.Equalf(t, http.StatusOK, sd.Code, "stand-down: %s", sd.Body.String())
	agentd.WaitForBackgroundForTest()

	// Dormant: 0 members, no rhythms — but still a force (source_template) with
	// its mission, and its process history survives.
	row := groupRowByName(t, f, "dusk")
	require.NotNil(t, row, "the stood-down force is still listed (a dormant record)")
	assert.Equal(t, 0, row.Online, "no live members → status renders it dormant")
	assert.Equal(t, mission, row.Mission, "mission preserved")
	assert.Equal(t, "recon2", row.SourceTemplate, "provenance preserved — still a force")
	assert.Len(t, forceCronRowsForGroup(t, f, "dusk"), 0, "rhythms were swept")

	// Process history survives — the status phase block still renders.
	pr := humanReq(t, f, http.MethodGet, "/v1/groups/dusk/process", nil)
	require.Equalf(t, http.StatusOK, pr.Code, "process survives stand-down: %s", pr.Body.String())
	var st processStateResult
	testharness.DecodeJSON(t, pr, &st)
	assert.Equal(t, "recon", st.CurrentPhase, "the phase history is intact")
}

// Scenario: a retire that empties a force AUTO-DISABLES its rhythms with the
// group-retired marker — the field `status` renders as "disabled (auto:
// group-retired)", distinct from a hand-paused job.
func TestTaskForceReadback_RetireEmptiedForceShowsAutoDisabledRhythm(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", reconTemplateBody("recon3")).Code, "create template")
	dep := humanReq(t, f, http.MethodPost, "/v1/templates/recon3/deploy",
		map[string]any{"group_name": "ember", "mission": "m"})
	require.Equalf(t, http.StatusCreated, dep.Code, "deploy: %s", dep.Body.String())
	agentd.WaitForBackgroundForTest()

	// Before retire: the rhythm is enabled, no marker.
	rows := forceCronRowsForGroup(t, f, "ember")
	require.Len(t, rows, 1)
	require.True(t, rows[0].Enabled, "rhythm starts enabled")
	require.Empty(t, rows[0].DisabledReason)

	// Retire the whole force — leaves it with no live members.
	code, _ := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, "ember", "")
	require.Equalf(t, http.StatusOK, code, "retire")
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 0, memberCount(t, "ember"), "the force is now empty")

	// The rhythm is auto-disabled and the wire carries the marker distinctly.
	rows = forceCronRowsForGroup(t, f, "ember")
	require.Len(t, rows, 1, "the group-target rhythm is retire-disabled, not deleted")
	assert.False(t, rows[0].Enabled, "the rhythm was auto-disabled")
	assert.Equal(t, db.CronDisabledReasonGroupRetired, rows[0].DisabledReason,
		"the wire distinguishes the tclaude-auto-paused rhythm")
}
