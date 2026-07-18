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

func TestSpawnApprovalLineage_Matrix(t *testing.T) {
	tests := []struct {
		name                        string
		parentHarness, parentPolicy string
		parentAutoReview            bool
		childHarness, childPolicy   string
		childAutoReview             bool
		wantStatus                  int
	}{
		// TCL-576 required allows — the postures that were wrongly blocked by the
		// old per-direction rules. Codex `never` is the safe unattended posture
		// and Claude `auto` is in-sandbox review, not a boundary-escalation grant.
		{"codex never can mint claude auto", harness.CodexName, harness.ApprovalNever, false, harness.DefaultName, "auto", false, http.StatusOK},
		{"codex never leaves guardian idle and still mints claude auto", harness.CodexName, harness.ApprovalNever, true, harness.DefaultName, "auto", false, http.StatusOK},
		{"codex guardian can mint claude auto", harness.CodexName, harness.ApprovalOnRequest, true, harness.DefaultName, "auto", false, http.StatusOK},
		{"codex guardian can mint accept edits", harness.CodexName, harness.ApprovalOnRequest, true, harness.DefaultName, "acceptEdits", false, http.StatusOK},
		{"claude inherit can mint identical inherit", harness.DefaultName, harness.ClaudePermissionInherit, false, harness.DefaultName, harness.ClaudePermissionInherit, false, http.StatusOK},
		{"claude inherit can mint proven baseline plan", harness.DefaultName, harness.ClaudePermissionInherit, false, harness.DefaultName, "plan", false, http.StatusOK},
		{"claude inherit cannot mint explicit claude auto", harness.DefaultName, harness.ClaudePermissionInherit, false, harness.DefaultName, "auto", false, http.StatusForbidden},
		{"claude inherit cannot mint codex never", harness.DefaultName, harness.ClaudePermissionInherit, false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"claude auto can mint codex never", harness.DefaultName, "auto", false, harness.CodexName, harness.ApprovalNever, false, http.StatusOK},

		// Bypass and unresolvable-inherit children stay gated.
		{"codex human-gated cannot mint claude bypass", harness.CodexName, harness.ApprovalOnRequest, false, harness.DefaultName, "bypassPermissions", false, http.StatusForbidden},
		{"claude bypass can mint claude bypass", harness.DefaultName, "bypassPermissions", false, harness.DefaultName, "bypassPermissions", false, http.StatusOK},
		{"claude auto cannot mint claude bypass", harness.DefaultName, "auto", false, harness.DefaultName, "bypassPermissions", false, http.StatusForbidden},
		{"codex never cannot mint unresolvable claude inherit", harness.CodexName, harness.ApprovalNever, false, harness.DefaultName, harness.ClaudePermissionInherit, false, http.StatusForbidden},
		{"claude auto cannot mint unresolvable claude inherit", harness.DefaultName, "auto", false, harness.DefaultName, harness.ClaudePermissionInherit, false, http.StatusForbidden},

		// Genuinely broader capability is still denied, in both directions.
		// acceptEdits auto-approves EDITS only; a child that runs arbitrary
		// commands unattended is a real escalation even though both are
		// "automatic".
		{"accept edits cannot mint codex never", harness.DefaultName, "acceptEdits", false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"accept edits cannot mint claude auto", harness.DefaultName, "acceptEdits", false, harness.DefaultName, "auto", false, http.StatusForbidden},
		{"accept edits cannot mint codex guardian", harness.DefaultName, "acceptEdits", false, harness.CodexName, harness.ApprovalOnRequest, true, http.StatusForbidden},
		{"claude auto cannot delegate codex guardian review", harness.DefaultName, "auto", false, harness.CodexName, harness.ApprovalOnRequest, true, http.StatusForbidden},
		{"claude default cannot delegate codex in-sandbox execution", harness.DefaultName, "default", false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"codex untrusted cannot delegate claude auto", harness.CodexName, harness.ApprovalUntrusted, false, harness.DefaultName, "auto", false, http.StatusForbidden},

		// Same-harness Codex lineage is unchanged.
		{"codex baseline can mint codex baseline", harness.CodexName, harness.ApprovalOnRequest, false, harness.CodexName, harness.ApprovalNever, false, http.StatusOK},
		{"codex untrusted cannot mint codex never", harness.CodexName, harness.ApprovalUntrusted, false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"codex untrusted can mint codex untrusted", harness.CodexName, harness.ApprovalUntrusted, false, harness.CodexName, harness.ApprovalUntrusted, false, http.StatusOK},
		{"codex guardian can mint codex guardian", harness.CodexName, harness.ApprovalOnRequest, true, harness.CodexName, harness.ApprovalUntrusted, true, http.StatusOK},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			parent := fmt.Sprintf("approval-parent-%04d-aaaa-bbbb-cccccccccccc", i)
			haveSpawnCapableApprovalParent(t, f, "alpha", parent, tt.parentHarness, tt.parentPolicy, tt.parentAutoReview)

			childSandbox := harness.ClaudeSandboxOff
			if tt.childHarness == harness.CodexName {
				childSandbox = harness.SandboxDangerFull
			}
			resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
				"name":        "worker",
				"harness":     tt.childHarness,
				"sandbox":     childSandbox,
				"approval":    tt.childPolicy,
				"auto_review": tt.childAutoReview,
			})
			require.Equalf(t, tt.wantStatus, resp.Code, "spawn body=%s", resp.Raw)
			if tt.wantStatus == http.StatusForbidden {
				assert.Contains(t, string(resp.Raw), "approval_restricted")
			}
		})
	}
}

// TCL-576: an unresolvable `inherit` child is the one denial a caller hits by
// accident, so the refusal must name the explicit mode that works instead of
// reading as a dead end.
func TestSpawnApprovalLineage_InheritChildDenialNamesTheWayOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-hint-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, harness.ApprovalNever, false)

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": harness.DefaultName,
		"sandbox": harness.ClaudeSandboxOff, "approval": harness.ClaudePermissionInherit,
	})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "approval_restricted")
	assert.Contains(t, string(resp.Raw), "cannot be proven at spawn time")
	assert.Contains(t, string(resp.Raw), "auto", "the denial must name the explicit mode that works")
}

// TCL-576: the reported failure came in through a saved profile
// (`fable[1m]-high`), so the profile path must resolve to the same effective
// posture as the equivalent explicit flag and be allowed identically.
func TestSpawnApprovalLineage_ProfileResolvedAutoMatchesExplicitAuto(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"explicit flag", map[string]any{"approval": "auto"}},
		{"saved profile", map[string]any{"profile": "claude-auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "claude-auto", "harness": harness.DefaultName,
				"sandbox": harness.ClaudeSandboxOff, "approval": "auto",
			}).Code)
			const parent = "approval-profile-auto-aaaa-bbbb-cccccccccccc"
			haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, harness.ApprovalNever, false)

			body := map[string]any{
				"name": "worker", "harness": harness.DefaultName,
				"sandbox": harness.ClaudeSandboxOff,
			}
			for key, value := range tc.body {
				body[key] = value
			}
			resp := f.AsAgent(parent).SpawnWith("alpha", body)
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
			approval, ok := f.World.SpawnApproval(resp.ConvID)
			require.True(t, ok)
			assert.Equal(t, "auto", approval, "both paths must land on the same effective posture")
		})
	}
}

func TestSpawnApprovalLineage_MissingParentSessionFailsClosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-missing-parent-aaaa-bbbb-cccccccccccc"
	f.HaveMember("alpha", parent)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermGroupsSpawn, "test"))

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", map[string]any{
		"name": "worker", "harness": harness.DefaultName,
		"sandbox": harness.ClaudeSandboxInherit, "approval": "default",
	})
	require.Equalf(t, http.StatusForbidden, rec.Code, "spawn body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "approval_restricted")
	assert.Contains(t, rec.Body.String(), "no recorded launch approval posture")
}

func TestSpawnApprovalLineage_LegacyCodexDefaultIsReconstructedFromSpawnProvenance(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-legacy-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, "", false)
	agentID, err := db.AgentIDForConv(parent)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	require.NoError(t, db.SetAgentInitialSpawnConfig(agentID, `{"harness":"codex"}`))

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": harness.CodexName,
		"sandbox": harness.SandboxDangerFull,
	})
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
}

func TestSpawnApprovalLineage_AmbiguousLegacyCodexPolicyStillFailsClosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-ambiguous-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, "", false)
	agentID, err := db.AgentIDForConv(parent)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentInitialSpawnConfig(agentID,
		`{"harness":"codex","approval":"untrusted"}`))

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": harness.CodexName,
		"sandbox": harness.SandboxDangerFull, "approval": harness.ApprovalUntrusted,
	})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "cannot be reconstructed")
	assert.Contains(t, string(resp.Raw), "relaunch")
}

func TestSpawnApprovalLineage_CompatiblePolicyAfterEveryProfileTierResolves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *testharness.Flow)
		body  map[string]any
	}{
		{"harness default", func(*testing.T, *testharness.Flow) {}, map[string]any{}},
		{"explicit policy", func(*testing.T, *testharness.Flow) {}, map[string]any{"approval": harness.ApprovalNever}},
		{"named profile", func(t *testing.T, f *testharness.Flow) {
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "compatible", "harness": harness.CodexName, "approval": harness.ApprovalNever,
			}).Code)
		}, map[string]any{"profile": "compatible"}},
		{"group default profile", func(t *testing.T, f *testharness.Flow) {
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "compatible", "harness": harness.CodexName, "approval": harness.ApprovalNever,
			}).Code)
			require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "compatible").Code)
		}, map[string]any{}},
		{"global default profile", func(t *testing.T, f *testharness.Flow) {
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "compatible", "harness": harness.CodexName, "approval": harness.ApprovalNever,
			}).Code)
			require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "compatible").Code)
		}, map[string]any{}},
		{"dashboard repaired legacy profile", func(t *testing.T, f *testharness.Flow) {
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "dashboard", "harness": harness.CodexName,
			}).Code)
			rec := profileReq(t, f, http.MethodPatch, "/v1/spawn-profiles/dashboard", map[string]any{
				"name": "dashboard", "harness": harness.CodexName, "approval": harness.ApprovalNever,
			})
			require.Equalf(t, http.StatusOK, rec.Code, "profile patch body=%s", rec.Body.String())
		}, map[string]any{"profile": "dashboard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			tc.setup(t, f)
			const parent = "approval-tier-parent-aaaa-bbbb-cccccccccccc"
			haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, "", false)
			agentID, err := db.AgentIDForConv(parent)
			require.NoError(t, err)
			require.NoError(t, db.SetAgentInitialSpawnConfig(agentID, `{"harness":"codex"}`))

			body := map[string]any{
				"name": "worker", "harness": harness.CodexName,
				"sandbox": harness.SandboxDangerFull,
			}
			for key, value := range tc.body {
				body[key] = value
			}
			resp := f.AsAgent(parent).SpawnWith("alpha", body)
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
			approval, ok := f.World.SpawnApproval(resp.ConvID)
			require.True(t, ok)
			assert.Equal(t, harness.ApprovalNever, approval)
		})
	}
}

func TestSpawnApprovalLineage_TemplateInstantiateRejectsBypass(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-template-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, harness.ApprovalNever, false)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "approval-bypass-template",
		"agents": []map[string]any{{
			"name": "worker", "harness": harness.DefaultName,
			"sandbox": harness.ClaudeSandboxOff, "approval": "bypassPermissions",
		}},
	}).Code)
	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/templates/approval-bypass-template/instantiate",
		map[string]any{"group_name": "approval-bypass-cast", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 0, res.Spawned)
	assert.Equal(t, 1, res.Failed)
	require.Len(t, res.Agents, 1)
	assert.Contains(t, res.Agents[0].Error, "may not spawn")
}

func TestSpawnApprovalLineage_ProfileDerivedBypassRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-profile-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, harness.ApprovalNever, false)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "unsafe-approval", "harness": harness.DefaultName,
		"sandbox": harness.ClaudeSandboxOff, "approval": "bypassPermissions",
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "unsafe-approval").Code)

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusForbidden, resp.Code, "spawn body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "approval_restricted")
}

func TestSpawnApprovalLineage_AgentScribeSummonRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "approval-scribe-parent-aaaa-bbbb-cccccccccccc"
	haveSpawnCapableApprovalParent(t, f, "alpha", parent, harness.CodexName, harness.ApprovalNever, false)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermPermissionsGrant, "test"))

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "lineage-scribe", "slugs": []string{agentd.PermTemplatesManage},
		"brief": "Author the requested template without exceeding the caller's authority.",
	})
	require.Equalf(t, http.StatusForbidden, rec.Code, "scribe summon body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "approval_restricted")
}

func haveSpawnCapableApprovalParent(t *testing.T, f *testharness.Flow, group, convID, h, approval string, autoReview bool) {
	t.Helper()
	f.HaveMember(group, convID)
	require.NoError(t, db.GrantAgentPermission(convID, agentd.PermGroupsSpawn, "test"))
	sandbox := harness.ClaudeSandboxOff
	if h == harness.CodexName {
		sandbox = harness.SandboxDangerFull
	}
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + convID, TmuxSession: "tmux-" + convID,
		ConvID: convID, Cwd: f.World.HomeDir, Status: "running",
		Harness: h, SandboxMode: sandbox,
		ApprovalPolicy: approval, ApprovalAutoReview: autoReview,
	}))
}
