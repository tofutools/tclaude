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

// A task-force deploy is the spawn surface where the operator is LEAST likely
// to have chosen the sandbox themselves: they click deploy, and a template's
// profile, a role's profile, or a role's inline defaults supply the mode for
// every member at once. resolveTemplateAgentLaunch resolves that through its
// own tier stack, separate from handleGroupSpawn's, so a fix applied only to
// the direct-spawn boundary leaves every deployed member's badge saying "this
// launch" — crediting the operator with a per-agent containment decision they
// never made.
func TestTaskForceDeploy_RecordsWhoChoseTheSandbox(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "locked-down", "harness": "claude", "sandbox": "on",
	}).Code, "create locked-down")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "sandbox-team",
		"agents": []map[string]any{
			{"name": "confined-worker", "role": "dev", "spawn_profile": "locked-down"},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/sandbox-team/deploy", map[string]any{
		"group_name": "sandbox-force",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned, "the member spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	convID := res.Agents[0].ConvID
	require.Emptyf(t, res.Agents[0].Error, "member spawned cleanly")

	sandbox, ok := f.World.SpawnSandbox(convID)
	require.Truef(t, ok, "no spawn recorded for conv %s", convID)
	require.Equal(t, "on", sandbox, "the profile's sandbox reaches the deployed member")

	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "spawned session row")
	assert.Equal(t, `profile "locked-down"`, rows[0].SandboxModeSource,
		"the deployed member records the tier that chose its sandbox, not an anonymous launch")
	assert.Contains(t, rows[0].OSSandboxSource, `profile "locked-down"`,
		"and the recorded verdict the badge renders names it too")
}
