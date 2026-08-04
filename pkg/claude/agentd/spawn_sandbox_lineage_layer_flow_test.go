package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The single-wall resolver forces a tclaude-layer child onto its harness's
// no-inner-wall mode, and the session row now records that mode — which is what
// production's `session new` has always written. These tests pin the two things
// that must both hold: the guard still ADMITS exactly the children it admitted
// before the forcing moved earlier, and the row the simulator writes carries
// the forced mode rather than the requested one (TCL-989).

func TestSpawnSandboxLineage_TclaudeLayerChildKeepsItsConfinementClass(t *testing.T) {
	for _, tc := range []struct {
		name         string
		childHarness string
		childSandbox string
		approval     string
		wantMode     string
	}{
		{
			name:         "claude",
			childHarness: harness.DefaultName,
			childSandbox: harness.ClaudeSandboxOn,
			approval:     "default",
			wantMode:     harness.ClaudeSandboxOff,
		},
		{
			name:         "codex",
			childHarness: harness.CodexName,
			childSandbox: harness.SandboxManagedProfile,
			approval:     harness.ApprovalNever,
			wantMode:     harness.SandboxDangerFull,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			const parent = "parent-lyr1-aaaa-bbbb-cccc-111111111111"
			// A Claude `inherit` parent is the tightest posture that could mint
			// the child's PRE-forcing class, so it is the case that would break
			// first if the guard read the forced mode without mapping it.
			haveSpawnCapableSandboxParent(t, f, "alpha", parent,
				harness.DefaultName, harness.ClaudeSandboxInherit)

			resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
				"name":                   "worker",
				"harness":                tc.childHarness,
				"sandbox":                tc.childSandbox,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"approval":               tc.approval,
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			row, err := db.FindSessionByConvID(resp.ConvID)
			require.NoError(t, err)
			require.NotNil(t, row, "the spawn must have written a session row")
			assert.Equal(t, tc.wantMode, row.HarnessBuiltinMode,
				"the simulator must persist the mode the implementation launches under")
			assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
				row.SandboxImplementation,
				"the row must record who owns the wall alongside the forced mode")
		})
	}
}

// The same child from a parent that could never mint its confinement class must
// still be refused: the mapping restores the previous decision, it does not
// widen it.
func TestSpawnSandboxLineage_TclaudeLayerChildStillRefusedByWeakerParent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-lyr2-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent,
		harness.CodexName, harness.SandboxWorkspaceWrite)

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name":                   "worker",
		"harness":                harness.DefaultName,
		"sandbox":                harness.ClaudeSandboxOn,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		"approval":               "default",
	})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "sandbox_restricted")
}

// executeSpawn is the other spawn boundary — templates, waves and process
// adapters never pass through the HTTP request path — so it has to carry the
// implementation into the guard too.
func TestSpawnSandboxLineage_TemplateCarriesTclaudeLayerImplementation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-lyr3-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent,
		harness.DefaultName, harness.ClaudeSandboxInherit)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	createBody := map[string]any{
		"name": "layered-template",
		"agents": []map[string]any{{
			"name":    "worker",
			"harness": harness.DefaultName,
			"sandbox": harness.ClaudeSandboxOn,
			"profile_inline": map[string]any{
				"harness":                harness.DefaultName,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
			},
		}},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := agentReqProof(t, f, parent, http.MethodPost,
		"/v1/templates/layered-template/instantiate",
		map[string]any{"group_name": "layered-cast", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equalf(t, 1, res.Spawned,
		"the template child must be admitted through executeSpawn: %+v", res.Agents)
	require.Len(t, res.Agents, 1)

	row, err := db.FindSessionByConvID(res.Agents[0].ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, harness.ClaudeSandboxOff, row.HarnessBuiltinMode,
		"the template path must persist the forced single-wall mode too")
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), row.SandboxImplementation)
}

// PR2 admits Copilot in one pair only, so the UNPROVEN Copilot child stays
// refused — including from the most permissive parent there is, which is where
// a rule expressed as a child arm would have failed, since that parent's arm
// returns true for every child before any arm could run.
//
// The admitted pair's own flows live in copilot_spawn_lineage_flow_test.go.
func TestSpawnSandboxLineage_UnprovenCopilotChildStillRestricted(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-lyr4-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent,
		harness.DefaultName, harness.ClaudeSandboxOff)

	// No implementation: the legacy/harness-builtin row, where Copilot's own
	// experimental wall is the only one named and tclaude has no lever for it.
	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name":     "worker",
		"harness":  harness.CopilotName,
		"sandbox":  harness.CopilotSandboxInherit,
		"approval": harness.CopilotApprovalAllowTools,
	})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "sandbox_restricted")

	// `stacked` reaches the guard as a tclaude-layer variant and must NOT
	// borrow the proven pair's admission: it runs Copilot's own policy nested
	// inside tclaude's, an intersection nobody reviewed.
	stacked := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name":                   "stacked-worker",
		"harness":                harness.CopilotName,
		"sandbox_implementation": string(sandboxpolicy.ImplementationStacked),
		"approval":               harness.CopilotApprovalAllowTools,
	})
	require.NotEqualf(t, http.StatusOK, stacked.Code, "spawn body=%s", stacked.Raw)
}
