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
