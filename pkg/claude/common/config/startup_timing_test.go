package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartupTimingReloadsConfigBetweenTraces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TCLAUDE_STARTUP_TIMING", "")
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	StartupTiming("disabled")("return")
	require.Empty(t, output.String())
	enabled := true
	require.NoError(t, Save(&Config{StartupTiming: &enabled}))
	active := StartupTiming("active")
	require.Contains(t, output.String(), `"component":"active"`)

	enabled = false
	require.NoError(t, Save(&Config{StartupTiming: &enabled}))
	output.Reset()
	StartupTiming("disabled_again")("return")
	require.Empty(t, output.String())
	active("return") // Keep an in-flight trace complete after the runtime toggle.
	require.Contains(t, output.String(), `"stage":"return"`)
}

func TestStartupTimingConfigOverridesEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TCLAUDE_STARTUP_TIMING", "1")
	require.True(t, StartupTimingEnabled())
	enabled := false
	require.NoError(t, Save(&Config{StartupTiming: &enabled}))
	require.False(t, StartupTimingEnabled())
	enabled = true
	require.NoError(t, Save(&Config{StartupTiming: &enabled}))
	t.Setenv("TCLAUDE_STARTUP_TIMING", "0")
	require.True(t, StartupTimingEnabled())
}
