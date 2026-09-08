//go:build linux || darwin

package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestOpenCodeConfigProjectionRequiresOwnedTargetAndRetainsBothIdentities(t *testing.T) {
	private, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	source := t.TempDir()
	state := filepath.Join(private, "execution")
	target := filepath.Join(state, "config", "opencode")
	require.NoError(t, os.MkdirAll(target, 0700))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	projection := SandboxOpenCodeConfiguration(source, state, model.SandboxFilesystemRead)
	_, err = inspector.BindSandboxProviderResources(context.Background(), []SandboxProviderResource{projection})
	require.ErrorContains(t, err, "declared private writable state")
	resources := []SandboxProviderResource{{Path: state, Access: model.SandboxFilesystemWrite}, {Path: source, Access: model.SandboxFilesystemRead}, projection}
	bound, err := inspector.BindSandboxProviderResources(context.Background(), resources)
	require.NoError(t, err)
	defer func() { _ = bound.Close() }()
	pins := bound.Pins()
	require.Equal(t, target, pins[2].Guest)
	reopened, err := inspector.reopenSandboxChildBindings(context.Background(), nil, pins)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
	require.NoError(t, os.Rename(target, target+".old"))
	require.NoError(t, os.Mkdir(target, 0700))
	_, err = inspector.reopenSandboxChildBindings(context.Background(), nil, pins)
	require.ErrorContains(t, err, "identity changed")
	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Symlink(source, target))
	_, err = inspector.BindSandboxProviderResources(context.Background(), resources)
	require.ErrorContains(t, err, "target changed")
}
