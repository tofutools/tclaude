package statusbar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common"
)

func TestStatusLineCommandRemainsPortableWithAbsoluteHostIntegrations(t *testing.T) {
	common.SetAbsolutePaths(true)
	t.Cleanup(func() { common.SetAbsolutePaths(false) })

	assert.Equal(t, "tclaude status-bar", StatusLineCommand)
}

func TestInstallRepairsAbsoluteStatusLineToPortableCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{
  "statusLine": {
    "type": "command",
    "command": "/host-only/bin/tclaude status-bar"
  }
}`), 0o644))

	assert.False(t, CheckInstalled())
	require.NoError(t, Install())

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings struct {
		StatusLine StatusLineConfig `json:"statusLine"`
	}
	require.NoError(t, json.Unmarshal(data, &settings))
	assert.Equal(t, StatusLineConfig{Type: "command", Command: "tclaude status-bar"}, settings.StatusLine)
}
