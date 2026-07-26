package harness

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSSHWorkaround(t *testing.T) {
	codex, err := Resolve(CodexName)
	require.NoError(t, err)
	claude, err := Resolve(DefaultName)
	require.NoError(t, err)

	assert.Equal(t, runtime.GOOS == "linux", codex.CanSSHWorkaround())
	assert.False(t, claude.CanSSHWorkaround())
	assert.False(t, (*Harness)(nil).CanSSHWorkaround())

	got, err := ResolveSSHWorkaround(codex, nil)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got, "Codex defaults the workaround on where it applies")

	got, err = ResolveSSHWorkaround(claude, nil)
	require.NoError(t, err)
	assert.False(t, got)

	on, off := true, false
	got, err = ResolveSSHWorkaround(codex, &off)
	require.NoError(t, err)
	assert.False(t, got, "Codex supports an explicit opt-out")
	got, err = ResolveSSHWorkaround(codex, &on)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got)

	_, err = ResolveSSHWorkaround(claude, &on)
	require.ErrorContains(t, err, "not supported")
	got, err = ResolveSSHWorkaround(claude, &off)
	require.NoError(t, err)
	assert.False(t, got)
}
