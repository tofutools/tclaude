package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a group has a member cap set (agent_groups.max_members).
// The dashboard renders its 👥 member-cap chip from the `/api/snapshot`
// response — so the snapshot's per-group payload MUST carry
// max_members.
//
// Regression: the spawn-guardrails feature added max_members to
// `GET /v1/groups` (the CLI surface) and to the create/update
// handlers, but the dashboard's own snapshot struct (dashboardGroup)
// was missed — so setting a cap via the dashboard chip persisted to
// the DB yet the chip still rendered "no member cap" after a refresh,
// because `/api/snapshot` never sent the field. This pins it on the
// real surface the dashboard refreshes through.
func TestDashboardSnapshot_SurfacesGroupMaxMembers(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)
	f.HaveGroup("alpha") // no cap
	f.HaveGroup("beta")  // capped at 10 — the same write the dashboard PATCH performs
	_, err := db.SetAgentGroupMaxMembers("beta", 10)
	require.NoError(t, err, "SetAgentGroupMaxMembers")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	byName := map[string]*dashGroup{}
	for i := range snap.Groups {
		byName[snap.Groups[i].Name] = &snap.Groups[i]
	}
	require.Contains(t, byName, "alpha", "snapshot missing group alpha")
	require.Contains(t, byName, "beta", "snapshot missing group beta")

	assert.Equal(t, 10, byName["beta"].MaxMembers,
		"a capped group's max_members must surface in /api/snapshot — the 👥 chip renders from it")
	assert.Equal(t, 0, byName["alpha"].MaxMembers,
		"an uncapped group reports 0 (unlimited)")
}
