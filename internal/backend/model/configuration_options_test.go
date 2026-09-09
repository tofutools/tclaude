package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigurationOptionsRoundTripPreservesOmissionAndExplicitFalse(t *testing.T) {
	var options ConfigurationOptions
	require.NoError(t, json.Unmarshal([]byte(`{"AutoReview":false,"FastMode":"off","Environment":{"KEEP":"","NEW":"literal"}}`), &options))
	encoded, err := json.Marshal(options)
	require.NoError(t, err)
	var reopened ConfigurationOptions
	require.NoError(t, json.Unmarshal(encoded, &reopened))
	require.Nil(t, reopened.Harness)
	require.Nil(t, reopened.Model)
	require.NotNil(t, reopened.AutoReview)
	require.False(t, *reopened.AutoReview)
	base := DesiredConfiguration{Harness: "codex", Model: "current", AutoReview: true, FastMode: FastModeOn, Environment: Environment{"KEEP": "before", "OTHER": "retained"}}
	resolved := reopened.Apply(base)
	require.Equal(t, "current", resolved.Model)
	require.False(t, resolved.AutoReview)
	require.Equal(t, FastModeOff, resolved.FastMode)
	require.Equal(t, Environment{"KEEP": "", "OTHER": "retained", "NEW": "literal"}, resolved.Environment)
	resolved.Environment["OTHER"] = "changed"
	require.Equal(t, "retained", base.Environment["OTHER"])
	require.NotContains(t, reopened.Environment, "OTHER")
	inherited := (ConfigurationOptions{}).Apply(base)
	require.True(t, inherited.AutoReview)
	require.Equal(t, FastModeOn, inherited.FastMode)
}

func TestConfigurationOptionsHarnessSwitchDropsForeignNativeChoices(t *testing.T) {
	harness := "opencode"
	options := ConfigurationOptions{Harness: &harness}
	resolved := options.Apply(DesiredConfiguration{Harness: "codex", Model: "codex-model", Effort: "high", FastMode: FastModeOn, AutoReview: true})
	require.Equal(t, harness, resolved.Harness)
	require.Empty(t, resolved.Model)
	require.Empty(t, resolved.Effort)
	require.Empty(t, resolved.FastMode)
	require.False(t, resolved.AutoReview)
}
