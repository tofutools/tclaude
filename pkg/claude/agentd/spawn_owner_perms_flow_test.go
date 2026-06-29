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

// Flow coverage for the birth-time access controls the spawn dialog's "Group
// owner" checkbox + permission editor add: a spawn request may carry
// is_owner + permission_overrides, which enrollSpawnedConv applies right after
// the membership add. We assert at the same real surfaces the dashboard reads —
// db.ListAgentGroupOwners and db.ListAgentPermissionOverridesForConv — not the
// simulator internals.

// ownsGroup reports whether conv is among the group's recorded owners.
func ownsGroup(t *testing.T, groupID int64, conv string) bool {
	t.Helper()
	owners, err := db.ListAgentGroupOwners(groupID)
	require.NoError(t, err)
	for _, o := range owners {
		if o.ConvID == conv {
			return true
		}
	}
	return false
}

// Scenario: a human spawn with is_owner + permission_overrides makes the new
// agent a group owner and stamps its per-slug overrides at enrollment — the
// agent's first turn already has them.
func TestSpawnOwnerPerms_HumanAppliesOwnerAndOverrides(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":     "lead",
		"is_owner": true,
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "grant",
			"self.rename":          "deny",
		},
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv-id")

	// Ownership landed on the new conv — the surface the dashboard's owner
	// badge + the daemon's owner-bypass read.
	assert.True(t, ownsGroup(t, g.ID, spawn.ConvID), "spawned agent is a group owner")

	// The per-slug overrides are real persisted rows.
	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn], "groups.spawn granted at birth")
	assert.Equal(t, "deny", overrides["self.rename"], "self.rename denied at birth")
}

// Scenario: a plain spawn (no is_owner / overrides) enrolls an ordinary member
// — the controls are opt-in, so nothing is granted by default.
func TestSpawnOwnerPerms_PlainSpawnGrantsNothing(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	assert.False(t, ownsGroup(t, g.ID, spawn.ConvID), "plain spawn is not an owner")
	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Empty(t, overrides, "plain spawn carries no overrides")
}

// Scenario: a "default" effect is a no-op — the editor posts every slug, most
// at Default, and the daemon drops them so only real grants/denies persist.
func TestSpawnOwnerPerms_DefaultEffectDropped(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker",
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "grant",
			"self.rename":          "default",
			"self.compact":         "",
		},
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{agentd.PermGroupsSpawn: "grant"}, overrides,
		"only the real grant persists; default/'' are dropped")
}

// Scenario: an unknown slug is rejected at the boundary (400) and nothing is
// spawned — fail fast before any subprocess launches.
func TestSpawnOwnerPerms_UnknownSlugRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                 "worker",
		"permission_overrides": map[string]any{"not.a.real.slug": "grant"},
	})
	assert.Equalf(t, http.StatusBadRequest, spawn.Code,
		"unknown slug should 400; body=%s", spawn.Raw)
}

// Scenario: a bad effect (not grant/deny/default) is a 400.
func TestSpawnOwnerPerms_BadEffectRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                 "worker",
		"permission_overrides": map[string]any{agentd.PermGroupsSpawn: "sudo"},
	})
	assert.Equalf(t, http.StatusBadRequest, spawn.Code,
		"bad effect should 400; body=%s", spawn.Raw)
}

// Scenario: escalation gate — an agent caller that holds groups.spawn but holds
// neither groups.own nor permissions.grant cannot mint an owner or grant slugs
// at spawn. A spawn must confer no MORE authority than the post-spawn path, so
// each portion is gated on the SAME slug the dedicated endpoint requires.
func TestSpawnOwnerPerms_AgentWithoutGrantSlugsForbidden(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// A spawner agent enrolled with groups.spawn ONLY.
	const spawner = "spwn-1111-2222-3333-4444"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsSpawn, "test"), "grant groups.spawn")

	owner := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name":     "henchman",
		"is_owner": true,
	})
	assert.Equalf(t, http.StatusForbidden, owner.Code,
		"minting an owner without groups.own should 403; body=%s", owner.Raw)

	perms := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name":                 "henchman",
		"permission_overrides": map[string]any{agentd.PermGroupsSpawn: "grant"},
	})
	assert.Equalf(t, http.StatusForbidden, perms.Code,
		"granting slugs without permissions.grant should 403; body=%s", perms.Raw)
}

// Scenario: GROUP OWNERSHIP ALONE is NOT sufficient — owning a group confers the
// owner-implied lifecycle slugs (groups.spawn/…) but NOT groups.own or
// permissions.grant. So an owner that lacks those two slugs still can't mint a
// child owner or grant it slugs; otherwise ownership of one group would let a
// lead spawn a child holding permissions.grant and escalate globally. This is
// the regression guarding that boundary.
func TestSpawnOwnerPerms_OwnershipAloneInsufficient(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	// An OWNER of alpha (so it passes the spawn via owner-bypass) that holds
	// neither groups.own nor permissions.grant.
	const spawner = "ownr-1111-2222-3333-4444"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, spawner, "test"), "make spawner an owner")

	owner := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name":     "heir",
		"is_owner": true,
	})
	assert.Equalf(t, http.StatusForbidden, owner.Code,
		"an owner without groups.own must NOT mint a child owner; body=%s", owner.Raw)

	perms := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name":                 "heir",
		"permission_overrides": map[string]any{agentd.PermPermissionsGrant: "grant"},
	})
	assert.Equalf(t, http.StatusForbidden, perms.Code,
		"an owner without permissions.grant must NOT grant slugs (esp. permissions.grant); body=%s", perms.Raw)
}

// Scenario: an agent that DOES hold the required slugs may apply the controls —
// the spawn-time path mirrors the dedicated endpoints exactly.
func TestSpawnOwnerPerms_AgentWithGrantSlugsAllowed(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	const spawner = "good-1111-2222-3333-4444"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsSpawn, "test"), "grant groups.spawn")
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsOwn, "test"), "grant groups.own")
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermPermissionsGrant, "test"), "grant permissions.grant")

	spawn := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name":     "deputy",
		"is_owner": true,
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "grant",
		},
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "authorised agent spawn body=%s", spawn.Raw)
	assert.True(t, ownsGroup(t, g.ID, spawn.ConvID), "deputy spawned as a group owner")
	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn], "deputy granted groups.spawn")
}

// Scenario: a saved spawn profile round-trips the owner default + overrides
// through the /v1/spawn-profiles API, so it can pre-fill the spawn dialog.
func TestSpawnOwnerPerms_ProfileRoundTrip(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name":     "lead-team",
		"is_owner": true,
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "grant",
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profiles/lead-team", nil))
	got := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, got.Code, "get profile body=%s", got.Body.String())
	body := got.Body.String()
	assert.Contains(t, body, `"is_owner":true`, "profile carries the owner default")
	assert.Contains(t, body, `"permission_overrides"`, "profile carries the overrides")
	assert.Contains(t, body, agentd.PermGroupsSpawn, "profile names the granted slug")
}
