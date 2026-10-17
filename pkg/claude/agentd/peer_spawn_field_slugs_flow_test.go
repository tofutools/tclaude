package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
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
