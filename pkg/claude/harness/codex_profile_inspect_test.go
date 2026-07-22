package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func TestCodexManagedBaselineFilesystemRulesDoNotDependOnInstalledProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-not-installed"))

	rules, err := CodexManagedBaselineFilesystemRules()
	require.NoError(t, err)
	byPath := map[string]string{}
	for _, rule := range rules {
		byPath[rule.Path] = rule.Access
	}
	assert.Equal(t, "none", byPath[filepath.Join(home, ".tclaude", "data")])
	assert.Equal(t, "read", byPath[agentipc.CanonicalSocketPath()])
	assert.Equal(t, "none", byPath[filepath.Join(home, ".claude", "sessions")])

	installed, err := CodexAgentProfilePath()
	require.NoError(t, err)
	_, err = os.Stat(installed)
	assert.ErrorIs(t, err, os.ErrNotExist, "inspection must not create the installed convenience profile")
}
