package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestFederationNodeConfigValidation(t *testing.T) {
	require.Empty(t, Validate(&Config{Federation: &FederationConfig{NodeLabels: []string{"gpu", "test-rig"}, MaxLiveAgents: 8}}))
	for _, f := range []*FederationConfig{{MaxLiveAgents: -1}, {NodeLabels: []string{"bad label"}}, {NodeLabels: []string{""}}} {
		require.NotEmpty(t, Validate(&Config{Federation: f}))
	}
	require.Empty(t, Validate(&Config{Federation: &FederationConfig{MaxLiveAgents: 0}}))
}
