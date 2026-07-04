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

// JOH-246 bundled starter task forces. These flow tests drive the /v1/starters
// surface (list / show / install) and prove every embedded starter installs on
// a fresh empty DB through the REAL import validator, then deploys dev-squad end
// to end (waves settle via the sim, its rhythm materializes as a group cron job,
// its work pattern delivers once the roster is whole). A starter that drifts out
// of schema fails install here — so schema drift fails CI.

// starterSummaryResp mirrors the daemon's GET /v1/starters list shape.
type starterSummaryResp struct {
	Name        string `json:"name"`
	Descr       string `json:"descr"`
	Agents      int    `json:"agents"`
	Waves       int    `json:"waves"`
	Rhythms     int    `json:"rhythms"`
	Process     int    `json:"process"`
	WorkPattern int    `json:"work_pattern"`
}

// starterInstallResp mirrors the daemon's install response.
type starterInstallResp struct {
	Name      string   `json:"name"`
	Installed bool     `json:"installed"`
	Skipped   bool     `json:"skipped"`
	Message   string   `json:"message"`
	Warnings  []string `json:"warnings"`
}

// Scenario: list the bundled starters, then install EVERY one on a fresh DB
// through the real import path. Iterating the live list (not a hardcoded set)
// means a newly-added starter is covered automatically, and any starter that no
// longer validates fails the install here — the CI drift guard. Re-installing
// is idempotent: the second install is skipped, never a clobber.
func TestStarters_InstallEachOnFreshDB(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodGet, "/v1/starters", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "list starters: %s", rec.Body.String())
	var starters []starterSummaryResp
	testharness.DecodeJSON(t, rec, &starters)
	require.GreaterOrEqual(t, len(starters), 3, "at least the three curated starters ship")

	// The three curated starters are present.
	byName := map[string]starterSummaryResp{}
	for _, s := range starters {
		byName[s.Name] = s
	}
	for _, want := range []string{"dev-squad", "research-pod", "review-crew"} {
		s, ok := byName[want]
		require.Truef(t, ok, "starter %q is bundled", want)
		assert.NotEmptyf(t, s.Descr, "starter %q has a description", want)
		assert.Positivef(t, s.Agents, "starter %q has agents", want)
	}

	for _, s := range starters {
		// show renders the inner template (same shape as templates show).
		showRec := humanReq(t, f, http.MethodGet, "/v1/starters/"+s.Name, nil)
		require.Equalf(t, http.StatusOK, showRec.Code, "show %q: %s", s.Name, showRec.Body.String())

		// Install on the fresh DB — through the real import validator.
		insRec := humanReq(t, f, http.MethodPost, "/v1/starters/"+s.Name+"/install", nil)
		require.Equalf(t, http.StatusCreated, insRec.Code, "install %q: %s", s.Name, insRec.Body.String())
		var ins starterInstallResp
		testharness.DecodeJSON(t, insRec, &ins)
		assert.Truef(t, ins.Installed, "%q reports installed", s.Name)
		assert.Falsef(t, ins.Skipped, "%q not skipped on first install", s.Name)
		assert.Equalf(t, s.Name, ins.Name, "%q installed under its own name", s.Name)

		// It is now a real, fetchable local template.
		getRec := humanReq(t, f, http.MethodGet, "/v1/templates/"+s.Name, nil)
		require.Equalf(t, http.StatusOK, getRec.Code, "fetch installed template %q: %s", s.Name, getRec.Body.String())

		// Re-install is idempotent: skipped, never clobbered.
		reRec := humanReq(t, f, http.MethodPost, "/v1/starters/"+s.Name+"/install", nil)
		require.Equalf(t, http.StatusOK, reRec.Code, "re-install %q: %s", s.Name, reRec.Body.String())
		var re starterInstallResp
		testharness.DecodeJSON(t, reRec, &re)
		assert.Falsef(t, re.Installed, "%q re-install reports not installed", s.Name)
		assert.Truef(t, re.Skipped, "%q re-install skipped", s.Name)
		assert.NotEmptyf(t, re.Message, "%q skip carries a clear message", s.Name)
	}
}

// Scenario: --as installs a fresh copy under a different name, leaving the
// original name free to install too (so the two coexist).
func TestStarters_InstallAs(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodPost, "/v1/starters/dev-squad/install?as=my-squad", nil)
	require.Equalf(t, http.StatusCreated, rec.Code, "install --as: %s", rec.Body.String())
	var ins starterInstallResp
	testharness.DecodeJSON(t, rec, &ins)
	assert.True(t, ins.Installed)
	assert.Equal(t, "my-squad", ins.Name, "installed under the --as name")

	// The renamed copy exists; the starter's own name is still free.
	assert.Equal(t, http.StatusOK, humanReq(t, f, http.MethodGet, "/v1/templates/my-squad", nil).Code)
	assert.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodGet, "/v1/templates/dev-squad", nil).Code,
		"the --as install did not also claim the starter's own name")
}

// Scenario: a missing starter name 404s on both show and install.
func TestStarters_UnknownName(t *testing.T) {
	f := newFlow(t)
	assert.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodGet, "/v1/starters/nope", nil).Code)
	assert.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodPost, "/v1/starters/nope/install", nil).Code)
}

// Scenario: install the dev-squad starter, then deploy it as a real task force
// and drive it to completion. The lead is wave 0 (plans first); dev, designer,
// reviewer and tester are wave 1 — so deploy spawns only the lead, defers the
// rest, materializes the lead-checkin rhythm as a group cron job, and delivers
// the (deferred) work pattern once the lead settles and the roster is whole.
func TestStarters_DeployDevSquadEndToEnd(t *testing.T) {
	f := newFlow(t)

	insRec := humanReq(t, f, http.MethodPost, "/v1/starters/dev-squad/install", nil)
	require.Equalf(t, http.StatusCreated, insRec.Code, "install dev-squad: %s", insRec.Body.String())

	const mission = "Build the OAuth2 login flow"
	depRec := humanReq(t, f, http.MethodPost, "/v1/templates/dev-squad/deploy",
		map[string]any{"group_name": "devteam", "mission": mission})
	require.Equalf(t, http.StatusCreated, depRec.Code, "deploy: %s", depRec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, depRec, &res)
	agentd.WaitForBackgroundForTest()

	// Wave 0: only the lead spawns synchronously; the other four defer.
	assert.Equal(t, 1, res.Spawned, "only wave 0 (the lead) spawned synchronously")
	assert.Equal(t, 2, res.WavesTotal, "lead wave 0 + rest wave 1")
	assert.Equal(t, 1, res.PendingWaves)
	assert.Equal(t, 4, res.PendingAgents, "dev, designer, reviewer, tester deferred")
	assert.Equal(t, 1, res.RhythmsCreated, "the lead-checkin rhythm materialized")
	assert.Equal(t, 0, res.PatternDelivered, "work pattern deferred until the roster is whole")
	assert.Equal(t, 1, memberCount(t, "devteam"), "only the lead is up")

	// The rhythm materialized as a group-target cron job, role-filtered to lead.
	g, err := db.GetAgentGroupByName("devteam")
	require.NoError(t, err)
	require.NotNil(t, g)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	var checkin *db.AgentCronJob
	for _, j := range jobs {
		if j.Name == "devteam-lead-checkin" {
			checkin = j
		}
	}
	require.NotNil(t, checkin, "lead-checkin cron job present")
	assert.True(t, checkin.IsGroupTarget(), "group-target job")
	assert.Equal(t, "lead", checkin.TargetRole, "role filter carried onto the cron job")
	assert.Equal(t, int64(1800), checkin.IntervalSeconds, "30m interval parsed to seconds")

	// Drive the lead working → idle: the gate releases and wave 1 spawns.
	leadConv := memberByRole(t, "devteam", "lead")
	require.NotEmpty(t, leadConv)
	settleWaveMember(t, f, leadConv)

	// The whole squad is now up: lead + dev + designer + reviewer + tester.
	assert.Equal(t, 5, memberCount(t, "devteam"), "the full squad spawned once the lead settled")
	for _, role := range []string{"lead", "dev", "designer", "reviewer", "tester"} {
		assert.NotEmptyf(t, memberByRole(t, "devteam", role), "role %q member is up", role)
	}

	// The deferred work pattern delivered once the roster was whole — the lead
	// got its kickoff step with the mission interpolated.
	msgs, err := db.ListAgentMessagesForConv(leadConv, 100)
	require.NoError(t, err)
	joined := ""
	for _, m := range msgs {
		joined += m.Body + "\n"
	}
	assert.Contains(t, joined, mission, "the lead's kickoff carries the interpolated mission")
}
