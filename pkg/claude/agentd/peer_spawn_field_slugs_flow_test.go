package agentd_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Roles and an explicit sandbox profile ride on a remote spawn under their
// own grants, validated against this node's libraries, and reach the launch.
func TestPeerSpawnRolesAndSandboxUnderTheirGrants(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	_, err := db.CreateRole(&db.Role{Name: "builder", Brief: "builds"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "tight"})
	require.NoError(t, err)
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grantSpawnFields(t, f, "bob", "builders", nil)

	body := map[string]any{"brief": "task", "roles": []string{"builder"}, "sandbox_profile": "tight"}
	code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", body)
	require.Equal(t, 403, code, "roles and a sandbox profile need their grants")
	require.Equal(t, "override_not_allowed", errCode)

	grantSpawnFields(t, f, "bob", "builders", nil, "roles", "sandbox")
	code, opts := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	require.True(t, opts.Overrides["roles"])
	require.True(t, opts.Overrides["sandbox"])
	require.False(t, opts.Overrides["cwd"])

	for _, bad := range []map[string]any{
		{"brief": "task", "roles": []string{"nobody"}},
		{"brief": "task", "sandbox_profile": "missing"},
		{"brief": "task", "sandbox_profile": "/omit-all-tclaude-sandbox-profiles"},
	} {
		code, _, _ = peerSpawnPost(t, bob.ID(), "builders", bad)
		require.Equal(t, 400, code, "%v names nothing on this node", bad)
	}
	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", body)
	require.Equal(t, 202, code)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, db.FederationSpawnOverrides{Roles: []string{"builder"}, SandboxProfile: "tight"}, req.Overrides)
	waitSpawnSettled(t, id)
}

// groups.members.spawn.all opens every field; .profile opens any enabled,
// non-operator-only profile without an allowlist.
func TestPeerSpawnAllAndProfileGrants(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	f.HaveGroup("other")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "cheap", Harness: "claude", Model: "haiku"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "vip", Harness: "claude", OperatorOnly: true})
	require.NoError(t, err)
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))

	grantSpawnFields(t, f, "bob", "builders", nil, "all")
	code, opts := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	for _, field := range []string{"cwd", "harness", "model", "effort", "profile", "roles", "sandbox"} {
		require.True(t, opts.Overrides[field], field)
	}
	var names []string
	for _, p := range opts.Profiles {
		names = append(names, p.Name)
	}
	require.Contains(t, names, "cheap")
	require.NotContains(t, names, "vip")

	grantSpawnFields(t, f, "bob", "other", nil, "profile")
	code, opts = peerSpawnOptions(t, bob.ID(), "other")
	require.Equal(t, 200, code)
	require.True(t, opts.Overrides["profile"])
	require.False(t, opts.Overrides["cwd"], "the grants are per field and per group")
	code, id, _ := peerSpawnPost(t, bob.ID(), "other", map[string]any{"brief": "task", "profile": "cheap"})
	require.Equal(t, 202, code)
	waitSpawnSettled(t, id)
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawnAll})
	require.Equal(t, 200, rec.Code, "an unscoped field grant covers every group: %s", rec.Body.String())
}

// A group-scoped spawn grant narrows what a broader field grant opens, as
// its own policy narrows a broader spawn grant's; a field grant alone shows
// the peer nothing; and revoking a spawn grant takes its field grants along.
func TestPeerSpawnFieldGrantsFollowTheSpawnGrant(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("open")
	f.HaveGroup("secrets")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grant := func(method, slug, scope string) {
		t.Helper()
		body := map[string]any{"peer": "bob", "slug": slug}
		if scope != "" {
			body["scope"] = scope
		}
		rec := fedHuman(t, f, method, "/v1/federation/grants", body)
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}

	grant("POST", agentd.PermGroupsMembersSpawnAll, "")
	code, _ := peerSpawnOptions(t, bob.ID(), "open")
	require.Equal(t, 404, code, "a field grant alone makes no group visible")

	grant("POST", agentd.PermGroupsMembersSpawn, "")
	grant("POST", agentd.PermGroupsMembersSpawn, "group=secrets")
	code, opts := peerSpawnOptions(t, bob.ID(), "open")
	require.Equal(t, 200, code)
	require.True(t, opts.Overrides["cwd"])
	code, opts = peerSpawnOptions(t, bob.ID(), "secrets")
	require.Equal(t, 200, code)
	require.False(t, opts.Overrides["cwd"], "the group's own spawn grant opens no field; the unscoped field grant does not reach past it")
	grant("POST", agentd.PermGroupsMembersSpawnCwd, "group=secrets")
	_, opts = peerSpawnOptions(t, bob.ID(), "secrets")
	require.True(t, opts.Overrides["cwd"])
	require.False(t, opts.Overrides["model"])

	grant("DELETE", agentd.PermGroupsMembersSpawn, "group=secrets")
	grants, err := db.ListEffectiveFederationPeerGrants(bob.ID())
	require.NoError(t, err)
	for _, g := range grants {
		require.NotEqual(t, agentd.PermGroupsMembersSpawnCwd, g.Slug, "revoking the spawn grant revokes its field grants")
	}
	_, opts = peerSpawnOptions(t, bob.ID(), "secrets")
	require.True(t, opts.Overrides["cwd"], "the unscoped grants apply again")
}

// The rest of the dialog's launch settings ride as launch keys under their
// own grants, and reach the launch as the dialog sent them.
func TestPeerSpawnLaunchSettingsUnderTheirGrants(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	require.NoError(t, db.SaveSavedPrompt("", db.SavedPrompt{Name: "house", Body: "Follow the house rules."}))
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grantSpawnFields(t, f, "bob", "builders", nil)

	launch := map[string]any{
		"is_owner":    true,
		"environment": []map[string]string{{"name": "BUILD_MODE", "value": "fast"}},
		"descr":       "nightly build",
		"prompt_refs": []string{"house"},
		"auto_memory": false,
	}
	body := map[string]any{"brief": "task", "launch": launch}
	code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", body)
	require.Equal(t, 403, code, "launch settings need their grants")
	require.Equal(t, "override_not_allowed", errCode)

	grantSpawnFields(t, f, "bob", "builders", nil, "owner", "environment", "features", "prompts")
	code, opts := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	require.True(t, opts.Overrides["owner"])
	require.False(t, opts.Overrides["permissions"])
	for _, bad := range []map[string]any{
		{"attachments": []string{"/etc/passwd"}},
		{"break_glass_acknowledged": true},
		{"is_owner": "yes"},
	} {
		code, _, _ = peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "launch": bad})
		require.Equal(t, 400, code, "%v is not a carried launch setting", bad)
	}
	code, _, _ = peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "launch": map[string]any{"approval": "plan"}})
	require.Equal(t, 403, code, "the permission modes need groups.members.spawn.permissions")

	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", body)
	require.Equal(t, 202, code)
	waitSpawnSettled(t, id)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnApproved, req.Status, req.Reason)
	require.NotEmpty(t, req.ResultAgent)

	d, err := db.Open()
	require.NoError(t, err)
	var cfg string
	require.NoError(t, d.QueryRow(`SELECT initial_spawn_config FROM agents WHERE agent_id = ?`, req.ResultAgent).Scan(&cfg))
	var launched agent.SpawnRequest
	require.NoError(t, json.Unmarshal([]byte(cfg), &launched))
	require.True(t, launched.IsOwner)
	require.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "BUILD_MODE", Value: "fast"}}, launched.Environment)
	require.Equal(t, []string{"house"}, launched.PromptRefs)
	require.NotNil(t, launched.AutoMemory)
	require.False(t, *launched.AutoMemory)
	require.True(t, strings.HasPrefix(launched.Descr, "nightly build — spawned for operator@"), launched.Descr)
}

// The worktree grant lends the dialog this node's worktree routes, to list a
// repository's worktrees and create one before the spawn; cleanup and the
// directory picker stay local-only.
func TestPeerSpawnWorktreeRoutesUnderTheirGrant(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	h := agentd.PeerViewHandler(bob.ID())
	repo := testutil.CanonicalTempDir(t)
	list := func() int {
		return testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/worktrees?repo="+repo, nil)).Code
	}
	grantSpawnFields(t, f, "bob", "builders", nil, "cwd")
	require.Equal(t, 403, list(), "worktrees need groups.members.spawn.worktree")
	grantSpawnFields(t, f, "bob", "builders", nil, "worktree")
	require.Equal(t, 200, list())
	carol, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: carol.ID(), PubKey: carol.Pub, Label: "carol"}))
	grantSpawnFields(t, f, "carol", "builders", nil, "worktree")
	require.Equal(t, 403, testharness.Serve(agentd.PeerViewHandler(carol.ID()), testharness.JSONRequest(t, "GET", "/api/worktrees?repo="+repo, nil)).Code,
		"a worktree is a directory too: it needs groups.members.spawn.cwd")
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/worktrees", map[string]any{"repo": repo, "branch": "b", "path": "/tmp/elsewhere"}))
	require.Equal(t, 403, rec.Code, "the peer does not choose where a worktree goes: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "cannot choose where its worktree is created")
	require.NotEqual(t, 403, testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/worktrees/progress?id=x", nil)).Code, "progress reaches the route")
	for _, path := range []string{"/api/worktrees/cleanup", "/api/browse-directories"} {
		require.Equal(t, 403, testharness.Serve(h, testharness.JSONRequest(t, "POST", path, map[string]any{})).Code, path)
	}
	require.Equal(t, 403, testharness.Serve(h, testharness.JSONRequest(t, "DELETE", "/api/worktrees", nil)).Code)
}

// The sandbox-weakening launch settings, which a local spawn takes only from
// the human operator, launch under the peer operator's .permissions grant.
func TestPeerSpawnPermissionsLaunchMayOmitSandboxProfiles(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grantSpawnFields(t, f, "bob", "builders", nil, "permissions")
	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "launch": map[string]any{"omit_sandbox_profiles": true}})
	require.Equal(t, 202, code)
	waitSpawnSettled(t, id)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnApproved, req.Status, req.Reason)
}
