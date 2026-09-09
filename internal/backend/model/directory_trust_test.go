package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryTrustRetainsExplicitOffAndCompatibleInheritance(t *testing.T) {
	var options ConfigurationOptions
	require.NoError(t, json.Unmarshal([]byte(`{"TrustDirectory":false}`), &options))
	require.NotNil(t, options.TrustDirectory)
	base := DesiredConfiguration{Harness: "claude", TrustDirectory: true}
	require.False(t, options.Apply(base).TrustDirectory)
	require.True(t, (ConfigurationOptions{}).Apply(base).TrustDirectory)
	for _, harness := range []string{"claude", "codex", "copilot"} {
		require.NoError(t, ValidateDirectoryTrust(true, harness))
		require.True(t, (&TeamProfileOverrides{Harness: &harness}).Apply(base).TrustDirectory)
	}
	foreign := "opencode"
	require.Error(t, ValidateDirectoryTrust(true, foreign))
	require.NoError(t, ValidateDirectoryTrust(false, foreign))
	require.False(t, (&TeamProfileOverrides{Harness: &foreign}).Apply(base).TrustDirectory)
	on := true
	explicit := (&TeamProfileOverrides{Harness: &foreign, TrustDirectory: &on}).Apply(base)
	require.True(t, explicit.TrustDirectory, "explicit unsupported intent must survive until validation refuses it")
	require.Error(t, ValidateDirectoryTrust(explicit.TrustDirectory, explicit.Harness))
	require.False(t, (DesiredConfiguration{TrustDirectory: true}).Equal(DesiredConfiguration{}))
}
