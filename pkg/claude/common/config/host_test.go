package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHostWarningConfig(t *testing.T) {
	cfg := &Config{}
	load, ram, disk := cfg.HostWarningThresholds()
	require.Equal(t, 1.5, load)
	require.Equal(t, 10.0, ram)
	require.Equal(t, 10.0, disk)
	zero, high, negative := 0.0, 101.0, -1.0
	cfg.Host = &HostConfig{WarnLoadPerCore: &zero, WarnRAMAvailablePercent: &zero, WarnDiskAvailablePercent: &zero}
	load, ram, disk = cfg.HostWarningThresholds()
	require.Zero(t, load)
	require.Zero(t, ram)
	require.Zero(t, disk)
	require.Empty(t, Validate(cfg))
	cfg.Host = &HostConfig{WarnLoadPerCore: &negative, WarnRAMAvailablePercent: &high, WorkDirs: []string{"relative"}}
	require.Len(t, Validate(cfg), 3)
	load, ram, _ = cfg.HostWarningThresholds()
	require.Equal(t, 1.5, load)
	require.Equal(t, 10.0, ram)
}
