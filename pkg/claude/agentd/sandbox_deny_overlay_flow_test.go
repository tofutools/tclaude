package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Child sandbox step 3 (tcl-ma3551), tcl-a0ba04 rows 6 and 10: a spawner adds
// denies for its child with sandbox_deny. The overlay covers each path's whole
// subtree, is stored with the child, kept across its relaunches, and inherited
// by its own children with their denies added.

// denyFixture is a write root holding a secrets directory, with a profile
// "mine" that grants the root and, more specifically, a directory inside the
// secrets.
type denyFixture struct {
	root, secrets, inner string
}

func newDenyFixture(t *testing.T) denyFixture {
	t.Helper()
	root := testutil.CanonicalTempDir(t)
	fx := denyFixture{root: root, secrets: filepath.Join(root, "secrets"), inner: filepath.Join(root, "secrets", "inner")}
	require.NoError(t, os.MkdirAll(fx.inner, 0o755))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "mine", Filesystem: []db.SandboxFilesystemGrant{
		{Path: fx.root, Access: "write"}, {Path: fx.inner, Access: "write"},
	}})
	require.NoError(t, err)
	return fx
}

func snapshotAccess(t *testing.T, conv, path string) string {
	t.Helper()
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Path == path {
			return string(grant.Access)
		}
	}
	return ""
}

func denyNotice(t *testing.T, policy *sandboxpolicy.Snapshot) string {
	t.Helper()
	for _, notice := range policy.Effective.AccessNotices {
		if notice.Reason == sandboxpolicy.AccessNoticeReasonDenyOverlayRemoved {
			return notice.Detail
		}
	}
	return ""
}

func spawnDenied(t *testing.T, f *testharness.Flow, name string, extra map[string]any) testharness.SpawnResp {
	t.Helper()
	body := map[string]any{"name": name, "cwd": t.TempDir(), "sandbox_profile": "mine", "sandbox": "on"}
	for k, v := range extra {
		body[k] = v
	}
	return f.AsHuman().SpawnWith("crew", body)
}

// A grant more specific than a denied path is removed, not kept by
// specificity, and the launch names what was removed.
func TestSandboxDenyRemovesANestedGrantAtSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	fx := newDenyFixture(t)

	spawn := spawnDenied(t, f, "denied", map[string]any{"sandbox_deny": []string{fx.secrets}})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Equal(t, "deny", snapshotAccess(t, spawn.ConvID, fx.secrets))
	assert.Equal(t, "", snapshotAccess(t, spawn.ConvID, fx.inner), "the write grant inside the denied path is gone")
	assert.Equal(t, "write", snapshotAccess(t, spawn.ConvID, fx.root), "the grant above it is narrowed, not removed")
	policy := childPolicy(t, f, spawn.ConvID)
	assert.Equal(t, []string{fx.secrets}, policy.DenyOverlay, "the overlay is stored with the agent")
	assert.Contains(t, denyNotice(t, policy), "write "+fx.inner, "the launch names what was removed")

	plain := spawnDenied(t, f, "plain", nil)
	require.Equalf(t, http.StatusOK, plain.Code, "spawn body=%s", plain.Raw)
	assert.Equal(t, "write", snapshotAccess(t, plain.ConvID, fx.inner), "without the overlay the profile is as authored")
}

// Row 10: the overlay is the agent's configuration, kept by its relaunches. A
// profile edit that adds a grant beneath a denied path is removed again on
// the next relaunch rather than quietly reopening it.
func TestSandboxDenySurvivesAProfileEditOnRelaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	fx := newDenyFixture(t)
	spawn := spawnDenied(t, f, "denied", map[string]any{"sandbox_deny": []string{fx.secrets}})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	added := filepath.Join(fx.secrets, "added")
	require.NoError(t, os.MkdirAll(added, 0o755))
	profile, err := db.GetSandboxProfile("mine")
	require.NoError(t, err)
	profile.Filesystem = append(profile.Filesystem, db.SandboxFilesystemGrant{Path: added, Access: "read"})
	require.NoError(t, db.UpdateSandboxProfile(profile))

	resumeForPolicy(t, f, spawn.ConvID)
	policy := childPolicy(t, f, spawn.ConvID)
	assert.Equal(t, []string{fx.secrets}, policy.DenyOverlay)
	assert.Equal(t, "deny", snapshotAccess(t, spawn.ConvID, fx.secrets))
	assert.Equal(t, "", snapshotAccess(t, spawn.ConvID, added), "the edit's grant beneath the deny is removed")
	assert.Contains(t, denyNotice(t, policy), "read "+added)

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	resumeForPolicy(t, f, spawn.ConvID)
	policy = childPolicy(t, f, spawn.ConvID)
	assert.Equal(t, []string{fx.secrets}, policy.DenyOverlay, "reuse keeps it too")
	assert.Equal(t, "deny", snapshotAccess(t, spawn.ConvID, fx.secrets))
	assert.NotEmpty(t, denyNotice(t, policy), "and still says what it removed")
}

// Row 6: a child's overlay is its parent's plus its own, into its parent's
// group or another, so denies accumulate down the lineage.
func TestSandboxDenyAccumulatesDownTheLineage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	f.HaveGroup("other")
	fx := newDenyFixture(t)
	second := filepath.Join(fx.root, "second")
	require.NoError(t, os.MkdirAll(second, 0o755))
	parent := spawnDenied(t, f, "parent", map[string]any{"sandbox_deny": []string{fx.secrets}})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermAgentSpawn, "test"))

	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "cwd": t.TempDir(), "sandbox": "on", "sandbox_deny": []string{second}})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assert.ElementsMatch(t, []string{fx.secrets, second}, childPolicy(t, f, child.ConvID).DenyOverlay)
	assert.Equal(t, "", snapshotAccess(t, child.ConvID, fx.inner), "the inherited deny still removes the nested grant")

	plain := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{"name": "plain-child", "cwd": t.TempDir(), "sandbox": "on"})
	require.Equalf(t, http.StatusOK, plain.Code, "spawn body=%s", plain.Raw)
	assert.Equal(t, []string{fx.secrets}, childPolicy(t, f, plain.ConvID).DenyOverlay, "a child naming none keeps its parent's")

	elsewhere := f.AsAgent(parent.ConvID).SpawnWith("other", map[string]any{"name": "elsewhere", "cwd": t.TempDir(), "sandbox": "on"})
	require.Equalf(t, http.StatusOK, elsewhere.Code, "spawn body=%s", elsewhere.Raw)
	assert.Equal(t, []string{fx.secrets}, childPolicy(t, f, elsewhere.ConvID).DenyOverlay, "so does one spawned into another group")
}

// The overlay fails closed: a launch that would not enforce it is refused,
// whether the overlay is the request's own or inherited, and so is a denied
// path the launch would reopen as the working directory.
func TestSandboxDenyRefusesALaunchThatWouldNotEnforceIt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	fx := newDenyFixture(t)

	off := spawnDenied(t, f, "off", map[string]any{"sandbox_deny": []string{fx.secrets}, "sandbox_implementation": "off"})
	assert.Equalf(t, http.StatusForbidden, off.Code, "spawn body=%s", off.Raw)
	assert.Contains(t, string(off.Raw), "enforce")

	omitted := f.AsHuman().SpawnWith("crew", map[string]any{"name": "omitted", "cwd": t.TempDir(),
		"omit_sandbox_profiles": true, "sandbox_deny": []string{fx.secrets}})
	assert.Equalf(t, http.StatusBadRequest, omitted.Code, "spawn body=%s", omitted.Raw)

	covering := spawnDenied(t, f, "covering", map[string]any{"sandbox_deny": []string{fx.root}, "cwd": fx.inner})
	assert.Equalf(t, http.StatusBadRequest, covering.Code, "spawn body=%s", covering.Raw)
	assert.Contains(t, string(covering.Raw), "covers the working directory")

	relative := spawnDenied(t, f, "relative", map[string]any{"sandbox_deny": []string{"relative/path"}})
	assert.Equalf(t, http.StatusBadRequest, relative.Code, "spawn body=%s", relative.Raw)

	parent := spawnDenied(t, f, "parent", map[string]any{"sandbox_deny": []string{fx.secrets}})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	inherited := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "inherited-off", "cwd": t.TempDir(), "sandbox": "on", "sandbox_implementation": "off"})
	assert.NotEqualf(t, http.StatusOK, inherited.Code, "an inherited overlay binds too: body=%s", inherited.Raw)
}
