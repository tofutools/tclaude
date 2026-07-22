package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestLegacyRuntimeCleanupPreservesTemplateStore(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(SetProcessStoreRootForTest(root))
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), storetest.Template())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "runs", "legacy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "legacy", "state.json"), []byte("legacy"), 0o644))

	require.NoError(t, removeLegacyProcessRuntimeData())
	_, err = os.Stat(filepath.Join(root, "runs"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = fs.GetTemplate(t.Context(), record.Ref)
	require.NoError(t, err)
}
