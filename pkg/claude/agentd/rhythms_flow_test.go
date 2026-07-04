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

// JOH-244 seeded "rhythms": a template declares recurring nudges that are
// materialized at deploy as normal group cron jobs, role-filtered at fire time,
// and removed on group delete.

// Scenario: a template rhythm materializes as a group-target cron job with the
// declared role filter, fires only to the role-matching members, and is
// removed when the group is deleted.
func TestRhythms_MaterializeFireAndCleanup(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "drummers",
		"agents": []templateAgentSpec{
			{Name: "po", Role: "po", IsOwner: true},
			{Name: "dev1", Role: "dev"},
			{Name: "dev2", Role: "dev"},
		},
		"rhythms": []map[string]any{
			{"name": "standup", "target_role": "dev", "interval": "10m", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/drummers/deploy",
		map[string]any{"group_name": "band", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, res.RhythmsCreated, "the rhythm was materialized")

	// The materialized cron job: named <group>-<rhythm>, group-target, role dev.
	g, err := db.GetAgentGroupByName("band")
	require.NoError(t, err)
	require.NotNil(t, g)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	var seeded *db.AgentCronJob
	for _, j := range jobs {
		if j.Name == "band-standup" {
			seeded = j
		}
	}
	require.NotNil(t, seeded, "materialized cron job present")
	assert.True(t, seeded.IsGroupTarget(), "group-target job")
	assert.Equal(t, g.ID, seeded.GroupID)
	assert.Equal(t, "dev", seeded.TargetRole, "role filter carried onto the cron job")
	assert.Equal(t, int64(600), seeded.IntervalSeconds, "interval parsed to seconds")

	po := memberByRole(t, "band", "po")
	require.NotEmpty(t, po)

	// Spawned members already carry their startup briefing rows, so measure the
	// DELTA the fire adds, not the absolute inbox count.
	g2, _ := db.GetAgentGroupByName("band")
	members, err := db.ListAgentGroupMembers(g2.ID)
	require.NoError(t, err)
	before := map[string]int{}
	for _, m := range members {
		before[m.ConvID] = msgRowCount(t, m.ConvID)
	}

	// Fire it: only the dev-role members get a new row; the PO does not.
	assert.Equal(t, "ok", fireCronNow(t, f, seeded.ID), "fire status")

	assert.Zero(t, msgRowCount(t, po)-before[po], "the PO's role does not match the filter — no delivery")
	devDeliveries := 0
	for _, m := range members {
		if m.Role == "dev" {
			assert.Equal(t, 1, msgRowCount(t, m.ConvID)-before[m.ConvID], "each dev got exactly one nudge")
			devDeliveries++
		}
	}
	assert.Equal(t, 2, devDeliveries, "both devs matched the role filter")

	// Deleting the group removes the seeded cron job (JOH-244).
	require.NoError(t, db.DeleteAgentGroup("band"), "delete group")
	gone, err := db.GetAgentCronJob(seeded.ID)
	require.NoError(t, err)
	assert.Nil(t, gone, "the seeded rhythm cron job was removed with its group")
}

// Scenario: a rhythm with no role filter ("all") fans out to every member.
func TestRhythms_NoRoleFilter_FiresToWholeGroup(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "wholeband",
		"agents": []templateAgentSpec{
			{Name: "po", Role: "po", IsOwner: true},
			{Name: "dev", Role: "dev"},
		},
		"rhythms": []map[string]any{
			{"name": "heartbeat", "target_role": "all", "interval": "30s", "body": "alive?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/wholeband/deploy",
		map[string]any{"group_name": "everyone", "mission": "m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	var seeded *db.AgentCronJob
	for _, j := range jobs {
		if j.Name == "everyone-heartbeat" {
			seeded = j
		}
	}
	require.NotNil(t, seeded)
	assert.Empty(t, seeded.TargetRole, `"all" normalized to no filter (whole group)`)

	po := memberByRole(t, "everyone", "po")
	dev := memberByRole(t, "everyone", "dev")
	poBefore, devBefore := msgRowCount(t, po), msgRowCount(t, dev)

	assert.Equal(t, "ok", fireCronNow(t, f, seeded.ID))
	assert.Equal(t, 1, msgRowCount(t, po)-poBefore, "PO got the whole-group nudge")
	assert.Equal(t, 1, msgRowCount(t, dev)-devBefore, "dev got the whole-group nudge")
}

// Scenario: waves + rhythms survive an export → import round-trip (they ride
// the same inner templateJSON the envelope wraps).
func TestWavesRhythms_ExportImportRoundTrip(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name":          "portable",
		"wave_max_wait": 120,
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true, Wave: 0},
			{Name: "dev", Role: "dev", Wave: 2},
		},
		"rhythms": []map[string]any{
			{"name": "ping", "target_role": "dev", "cron_expr": "0 * * * *", "subject": "hourly", "body": "status?"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	// Export.
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/portable/export", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export: %s", rec.Body.String())
	var env struct {
		Format        string         `json:"format"`
		FormatVersion int            `json:"format_version"`
		Template      map[string]any `json:"template"`
	}
	testharness.DecodeJSON(t, rec, &env)

	// Import under a new name.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?as=portable2",
		map[string]any{"format": env.Format, "format_version": env.FormatVersion, "template": env.Template})
	require.Equalf(t, http.StatusCreated, rec.Code, "import: %s", rec.Body.String())

	// Fetch the imported template — wave + rhythms + wave_max_wait survived.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/portable2", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "fetch imported: %s", rec.Body.String())
	var got struct {
		WaveMaxWait int `json:"wave_max_wait"`
		Agents      []struct {
			Name string `json:"name"`
			Wave int    `json:"wave"`
		} `json:"agents"`
		Rhythms []struct {
			Name       string `json:"name"`
			TargetRole string `json:"target_role"`
			CronExpr   string `json:"cron_expr"`
			Subject    string `json:"subject"`
			Body       string `json:"body"`
		} `json:"rhythms"`
	}
	testharness.DecodeJSON(t, rec, &got)

	assert.Equal(t, 120, got.WaveMaxWait, "wave_max_wait survived")
	require.Len(t, got.Agents, 2)
	waveByName := map[string]int{}
	for _, a := range got.Agents {
		waveByName[a.Name] = a.Wave
	}
	assert.Equal(t, 0, waveByName["lead"], "lead wave preserved")
	assert.Equal(t, 2, waveByName["dev"], "dev wave preserved")
	require.Len(t, got.Rhythms, 1, "rhythm preserved")
	assert.Equal(t, "ping", got.Rhythms[0].Name)
	assert.Equal(t, "dev", got.Rhythms[0].TargetRole)
	assert.Equal(t, "0 * * * *", got.Rhythms[0].CronExpr)
	assert.Equal(t, "hourly", got.Rhythms[0].Subject)
	assert.Equal(t, "status?", got.Rhythms[0].Body)
}
