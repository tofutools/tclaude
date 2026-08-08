package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveFastModePreservesThreeStates(t *testing.T) {
	codex := MustGet(CodexName)
	on, off := true, false

	mode, err := ResolveFastMode(codex, nil)
	require.NoError(t, err)
	assert.Equal(t, FastModeInherit, mode)
	mode, err = ResolveFastMode(codex, &on)
	require.NoError(t, err)
	assert.Equal(t, FastModeOn, mode)
	mode, err = ResolveFastMode(codex, &off)
	require.NoError(t, err)
	assert.Equal(t, FastModeOff, mode)

	_, err = ResolveFastMode(MustGet(DefaultName), &on)
	assert.ErrorContains(t, err, "does not support fast mode")
	_, err = ResolveFastMode(MustGet(DefaultName), &off)
	assert.ErrorContains(t, err, "does not support fast mode")
}

func TestResolveFastModeFlag(t *testing.T) {
	codex := MustGet(CodexName)
	for raw, want := range map[string]string{
		"": FastModeInherit, "inherit": FastModeInherit, "on": FastModeOn, "off": FastModeOff,
	} {
		got, err := ResolveFastModeFlag(codex, raw)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := ResolveFastModeFlag(codex, "turbo")
	assert.Error(t, err)
}
