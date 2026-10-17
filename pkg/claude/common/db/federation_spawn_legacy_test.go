package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A node profile saved with the replaced allowed_overrides list applies the
// field grants that took its place, and the list is never written back.
func TestNodeProfileExpandsLegacySpawnOverrides(t *testing.T) {
	var spec FederationNodeProfileSpec
	require.NoError(t, json.Unmarshal([]byte(`{"trust_level":"restricted","labels":[],"peer_grants":[
		{"peer":"","slug":"groups.members.spawn","scope":"group=1","spawn_policy":{"max_live":2,"allowed_overrides":["model","bogus"]}},
		{"peer":"","slug":"groups.members.spawn.model","scope":"group=1","spawn_policy":{}}]}`), &spec))
	var slugs []string
	for _, g := range spec.PeerGrants {
		slugs = append(slugs, g.Slug+"@"+g.Scope)
		require.Empty(t, g.SpawnPolicy.LegacyAllowedOverrides)
	}
	require.Equal(t, []string{"groups.members.spawn@group=1", "groups.members.spawn.model@group=1"}, slugs)

	var policy FederationSpawnPolicy
	require.NoError(t, json.Unmarshal([]byte(`{"max_live":2,"allowed_overrides":["cwd"]}`), &policy))
	require.Equal(t, []string{"cwd"}, policy.LegacyAllowedOverrides, "decoded, so a request still sending it can be refused")
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	require.JSONEq(t, `{"max_live":2}`, string(raw))
}

// An assignment that managed a spawn grant with a legacy list also manages
// the field grants the list became, so the profile can take them away.
func TestNodeProfileAssignmentManagesExpandedFieldGrants(t *testing.T) {
	var a FederationNodeProfileAssignment
	require.NoError(t, json.Unmarshal([]byte(`{"profile":{"definition":{"trust_level":"restricted","labels":[],"peer_grants":[
		{"peer":"","slug":"groups.members.spawn","scope":"group=1","spawn_policy":{"allowed_overrides":["model"]}}]}},
		"managed_pools":{},"managed_grants":{"groups.members.spawn\ngroup=1":true}}`), &a))
	require.True(t, a.ManagedGrants["groups.members.spawn.model\ngroup=1"])
	var none FederationNodeProfileAssignment
	require.NoError(t, json.Unmarshal([]byte(`{"profile":{"definition":{"trust_level":"restricted","labels":[],"peer_grants":[
		{"peer":"","slug":"groups.members.spawn","scope":"group=1","spawn_policy":{"allowed_overrides":["model"]}}]}}}`), &none))
	require.Empty(t, none.ManagedGrants, "an unmanaged spawn grant adopts nothing")
}
