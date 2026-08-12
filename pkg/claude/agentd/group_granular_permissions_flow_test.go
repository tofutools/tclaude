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

func patchGroupSettingAsAgent(t *testing.T, f *testharness.Flow, conv string, body map[string]any) (int, string) {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch,
		"/v1/groups/alpha", body), conv)
	rec := testharness.Serve(f.Mux, r)
	return rec.Code, rec.Body.String()
}

func TestGroupSettings_DedicatedDefaultDirGrantDoesNotAuthorizeRename(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const conv = "granular-dir-aaaa-bbbb-1111"
	f.HaveConvWithTitle(conv, "dir-editor")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsDefaultDir, "test"))

	code, body := patchGroupSettingAsAgent(t, f, conv, map[string]any{"default_cwd": t.TempDir()})
	require.Equal(t, http.StatusOK, code, body)

	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/rename", map[string]any{"new_name": "renamed"}), conv)
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermGroupsRename)
}

func TestGroupSettings_RenameGrantAuthorizesOnlyRename(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const conv = "granular-rename-bbbb-2222"
	f.HaveConvWithTitle(conv, "rename-editor")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsRename, "test"))

	code, body := patchGroupSettingAsAgent(t, f, conv, map[string]any{"default_cwd": t.TempDir()})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermGroupsDefaultDir)
}

func TestGroupSettings_AdminUmbrellaAndDedicatedDenyPrecedence(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const conv = "groups-admin-cccc-dddd-3333"
	f.HaveConvWithTitle(conv, "group-admin")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermGroupsAdmin, "test"))

	code, body := patchGroupSettingAsAgent(t, f, conv, map[string]any{"descr": "managed"})
	require.Equal(t, http.StatusOK, code, body)

	require.NoError(t, db.SetAgentPermissionOverride(conv, agentd.PermGroupsDefaultDir, db.PermEffectDeny, "test"))
	code, body = patchGroupSettingAsAgent(t, f, conv, map[string]any{"default_cwd": t.TempDir()})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermGroupsDefaultDir)

	// The deny narrows only this operation; the umbrella still covers others.
	code, body = patchGroupSettingAsAgent(t, f, conv, map[string]any{"max_members": 4})
	assert.Equal(t, http.StatusOK, code, body)
}

func TestGroupSettings_OneTimeApprovalUsesDedicatedPermission(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	approvalCalls, restoreApproval := agentd.StubCountingApprovalForTest(true)
	t.Cleanup(restoreApproval)

	f := newFlow(t)
	f.HaveGroup("alpha")
	const conv = "granular-approval-dddd-4444"
	f.HaveConvWithTitle(conv, "temporary-editor")
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch,
		"/v1/groups/alpha", map[string]any{"default_cwd": t.TempDir()}), conv)
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, approvalCalls(), "one dedicated setting should need one approval")
}
