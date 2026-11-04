package launchspec_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

func TestDefaultRelaunchPolicyReResolvesEveryConfigurableRow(t *testing.T) {
	p := launchspec.DefaultRelaunchPolicy()
	require.NoError(t, p.Validate())
	var keys []string
	for _, r := range p.Rows {
		assert.Equal(t, launchspec.ScopeGlobal, r.Scope)
		assert.Equal(t, launchspec.ModeReResolve, r.Mode)
		keys = append(keys, r.Field)
	}
	assert.ElementsMatch(t, []string{"group_env", "group_remote_control", "group_startup_context",
		"sandbox_profile_contents", "role_contents"}, keys)
}

func TestRelaunchPolicyReResolves(t *testing.T) {
	empty := launchspec.RelaunchPolicy{}
	reuseEnv := launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{
		{Scope: launchspec.ScopeGlobal, Field: "group_env", Mode: launchspec.ModeReuse}}}
	require.NoError(t, reuseEnv.Validate())

	for _, tc := range []struct {
		policy launchspec.RelaunchPolicy
		field  string
		want   bool
	}{
		{empty, "group_env", true},
		{reuseEnv, "group_env", false},
		{reuseEnv, "group_startup_context", true},
		{empty, "group_sandbox_assignment", true},
		{empty, "usage_profile_contents", true},
		{empty, "group_access", true},
		{empty, "harness", false},
		{empty, "model", false},
		{empty, "sandbox_mode", false},
		{empty, "initial_brief", false},
		{empty, "no_such_field", false},
	} {
		assert.Equalf(t, tc.want, tc.policy.ReResolves(tc.field), "%s with %d rows", tc.field, len(tc.policy.Rows))
	}
}

func TestRelaunchPolicyValidateRejects(t *testing.T) {
	row := func(scope, field string, mode launchspec.RelaunchMode) launchspec.RelaunchPolicy {
		return launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{{Scope: scope, Field: field, Mode: mode}}}
	}
	for name, p := range map[string]launchspec.RelaunchPolicy{
		"unknown scope":    row("group:7", "group_env", launchspec.ModeReuse),
		"unknown field":    row(launchspec.ScopeGlobal, "nope", launchspec.ModeReuse),
		"not configurable": row(launchspec.ScopeGlobal, "model", launchspec.ModeReResolve),
		"unknown mode":     row(launchspec.ScopeGlobal, "group_env", "sometimes"),
		"duplicate": {Rows: []launchspec.PolicyRow{
			{Scope: launchspec.ScopeGlobal, Field: "group_env", Mode: launchspec.ModeReuse},
			{Scope: launchspec.ScopeGlobal, Field: "group_env", Mode: launchspec.ModeReResolve}}},
	} {
		assert.Errorf(t, p.Validate(), name)
	}
}

func TestRelaunchPolicyJSONShape(t *testing.T) {
	raw, err := json.Marshal(launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{
		{Scope: launchspec.ScopeGlobal, Field: "group_env", Mode: launchspec.ModeReuse}}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"rows":[{"scope":"global","field":"group_env","mode":"reuse"}]}`, string(raw))
}
