package model

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAutoMemoryPreservesExplicitOffAndHarnessSwitch(t *testing.T) {
	var options ConfigurationOptions
	require.NoError(t, json.Unmarshal([]byte(`{"AutoMemory":false}`), &options))
	require.NotNil(t, options.AutoMemory)
	require.False(t, options.Apply(DesiredConfiguration{Harness: "claude", AutoMemory: true}).AutoMemory)
	require.True(t, (ConfigurationOptions{}).Apply(DesiredConfiguration{Harness: "claude", AutoMemory: true}).AutoMemory)
	foreign := "codex"
	require.False(t, (&TeamProfileOverrides{Harness: &foreign}).Apply(DesiredConfiguration{Harness: "claude", AutoMemory: true}).AutoMemory)
	require.NoError(t, ValidateAutoMemory(true, "claude"))
	require.Error(t, ValidateAutoMemory(true, "codex"))
	require.NoError(t, ValidateAutoMemory(false, "codex"))
	require.False(t, (DesiredConfiguration{AutoMemory: true}).Equal(DesiredConfiguration{}))
}
