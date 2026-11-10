package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A morph to Claude Code seeds the trust entry into the config file the
// morphed pane reads, <state root>/.claude.json (CLAUDE_CONFIG_DIR is pinned
// there for every tclaude Claude launch), and never into the top-level
// $HOME/.claude.json. CLAUDE_CONFIG_DIR is launch-owned, so neither the
// sandbox snapshot nor the daemon's environment supplies it.
func TestTrustAgentDirectory_ClaudeWritesStateRootConfig(t *testing.T) {
	resetTestDB(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	h, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	cwd := t.TempDir()

	require.NoError(t, trustAgentDirectory(h, "conv-without-sandbox", cwd))

	data, err := os.ReadFile(filepath.Join(home, ".claude", harness.ClaudeConfigJSONName))
	require.NoError(t, err, "the state-root config carries the trust entry")
	var root struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	resolved, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	assert.True(t, root.Projects[resolved].HasTrustDialogAccepted, "projects: %s", data)
	_, statErr := os.Stat(filepath.Join(home, harness.ClaudeConfigJSONName))
	assert.True(t, os.IsNotExist(statErr), "the top-level ~/.claude.json must not be created")
}
