package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDashboardSpawnHarnessPolicyGlobalAndGroupRoundTrip(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	_, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err)

	put := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		serveDashboardGroups(rec, dashboardRequest(http.MethodPut, path, body))
		return rec
	}
	global := put("/api/spawn-harness-policy", `{"rules":[{"source":"claude","target":"codex","decision":"deny","reason":"save credits"}]}`)
	require.Equal(t, http.StatusOK, global.Code, "body=%s", global.Body.String())
	group := put("/api/groups/alpha/spawn-harness-policy", `{"rules":[{"source":"claude","target":"codex","decision":"allow"}]}`)
	require.Equal(t, http.StatusOK, group.Code, "body=%s", group.Body.String())

	var view spawnHarnessPolicyView
	require.NoError(t, json.Unmarshal(group.Body.Bytes(), &view))
	assert.Equal(t, "group", view.Scope)
	assert.Equal(t, "alpha", view.Group)
	require.Len(t, view.Rules, 1)
	assert.Equal(t, db.SpawnHarnessAllow, view.Rules[0].Decision)
	require.Len(t, view.GlobalRules, 1)
	assert.Equal(t, "save credits", view.GlobalRules[0].Reason)
}

func TestDashboardSpawnHarnessPolicyRejectsDenyWithoutReason(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	rec := httptest.NewRecorder()
	serveDashboardGroups(rec, dashboardRequest(http.MethodPut, "/api/spawn-harness-policy",
		`{"rules":[{"source":"claude","target":"codex","decision":"deny"}]}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "requires a reason")
}

func TestDashboardHTMLSpawnHarnessPolicyUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="spawn-harness-policy-open"`:                                   "global cog launcher",
		`cross-harness spawns…`:                                            "regular global and group menu label",
		`theme-copy-wizard">⇄ cross-realm summons…`:                        "wizard global cog label",
		`wizard="⇄ cross-realm summons…"`:                                  "wizard group cog label",
		`id="spawn-harness-policy-root"`:                                   "stable Preact host",
		`mountSpawnHarnessPolicyFeature`:                                   "feature boot wiring",
		`registerSpawnHarnessPolicyController`:                             "group-menu controller boundary",
		`class="spawn-harness-matrix"`:                                     "directed matrix table",
		`Reason returned to the spawning agent`:                            "regular denial reason input",
		`: 'Global cross-realm summons'`:                                   "wizard dialog title",
		`<option value="inherit">${copy.inherit}</option>`:                 "theme-aware group inheritance choice",
		`.spawn-harness-cell select,`:                                      "dark form control styling",
		`resizeKey="tclaude.dash.modalSize.spawn-harness-policy"`:          "persisted dialog resize wiring",
		`.spawn-harness-matrix col.spawn-harness-target { width: 220px; }`: "equal target-column sizing",
		`body.wizard #spawn-harness-policy-modal .cron-create-modal {`:     "wizard dialog skin",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
}

func TestSpawnHarnessPolicyBlankTargetMeansDefaultHarness(t *testing.T) {
	setupTestDB(t)
	const conv = "codex-parent-default-target"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "codex-parent", ConvID: conv, Status: "idle", Harness: "codex",
	}))
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "codex", TargetHarness: "claude",
		Decision: db.SpawnHarnessDeny, Reason: "stay on Codex",
	}}))

	fail := spawnHarnessPolicyFailure(nil, conv, "")
	require.NotNil(t, fail)
	assert.Equal(t, "cross_harness_spawn_denied", fail.Kind)
	assert.Contains(t, fail.Msg, "codex → claude")
}

func TestSpawnHarnessPolicyCloneRequiresEveryDestinationGroup(t *testing.T) {
	setupTestDB(t)
	const caller = "codex-clone-manager"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "codex-clone-manager", ConvID: caller, Status: "idle", Harness: "codex",
	}))
	g1ID, err := db.CreateAgentGroup("allowing", "")
	require.NoError(t, err)
	g2ID, err := db.CreateAgentGroup("inheriting", "")
	require.NoError(t, err)
	g1, err := db.GetAgentGroupByID(g1ID)
	require.NoError(t, err)
	g2, err := db.GetAgentGroupByID(g2ID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "codex", TargetHarness: "claude",
		Decision: db.SpawnHarnessDeny, Reason: "global budget lock",
	}}))
	require.NoError(t, db.ReplaceSpawnHarnessRules(g1ID, []db.SpawnHarnessRule{{
		SourceHarness: "codex", TargetHarness: "claude", Decision: db.SpawnHarnessAllow,
	}}))

	fail := spawnHarnessPolicyFailureForGroups([]*db.AgentGroup{g1, g2}, caller, "claude")
	require.NotNil(t, fail, "one allowing group must not mask another group's inherited deny")
	assert.Contains(t, fail.Msg, "global budget lock")

	require.NoError(t, db.ReplaceSpawnHarnessRules(g2ID, []db.SpawnHarnessRule{{
		SourceHarness: "codex", TargetHarness: "claude", Decision: db.SpawnHarnessAllow,
	}}))
	assert.Nil(t, spawnHarnessPolicyFailureForGroups([]*db.AgentGroup{g1, g2}, caller, "claude"))
}
