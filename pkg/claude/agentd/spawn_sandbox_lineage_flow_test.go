package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestSpawnSandboxLineage_Matrix(t *testing.T) {
	tests := []struct {
		name          string
		parentHarness string
		parentSandbox string
		body          map[string]any
		wantStatus    int
	}{
		{
			name:          "codex agent profile can spawn claude inherit",
			parentHarness: harness.CodexName,
			parentSandbox: harness.SandboxManagedProfile,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.DefaultName,
				"sandbox": harness.ClaudeSandboxInherit,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:          "codex workspace write cannot spawn claude inherit",
			parentHarness: harness.CodexName,
			parentSandbox: harness.SandboxWorkspaceWrite,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.DefaultName,
				"sandbox": harness.ClaudeSandboxInherit,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:          "codex read only can spawn codex read only",
			parentHarness: harness.CodexName,
			parentSandbox: harness.SandboxReadOnly,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.CodexName,
				"sandbox": harness.SandboxReadOnly,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:          "codex read only cannot spawn codex managed profile",
			parentHarness: harness.CodexName,
			parentSandbox: harness.SandboxReadOnly,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.CodexName,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:          "claude inherit cannot spawn claude off",
			parentHarness: harness.DefaultName,
			parentSandbox: harness.ClaudeSandboxInherit,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.DefaultName,
				"sandbox": harness.ClaudeSandboxOff,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:          "claude inherit cannot spawn codex danger full access",
			parentHarness: harness.DefaultName,
			parentSandbox: harness.ClaudeSandboxInherit,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.CodexName,
				"sandbox": harness.SandboxDangerFull,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:          "claude on can spawn codex managed profile",
			parentHarness: harness.DefaultName,
			parentSandbox: harness.ClaudeSandboxOn,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.CodexName,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:          "claude off can spawn codex danger full access",
			parentHarness: harness.DefaultName,
			parentSandbox: harness.ClaudeSandboxOff,
			body: map[string]any{
				"name":    "worker",
				"harness": harness.CodexName,
				"sandbox": harness.SandboxDangerFull,
			},
			wantStatus: http.StatusOK,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			parent := fmt.Sprintf("parent-%04d-aaaa-bbbb-cccc-111111111111", i)
			haveSpawnCapableSandboxParent(t, f, "alpha", parent, tt.parentHarness, tt.parentSandbox)

			resp := f.AsAgent(parent).SpawnWith("alpha", tt.body)
			require.Equalf(t, tt.wantStatus, resp.Code, "spawn body=%s", resp.Raw)
			if tt.wantStatus == http.StatusForbidden {
				assert.Contains(t, string(resp.Raw), "sandbox_restricted")
			}
		})
	}
}

func TestSpawnSandboxLineage_ProfileDerivedWeakSandboxRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-prof-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "unsafe", "sandbox": harness.ClaudeSandboxOff}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "unsafe").Code, "set default_profile")

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "sandbox_restricted")
}

func TestSpawnSandboxLineage_TemplateInstantiateRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-tpl1-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	createBody := map[string]any{
		"name": "weak-template",
		"agents": []map[string]any{
			{"name": "worker", "harness": harness.DefaultName, "sandbox": harness.ClaudeSandboxOff},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/templates/weak-template/instantiate",
		map[string]any{"group_name": "weak-cast"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 0, res.Spawned, "template child with looser sandbox must not spawn")
	assert.Equal(t, 1, res.Failed, "template spawn reports the lineage refusal per agent")
	require.Len(t, res.Agents, 1)
	assert.Contains(t, res.Agents[0].Error, "may not spawn")
	assert.Equal(t, 0, memberCount(t, "weak-cast"), "refused template child was not enrolled")
}

func TestSpawnSandboxLineage_StagedTemplateWaveRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-wave-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	createBody := map[string]any{
		"name": "staged-weak",
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "wave": 0},
			{"name": "dev", "role": "dev", "wave": 1, "harness": harness.DefaultName, "sandbox": harness.ClaudeSandboxOff},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/templates/staged-weak/deploy",
		map[string]any{"group_name": "staged-cast", "mission": "exercise delayed wave guard"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res waveDeployResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 1, res.Spawned, "wave 0 still spawns")
	assert.Equal(t, 1, res.PendingAgents, "wave 1 is deferred")

	leadConv := memberByRole(t, "staged-cast", "lead")
	require.NotEmpty(t, leadConv)
	settleWaveMember(t, f, leadConv)

	assert.Empty(t, memberByRole(t, "staged-cast", "dev"), "looser delayed-wave child must not spawn")
	assert.Equal(t, 1, memberCount(t, "staged-cast"), "only the allowed first wave is enrolled")
}

func haveSpawnCapableSandboxParent(t *testing.T, f *testharness.Flow, group, convID, h, sandbox string) {
	t.Helper()
	f.HaveMember(group, convID)
	require.NoError(t, db.GrantAgentPermission(convID, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          "sess-" + convID[:8],
		TmuxSession: "tmux-" + convID[:8],
		ConvID:      convID,
		Cwd:         f.World.HomeDir,
		Status:      "running",
		Harness:     h,
		SandboxMode: sandbox,
	}))
}
