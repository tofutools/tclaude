package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoMemoryEnvValueInverts(t *testing.T) {
	// The variable DISABLES, so the mapping is inverted. This is the whole
	// reason callers must not open-code the literals.
	assert.Equal(t, "0", AutoMemoryEnvValue(true), "auto memory ON disables the disable")
	assert.Equal(t, "1", AutoMemoryEnvValue(false), "auto memory OFF sets the disable")
}

func TestSupportsAutoMemory(t *testing.T) {
	claude, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)
	assert.True(t, claude.SupportsAutoMemory(), "Claude Code has an auto-memory system")
	assert.True(t, claude.CanAutoMemory())

	codex, err := ResolveSpawnable(CodexName)
	require.NoError(t, err)
	assert.False(t, codex.SupportsAutoMemory(), "Codex has no auto-memory system")
	assert.False(t, codex.CanAutoMemory())

	var nilHarness *Harness
	assert.False(t, nilHarness.SupportsAutoMemory(), "nil-safe")
}

func TestResolveAutoMemory(t *testing.T) {
	claude, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)
	codex, err := ResolveSpawnable(CodexName)
	require.NoError(t, err)

	on, off := true, false

	// Unset resolves to OFF everywhere — tclaude's recommended posture, not a
	// pass-through of the harness default.
	got, err := ResolveAutoMemory(claude, nil)
	require.NoError(t, err)
	assert.False(t, got, "unset resolves to auto memory off")

	got, err = ResolveAutoMemory(codex, nil)
	require.NoError(t, err)
	assert.False(t, got)

	got, err = ResolveAutoMemory(claude, &on)
	require.NoError(t, err)
	assert.True(t, got, "an explicit opt-in survives for Claude Code")

	got, err = ResolveAutoMemory(claude, &off)
	require.NoError(t, err)
	assert.False(t, got)

	// An explicit opt-in for a harness with no auto-memory system is an error,
	// not a silent drop, so a mistake surfaces at the spawn boundary.
	_, err = ResolveAutoMemory(codex, &on)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no auto-memory system")

	// false is fine for every harness: it is simply never injected for one
	// that has no such switch.
	got, err = ResolveAutoMemory(codex, &off)
	require.NoError(t, err)
	assert.False(t, got)
}
