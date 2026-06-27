package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestResolveRetiredByDisplay covers the retired "by" column resolution
// (JOH-306) at the unit level: a stable agent_id companion resolves to
// "name (agt_xxxxxxxx)", falls back to the bare short agent_id when the name is
// gone, and a row with no companion shows its raw retired_by value unchanged.
func TestResolveRetiredByDisplay(t *testing.T) {
	setupTestDB(t)

	// A named retirer → "name (agt_xxxxxxxx)".
	retirer, err := db.AllocateAgent("retirer-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SetConvIndexCustomTitle("retirer-conv", "PO Lead", ""))
	assert.Equal(t, "PO Lead ("+agent.ShortAgentID(retirer, "")+")",
		resolveRetiredByDisplay("retirer-conv", retirer),
		"companion resolves to the retirer's current name + stable short id")

	// A companion whose actor has no resolvable title → bare short agent_id.
	nameless, err := db.AllocateAgent("nameless-conv", "spawn")
	require.NoError(t, err)
	assert.Equal(t, agent.ShortAgentID(nameless, ""),
		resolveRetiredByDisplay("nameless-conv", nameless),
		"no name → bare short agent_id, never a conv-id")

	// No companion → the raw retired_by literal, untouched.
	assert.Equal(t, "human", resolveRetiredByDisplay("human", ""))
	assert.Equal(t, "system:export-clone", resolveRetiredByDisplay("system:export-clone", ""))
}

// TestCollectRetiredSnapshot_RetiredByName drives the full retire→snapshot path:
// an agent retired by ANOTHER agent surfaces the retirer's NAME (+ stable id) in
// the dashboard's retired "by", while the raw conv-id is kept only for hover
// provenance. This is the surface the operator hit in JOH-306.
func TestCollectRetiredSnapshot_RetiredByName(t *testing.T) {
	setupTestDB(t)

	retirer, err := db.AllocateAgent("retirer-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SetConvIndexCustomTitle("retirer-conv", "PO Lead", ""))

	_, err = db.AllocateAgent("target-conv", "spawn")
	require.NoError(t, err)

	// Retire the target as if the retirer agent did it (by = its conv-id, the
	// value enrollmentActor produces for an agent caller).
	ok, err := db.RetireAgent("target-conv", "retirer-conv", "cleanup")
	require.NoError(t, err)
	require.True(t, ok)

	retired, err := db.ListRetiredAgents()
	require.NoError(t, err)

	out := collectRetiredSnapshot(retired, map[string]struct{}{})
	require.Len(t, out, 1)
	assert.Equal(t, "retirer-conv", out[0].RetiredBy, "raw audit value kept for provenance")
	assert.Equal(t, "PO Lead ("+agent.ShortAgentID(retirer, "")+")", out[0].RetiredByDisplay,
		"the by column shows the retirer's name, not a bare conv-id")
}
