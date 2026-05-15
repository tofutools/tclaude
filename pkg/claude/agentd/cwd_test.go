package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in, want string
	}{
		{"~", home},
		{"~/git/foo", filepath.Join(home, "git", "foo")},
		{"/abs/path", "/abs/path"},
		{"relative/dir", "relative/dir"},
		{"~user/x", "~user/x"}, // unsupported ~user form — left untouched
		{"", ""},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, expandTilde(c.in), "expandTilde(%q)", c.in)
	}
}

func TestResolveSpawnCwd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	t.Run("empty stays empty (daemon default)", func(t *testing.T) {
		got, err := resolveSpawnCwd("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("existing directory resolves absolute", func(t *testing.T) {
		got, err := resolveSpawnCwd(dir)
		require.NoError(t, err)
		assert.Equal(t, dir, got)
	})

	t.Run("tilde expands to home", func(t *testing.T) {
		got, err := resolveSpawnCwd("~")
		require.NoError(t, err)
		assert.Equal(t, dir, got)
	})

	t.Run("nonexistent directory errors", func(t *testing.T) {
		_, err := resolveSpawnCwd(filepath.Join(dir, "no-such-subdir"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("a file is not a valid cwd", func(t *testing.T) {
		f := filepath.Join(dir, "afile")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
		_, err := resolveSpawnCwd(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})
}
