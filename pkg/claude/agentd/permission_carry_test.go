package agentd

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"testing"
)

func TestPermissionCarryStandingPrecedence(t *testing.T) {
	rows := []agentbundle.Permission{
		{Slug: PermGroupsMembersSpawn, Effect: "grant", Source: "agent", Scope: json.RawMessage(`{"group":["source"]}`)},
		{Slug: PermGroupsMembersSpawn, Effect: "grant", Source: "defaults"},
		{Slug: PermGroupsMembersSpawn, Effect: "grant", Source: "profile:old"},
		{Slug: PermAgentStop, Effect: "grant", Source: "agent", Scope: json.RawMessage(`{"target_agent":["old-id"]}`)},
		{Slug: PermAgentStop, Effect: "grant", Source: "defaults"},
		{Slug: PermHumanNotify, Effect: "grant", Source: "profile:old"},
	}
	standing := standingCarryRows(rows)
	require.Len(t, standing, 2)
	decisions, overrides := planCarriedPermissions(rows, permissionCarryPolicy{Enabled: true, AllowSensitive: true, Group: "receiver"})
	require.JSONEq(t, `{"group":["receiver"]}`, overrides[PermGroupsMembersSpawn].Scope)
	require.NotContains(t, overrides, PermAgentStop, "unmappable override must not fall back to a broad default")
	require.NotContains(t, overrides, PermHumanNotify, "revoked historical profile permission must not return")
	require.Equal(t, "drop", decisions[1].Decision)
	require.Equal(t, "drop", decisions[2].Decision)
}

func TestPermissionCarryGroupTierKeepsGroupBoundary(t *testing.T) {
	rows := []agentbundle.Permission{{Slug: PermGroupsMembersSpawn, Effect: "grant", Source: "group:source"}}
	_, overrides := planCarriedPermissions(rows, permissionCarryPolicy{Enabled: true, Group: "receiver"})
	require.JSONEq(t, `{"group":["receiver"]}`, overrides[PermGroupsMembersSpawn].Scope)
}
