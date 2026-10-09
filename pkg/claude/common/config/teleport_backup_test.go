package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTeleportBackupPolicyDefaultsAndValidation(t *testing.T) {
	defaults := (TeleportBackupConfig{}).Effective()
	require.Equal(t, 30, defaults.RenewSeconds)
	require.Equal(t, 300, defaults.LeaseSeconds)
	require.Equal(t, 120, defaults.GraceSeconds)
	require.Equal(t, "auto", defaults.Recovery)
	require.NoError(t, defaults.Validate())
	for _, p := range []TeleportBackupConfig{{Recovery: "force"}, {Superseded: "resume"}, {RenewSeconds: -1}, {LeaseSeconds: 1}, {GraceSeconds: -1}, {DormantMax: -1}, {RenewSeconds: 1000000}} {
		require.Error(t, p.Validate())
		require.NotEmpty(t, Validate(&Config{Federation: &FederationConfig{Teleport: &FederationTeleportConfig{Backup: p}}}))
	}
}
