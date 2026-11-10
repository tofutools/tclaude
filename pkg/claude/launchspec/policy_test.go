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
		want := launchspec.ModeReResolve
		if r.Field == "harness_config" {
			want = launchspec.ModeExplicitWins
		}
		assert.Equal(t, want, r.Mode, r.Field)
		keys = append(keys, r.Field)
	}
	assert.ElementsMatch(t, []string{"group_env", "group_remote_control",
		"sandbox_profile_contents", "harness_config"}, keys)
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
		{reuseEnv, "group_remote_control", true},
		{empty, "group_sandbox_assignment", true},
		{empty, "usage_profile_contents", true},
		{empty, "group_access", true},
		{empty, "harness", false},
		{empty, "model", false},
		{empty, "sandbox_mode", false},
		{empty, "initial_brief", false},
		{empty, "no_such_field", false},
		{empty, "harness_config", true},
	} {
		assert.Equalf(t, tc.want, tc.policy.ReResolves(tc.field), "%s with %d rows", tc.field, len(tc.policy.Rows))
	}
}

func TestRelaunchPolicyValidateRejects(t *testing.T) {
	row := func(scope, field string, mode launchspec.RelaunchMode) launchspec.RelaunchPolicy {
		return launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{{Scope: scope, Field: field, Mode: mode}}}
	}
	for name, p := range map[string]launchspec.RelaunchPolicy{
		"unknown scope":                    row("group:7", "group_env", launchspec.ModeReuse),
		"unknown field":                    row(launchspec.ScopeGlobal, "nope", launchspec.ModeReuse),
		"not configurable":                 row(launchspec.ScopeGlobal, "model", launchspec.ModeReResolve),
		"unknown mode":                     row(launchspec.ScopeGlobal, "group_env", "sometimes"),
		"mode of another row":              row(launchspec.ScopeGlobal, "group_env", launchspec.ModeTightenOnly),
		"two-mode value on harness_config": row(launchspec.ScopeGlobal, "harness_config", launchspec.ModeReuse),
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

func TestRelaunchPolicyHarnessConfigModes(t *testing.T) {
	assert.Equal(t, launchspec.ModeExplicitWins, launchspec.RelaunchPolicy{}.Mode("harness_config"),
		"least surprise by default")
	tighten := launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{
		{Scope: launchspec.ScopeGlobal, Field: "harness_config", Mode: launchspec.ModeTightenOnly}}}
	require.NoError(t, tighten.Validate())
	assert.Equal(t, launchspec.ModeTightenOnly, tighten.Mode("harness_config"))
	assert.Equal(t, launchspec.RelaunchMode(""), tighten.Mode("model"), "not configurable")
}

func TestValidateOverrides(t *testing.T) {
	assert.NoError(t, launchspec.ValidateOverrides(map[string]string{"group_env": "reuse", "harness_config": "tighten-only"}, false))
	assert.NoError(t, launchspec.ValidateOverrides(map[string]string{"group_env": "inherit"}, true), "a spawn may follow the global table")
	assert.Error(t, launchspec.ValidateOverrides(map[string]string{"group_env": "inherit"}, false), "a profile leaves the row out instead")
	assert.Error(t, launchspec.ValidateOverrides(map[string]string{"group_env": "chain-wins"}, true))
	assert.Error(t, launchspec.ValidateOverrides(map[string]string{"harness": "reuse"}, true), "not configurable")
	assert.Error(t, launchspec.ValidateOverrides(map[string]string{"nope": "reuse"}, true))
}

func TestKeepsOldSettings(t *testing.T) {
	for mode, keeps := range map[launchspec.RelaunchMode]bool{
		launchspec.ModeReuse: true, launchspec.ModeExplicitWins: true,
		launchspec.ModeReResolve: false, launchspec.ModeTightenOnly: false,
		launchspec.ModeChainWins: false, launchspec.ModeInherit: false,
	} {
		assert.Equalf(t, keeps, launchspec.KeepsOldSettings(mode), "%s", mode)
	}
}

func TestWithOverrides(t *testing.T) {
	global := launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{
		{Scope: launchspec.ScopeGlobal, Field: "group_env", Mode: launchspec.ModeReuse},
		{Scope: launchspec.ScopeGlobal, Field: "harness_config", Mode: launchspec.ModeChainWins},
	}}
	got := global.WithOverrides(map[string]launchspec.RelaunchMode{
		"group_env":                launchspec.ModeReResolve,
		"sandbox_profile_contents": launchspec.ModeReuse,
		"harness_config":           launchspec.ModeReuse, // not a mode the row offers: skipped
	})
	require.NoError(t, got.Validate())
	assert.Equal(t, launchspec.ModeReResolve, got.Mode("group_env"), "the override replaces the global row")
	assert.Equal(t, launchspec.ModeReuse, got.Mode("sandbox_profile_contents"), "an override where the table has no row")
	assert.Equal(t, launchspec.ModeChainWins, got.Mode("harness_config"), "an invalid override leaves the global row")
	assert.Equal(t, launchspec.ModeReResolve, got.Mode("group_remote_control"), "untouched rows keep their default")
}
