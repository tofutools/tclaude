package agentd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type historyEnvironmentProbe struct {
	harness.HistoryTransfer
	environment []string
}

func (p historyEnvironmentProbe) WithEnvironment(env []string) harness.HistoryTransfer {
	p.environment = append([]string(nil), env...)
	return p
}

func TestOpenCodeHistoryUsesAllocatedPrivateStore(t *testing.T) {
	setupTestDB(t)
	const agent = "agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	allocation, err := allocatePrivateOpenCodeState(agent)
	require.NoError(t, err)
	original := harness.Harness{History: historyEnvironmentProbe{}}
	scoped, err := openCodeHistoryForAgent(&original, agent)
	require.NoError(t, err)
	env := scoped.History.(historyEnvironmentProbe).environment
	require.Contains(t, env, "XDG_DATA_HOME="+filepath.Join(allocation.StateRoot, "data"))
	require.Contains(t, env, "XDG_CONFIG_HOME="+filepath.Join(allocation.StateRoot, "config"))
	require.Empty(t, original.History.(historyEnvironmentProbe).environment)
	legacy, err := openCodeHistoryForAgent(&original, "agt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	require.Same(t, &original, legacy)
}
