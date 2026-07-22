package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestRemoveLegacyRuntimeDataPreservesAuthoringAndUnrelatedRootData(t *testing.T) {
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(t.Context(), storetest.Template())
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(root, "runs", "old-run", "artifacts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "old-run", "state.json"), []byte(`{"stateSchemaVersion":8}`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".locks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".locks", "run-old-run.lock"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".locks", "template-demo.lock"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep-me"), []byte("unrelated"), 0o644))

	require.NoError(t, fs.RemoveLegacyRuntimeData())

	_, err = os.Stat(filepath.Join(root, "runs"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(root, ".locks", "run-old-run.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(root, ".locks", "template-demo.lock"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "keep-me"))
	require.NoError(t, err)
	_, err = fs.GetTemplate(t.Context(), record.Ref)
	require.NoError(t, err)
}

func TestRemoveLegacyRuntimeDataIsIdempotent(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.RemoveLegacyRuntimeData())
	require.NoError(t, fs.RemoveLegacyRuntimeData())
}

func TestRemoveLegacyRuntimeDataRejectsSymlinkedLockDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	outsideLock := filepath.Join(external, "run-outside.lock")
	require.NoError(t, os.WriteFile(outsideLock, []byte("keep"), 0o644))
	require.NoError(t, os.Symlink(external, filepath.Join(root, ".locks")))
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	err = fs.RemoveLegacyRuntimeData()
	require.ErrorContains(t, err, "without following symlinks")
	data, readErr := os.ReadFile(outsideLock)
	require.NoError(t, readErr)
	require.Equal(t, "keep", string(data))
}

func TestRemoveLegacyRuntimeDataRemovesOnlyExactRegularRunLocks(t *testing.T) {
	root := t.TempDir()
	lockDir := filepath.Join(root, ".locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(lockDir, "run-valid.lock"), nil, 0o644))
	for _, name := range []string{"run-.lock", "run-UPPER.lock", "run-valid.lock.extra", ".run-valid.lock"} {
		require.NoError(t, os.WriteFile(filepath.Join(lockDir, name), []byte("keep"), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(lockDir, "run-directory.lock"), 0o755))
	external := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(external, []byte("keep"), 0o644))
	require.NoError(t, os.Symlink(external, filepath.Join(lockDir, "run-symlink.lock")))
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	require.NoError(t, fs.RemoveLegacyRuntimeData())
	_, err = os.Stat(filepath.Join(lockDir, "run-valid.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{"run-.lock", "run-UPPER.lock", "run-valid.lock.extra", ".run-valid.lock", "run-directory.lock"} {
		_, err = os.Lstat(filepath.Join(lockDir, name))
		require.NoError(t, err, name)
	}
	info, err := os.Lstat(filepath.Join(lockDir, "run-symlink.lock"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	data, err := os.ReadFile(external)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
}
