package agentd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func processAuthoringFlow(t *testing.T) (*testharness.Flow, string) {
	t.Helper()
	f := newFlow(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	return f, root
}
