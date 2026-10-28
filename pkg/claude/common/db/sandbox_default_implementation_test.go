package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedFreshInstallSandboxDefaultOnlyOnAFreshInstall(t *testing.T) {
	setupTestDB(t)
	_, decided, err := GetSandboxDefaultImplementation()
	require.NoError(t, err)
	require.False(t, decided, "a new database starts undecided")

	seeded, err := SeedFreshInstallSandboxDefault()
	require.NoError(t, err)
	assert.True(t, seeded)
	value, decided, err := GetSandboxDefaultImplementation()
	require.NoError(t, err)
	assert.True(t, decided)
	assert.Equal(t, SandboxDefaultTclaudeLayer, value)

	// A recorded decision is never overwritten by a later setup run.
	_, err = SetSandboxDefaultImplementation(SandboxDefaultHarness)
	require.NoError(t, err)
	seeded, err = SeedFreshInstallSandboxDefault()
	require.NoError(t, err)
	assert.False(t, seeded)
	value, _, _ = GetSandboxDefaultImplementation()
	assert.Equal(t, SandboxDefaultHarness, value)
}

func TestSeedFreshInstallSandboxDefaultLeavesAnExistingInstallUndecided(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "spwn-old", TmuxSession: "tmux-spwn-old", ConvID: "conv-old",
		Status: "idle", CreatedAt: time.Now().UTC(),
	}))

	seeded, err := SeedFreshInstallSandboxDefault()
	require.NoError(t, err)
	assert.False(t, seeded, "an install with history is asked, not switched")
	_, decided, err := GetSandboxDefaultImplementation()
	require.NoError(t, err)
	assert.False(t, decided)
}

func TestNormalizeSandboxDefaultImplementation(t *testing.T) {
	for raw, want := range map[string]string{
		"tclaude-layer": SandboxDefaultTclaudeLayer, " harness ": SandboxDefaultHarness, "harness-builtin": SandboxDefaultHarness,
	} {
		got, err := NormalizeSandboxDefaultImplementation(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"", "off", "resource-only", "stacked"} {
		_, err := NormalizeSandboxDefaultImplementation(raw)
		assert.Error(t, err, raw)
	}
}
