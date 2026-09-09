package model_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestPermissionScopePreservesConjunctionAndCurrentNamedValues(t *testing.T) {
	scope := model.PermissionScope{"group": {"release", "development"}, "spawn_profile": {"reviewer"}, "sandbox_profile": {"confined"}, "process_template": {"ship"}, "remote": {"github.com/acme/*"}, "linear_team": {" tcl "}, "awb_workspace": {"tcl"}, "target_agent": {"agt_worker"}}
	action := model.PermissionContext{Group: "development", SpawnProfile: "reviewer", SandboxProfile: "confined", ProcessTemplate: "ship", Remote: "GITHUB.COM/ACME/REPO", LinearTeam: "TCL", AWBWorkspace: "TCL", TargetAgent: "agt_worker"}
	require.True(t, scope.Matches(action, nil))
	for _, dimension := range []string{"group", "spawn_profile", "sandbox_profile", "process_template", "remote", "linear_team", "awb_workspace", "target_agent"} {
		t.Run(dimension, func(t *testing.T) {
			incomplete := action
			switch dimension {
			case "group":
				incomplete.Group = ""
			case "spawn_profile":
				incomplete.SpawnProfile = ""
			case "sandbox_profile":
				incomplete.SandboxProfile = ""
			case "process_template":
				incomplete.ProcessTemplate = ""
			case "remote":
				incomplete.Remote = ""
			case "linear_team":
				incomplete.LinearTeam = ""
			case "awb_workspace":
				incomplete.AWBWorkspace = ""
			case "target_agent":
				incomplete.TargetAgent = ""
			}
			require.False(t, scope.Matches(incomplete, nil), "a matching subset must not authorize the whole action")
		})
	}
	action.Group = "other"
	require.False(t, scope.Matches(action, nil))
	action.Group = "release"
	require.True(t, scope.Matches(action, nil))
	action.SpawnProfile = "renamed"
	require.False(t, scope.Matches(action, nil))
	normalized, err := scope.Normalize()
	require.NoError(t, err)
	normalized["group"][0] = "modified"
	require.Equal(t, []string{"release", "development"}, scope["group"])
}

func TestPermissionScopeRemoteUsesV1SegmentPrefixRules(t *testing.T) {
	for _, tt := range []struct {
		pattern, value string
		allowed        bool
	}{
		{"github.com/acme/*", "github.com/acme/repo", true},
		{"github.com/acme", "github.com/acme/repo", true},
		{"/GitHub.com/Acme/*/", "github.com/acme/repo", true},
		{"github.com/acme/*", "github.com/acme", false},
		{"github.com/acme/*", "github.com/acme-other/repo", false},
		{"github.com/ac*", "github.com/acme/repo", false},
		{"github.com/**", "github.com/acme/repo", false},
		{"*.com/acme/*", "github.com/acme/repo", false},
	} {
		t.Run(tt.pattern+tt.value, func(t *testing.T) {
			require.Equal(t, tt.allowed, model.PermissionScope{"remote": {tt.pattern}}.Matches(model.PermissionContext{Remote: tt.value}, nil))
		})
	}
	require.False(t, model.PermissionScope{"linear_team": {"TCL"}}.Matches(model.PermissionContext{LinearTeam: "TCLX"}, nil))
	require.False(t, model.PermissionScope{"group": {"Team"}}.Matches(model.PermissionContext{Group: "team"}, nil))
}

func TestPermissionScopeSelectorsNeedCurrentAncestryResolution(t *testing.T) {
	scope := model.PermissionScope{"target_agent": {"@descendants"}, "group": {"team"}}
	action := model.PermissionContext{TargetAgent: "agt_child", Group: "team"}
	require.False(t, scope.Matches(action, nil))
	calls := 0
	resolve := func(selector, target string) bool {
		calls++
		return selector == "@descendants" && target == "agt_child"
	}
	require.True(t, scope.Matches(action, resolve))
	require.Equal(t, 1, calls)
	action.TargetAgent = "agt_other"
	require.False(t, scope.Matches(action, resolve))
	for _, invalid := range []model.PermissionScope{{"future": {"value"}}, {"group": {}}, {"group": {"@descendants"}}, {"target_agent": {"@everyone"}}, {"group": {"bad\nname"}}, {"linear_team": {"TCL*"}}, {"awb_workspace": {"UPPER"}}} {
		_, err := invalid.Normalize()
		require.Error(t, err)
		require.False(t, invalid.Matches(action, func(string, string) bool { return true }))
	}
	require.True(t, model.PermissionScope(nil).Matches(model.PermissionContext{}, nil))
}
