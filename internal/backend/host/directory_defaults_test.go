package host

import (
	"context"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestGroupDefaultDirectoryAllowsFuturePathAndHomeShorthand(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	host := DirectoryBrowser{}
	for _, input := range []string{"~/future/../work", filepath.Join(home, "work"), "  " + filepath.Join(home, "work") + "  "} {
		got, err := host.NormalizeDefaultDirectory(ctx, input)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "work"), got)
	}
	got, err := host.NormalizeDefaultDirectory(ctx, "~")
	require.NoError(t, err)
	require.Equal(t, home, got)
	got, err = host.NormalizeDefaultDirectory(ctx, "  ")
	require.NoError(t, err)
	require.Empty(t, got)
	for _, input := range []string{"relative", "~somebody/work", "/tmp/a\x00b"} {
		_, err = host.NormalizeDefaultDirectory(ctx, input)
		require.Error(t, err)
	}
}
