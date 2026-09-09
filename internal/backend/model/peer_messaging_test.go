package model

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPeerMessagingPreservesExplicitOffAndHarnessSwitch(t *testing.T) {
	var options ConfigurationOptions
	require.NoError(t, json.Unmarshal([]byte(`{"PeerMessaging":false}`), &options))
	require.NotNil(t, options.PeerMessaging)
	require.False(t, options.Apply(DesiredConfiguration{Harness: "claude", PeerMessaging: true}).PeerMessaging)
	require.True(t, (ConfigurationOptions{}).Apply(DesiredConfiguration{Harness: "claude", PeerMessaging: true}).PeerMessaging)
	foreign := "codex"
	require.False(t, (&TeamProfileOverrides{Harness: &foreign}).Apply(DesiredConfiguration{Harness: "claude", PeerMessaging: true}).PeerMessaging)
	require.NoError(t, ValidatePeerMessaging(true, "claude"))
	require.Error(t, ValidatePeerMessaging(true, "codex"))
	require.NoError(t, ValidatePeerMessaging(false, "codex"))
	require.False(t, (DesiredConfiguration{PeerMessaging: true}).Equal(DesiredConfiguration{}))
}
