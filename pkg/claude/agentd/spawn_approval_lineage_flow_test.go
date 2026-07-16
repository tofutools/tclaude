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
		{"codex human-gated cannot mint claude bypass", harness.CodexName, harness.ApprovalOnRequest, false, harness.DefaultName, "bypassPermissions", false, http.StatusForbidden},
		{"claude bypass can mint claude bypass", harness.DefaultName, "bypassPermissions", false, harness.DefaultName, "bypassPermissions", false, http.StatusOK},
		{"codex guardian cannot delegate settings-driven claude auto", harness.CodexName, harness.ApprovalOnRequest, true, harness.DefaultName, "auto", false, http.StatusForbidden},
		{"codex never leaves guardian idle", harness.CodexName, harness.ApprovalNever, true, harness.DefaultName, "auto", false, http.StatusForbidden},
		{"codex guardian cannot mint accept edits", harness.CodexName, harness.ApprovalOnRequest, true, harness.DefaultName, "acceptEdits", false, http.StatusForbidden},
		{"accept edits cannot mint codex guardian", harness.DefaultName, "acceptEdits", false, harness.CodexName, harness.ApprovalOnRequest, true, http.StatusForbidden},
		{"claude auto cannot delegate codex in-sandbox execution", harness.DefaultName, "auto", false, harness.CodexName, harness.ApprovalOnRequest, true, http.StatusForbidden},
		{"claude default cannot delegate codex in-sandbox execution", harness.DefaultName, "default", false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"codex baseline can mint codex baseline", harness.CodexName, harness.ApprovalOnRequest, false, harness.CodexName, harness.ApprovalNever, false, http.StatusOK},
		{"codex untrusted cannot mint codex never", harness.CodexName, harness.ApprovalUntrusted, false, harness.CodexName, harness.ApprovalNever, false, http.StatusForbidden},
		{"codex untrusted can mint codex untrusted", harness.CodexName, harness.ApprovalUntrusted, false, harness.CodexName, harness.ApprovalUntrusted, false, http.StatusOK},
		{"codex guardian can mint codex guardian", harness.CodexName, harness.ApprovalOnRequest, true, harness.CodexName, harness.ApprovalUntrusted, true, http.StatusOK},
		{"claude inherit cannot delegate settings-driven inherit", harness.DefaultName, harness.ClaudePermissionInherit, false, harness.DefaultName, harness.ClaudePermissionInherit, false, http.StatusForbidden},
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
