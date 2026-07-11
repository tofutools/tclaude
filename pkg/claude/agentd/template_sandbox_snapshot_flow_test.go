package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func snapshotEnvironment(snapshot *sandboxpolicy.Snapshot) map[string]string {
	out := map[string]string{}
	if snapshot == nil {
		return out
	}
	for _, entry := range snapshot.Effective.Environment {
		out[entry.Name] = entry.Value
	}
	return out
}

func TestTemplateWavesFreezeEffectiveSandboxAcrossProfileEdit(t *testing.T) {
	f := newFlow(t)

	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "wave-policy",
		Environment: []db.SandboxEnvironmentEntry{
			{Name: "TCL_WAVE_POLICY", Value: "initial"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("wave-policy"))

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "sandboxed-waves",
			"agents": []templateAgentSpec{
				{Name: "lead", Role: "lead", Wave: 0},
				{Name: "dev", Role: "dev", Wave: 1},
			},
		}).Code)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/sandboxed-waves/deploy",
		map[string]any{"group_name": "frozen-team", "mission": "test immutable policy"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())

	leadConv := memberByRole(t, "frozen-team", "lead")
	require.NotEmpty(t, leadConv)
	leadSnapshot, err := db.AgentEffectiveSandboxConfigForConv(leadConv)
	require.NoError(t, err)
	require.NotNil(t, leadSnapshot)
	assert.Equal(t, "initial", snapshotEnvironment(leadSnapshot)["TCL_WAVE_POLICY"])

	g, err := db.GetAgentGroupByName("frozen-team")
	require.NoError(t, err)
	choreo, err := db.GetWaveChoreography(g.ID)
	require.NoError(t, err)
	require.NotNil(t, choreo)
	require.NotNil(t, choreo.EffectiveSandbox)
	assert.Equal(t, *leadSnapshot, *choreo.EffectiveSandbox,
		"wave 0 and durable choreography share the one instantiation snapshot")

	// Mutate the source registry after wave 0. The delayed wave must keep the
	// choreography value, not resolve the edited global profile again.
	require.NoError(t, db.UpdateSandboxProfile(&db.SandboxProfile{
		ID: profileID, Name: "wave-policy",
		Environment: []db.SandboxEnvironmentEntry{
			{Name: "TCL_WAVE_POLICY", Value: "edited-after-wave-zero"},
		},
	}))
	settleWaveMember(t, f, leadConv)

	devConv := memberByRole(t, "frozen-team", "dev")
	require.NotEmpty(t, devConv)
	devSnapshot, err := db.AgentEffectiveSandboxConfigForConv(devConv)
	require.NoError(t, err)
	require.NotNil(t, devSnapshot)
	assert.Equal(t, "initial", snapshotEnvironment(devSnapshot)["TCL_WAVE_POLICY"])
	assert.Equal(t, *leadSnapshot, *devSnapshot,
		"later wave consumes the persisted snapshot byte-for-byte")
}

func TestTemplateReinforceResolvesExistingGroupSandboxOnce(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)

	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "global-policy",
		Environment: []db.SandboxEnvironmentEntry{
			{Name: "TCL_SHARED_POLICY", Value: "global"},
			{Name: "TCL_GLOBAL_ONLY", Value: "yes"},
		},
	})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "crew-policy",
		Environment: []db.SandboxEnvironmentEntry{
			{Name: "TCL_SHARED_POLICY", Value: "group"},
			{Name: "TCL_GROUP_ONLY", Value: "yes"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("global-policy"))
	_, err = db.SetAgentGroupSandboxProfile("crew", "crew-policy")
	require.NoError(t, err)
	g, err = db.GetAgentGroupByName("crew")
	require.NoError(t, err)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "reinforcement-policy",
			"agents": []templateAgentSpec{{Name: "scout", Role: "scout"}},
		}).Code)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/reinforcement-policy/reinforce",
		map[string]any{"group_name": "crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "reinforce: %s", rec.Body.String())

	conv := memberByRole(t, "crew", "scout")
	require.NotEmpty(t, conv)
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Equal(t, map[string]string{
		"TCL_GLOBAL_ONLY":   "yes",
		"TCL_GROUP_ONLY":    "yes",
		"TCL_SHARED_POLICY": "group",
	}, snapshotEnvironment(snapshot))
	require.Len(t, snapshot.Applied, 2)
	assert.Equal(t, sandboxpolicy.ScopeGlobal, snapshot.Applied[0].Scope)
	assert.Equal(t, sandboxpolicy.ScopeGroup, snapshot.Applied[1].Scope)
	assert.Equal(t, g.SandboxProfileID, snapshot.Applied[1].ID)
}
