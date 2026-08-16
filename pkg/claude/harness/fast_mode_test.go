package harness

import (
	"os"
	"path/filepath"
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

func TestCodexMainConfigFastMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		fast    bool
	}{
		{name: "fast", content: "service_tier = \"fast\"\n", fast: true},
		{name: "priority alias", content: "service_tier = \"priority\"\n", fast: true},
		{name: "default", content: "service_tier = \"default\"\n"},
		{name: "unset", content: "model = \"gpt-5\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(tc.content), 0o600))
			got, err := CodexMainConfigFastMode(root)
			require.NoError(t, err)
			assert.Equal(t, tc.fast, got)
		})
	}

	got, err := CodexMainConfigFastMode(t.TempDir())
	require.NoError(t, err)
	assert.False(t, got, "a missing main config uses Codex's standard-tier default")
}
