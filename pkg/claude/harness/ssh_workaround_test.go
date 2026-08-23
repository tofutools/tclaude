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
	assert.Equal(t, runtime.GOOS == "linux", claude.CanSSHWorkaround())
	assert.False(t, (*Harness)(nil).CanSSHWorkaround())

	got, err := ResolveSSHWorkaround(codex, nil)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got, "Linux harnesses default the workaround on where it applies")

	got, err = ResolveSSHWorkaround(claude, nil)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got)

	on, off := true, false
	got, err = ResolveSSHWorkaround(codex, &off)
	require.NoError(t, err)
	assert.False(t, got, "Codex supports an explicit opt-out")
	got, err = ResolveSSHWorkaround(codex, &on)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got)

	got, err = ResolveSSHWorkaround(claude, &on)
	require.NoError(t, err)
	assert.Equal(t, runtime.GOOS == "linux", got)
	got, err = ResolveSSHWorkaround(claude, &off)
	require.NoError(t, err)
	assert.False(t, got)
	_, err = ResolveSSHWorkaround(nil, &on)
	require.ErrorContains(t, err, "requires a harness")
}
