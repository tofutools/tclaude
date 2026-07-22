package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxGlobalFilesystemRulesMergeHarnessProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(codexHome, 0o755))

	sharedDeny := filepath.Join(home, "private")
	sharedRead := filepath.Join(home, "control.sock")
	claudeReadDeny := filepath.Join(home, "read-hidden")
	codexWrite := filepath.Join(home, "codex-cache")
	claudeSettings := fmt.Sprintf(`{
  "sandbox": {
    "enabled": false,
    "filesystem": {
      "allowRead": ["%s"],
      "denyRead": ["%s", "%s"],
      "denyWrite": ["%s"]
    }
  }
}`, sharedRead, sharedDeny, claudeReadDeny, sharedDeny)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(claudeSettings), 0o600))
	codexProfile := fmt.Sprintf(`default_permissions = "tclaude-agent"

[permissions.tclaude-agent]
extends = ":workspace"

[permissions.tclaude-agent.filesystem]
%q = "none"
%q = "read"
%q = "write"
`, sharedDeny, sharedRead, codexWrite)
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "tclaude-agent.config.toml"), []byte(codexProfile), 0o600))

	got := sandboxGlobalFilesystemRules()
	require.Empty(t, got.Warnings)
	byKey := map[string]sandboxGlobalFilesystemRuleJSON{}
	for _, rule := range got.Filesystem {
		byKey[rule.Path+"|"+rule.Access] = rule
	}

	deny := byKey["~/private|deny"]
	assert.Equal(t, []string{"claude", "codex"}, deny.Harnesses)
	require.Len(t, deny.Origins, 2)
	assert.Contains(t, deny.Origins[0].Note+deny.Origins[1].Note, "inactive unless the launch forces")
	read := byKey["~/control.sock|read"]
	assert.Equal(t, []string{"claude", "codex"}, read.Harnesses)
	assert.Equal(t, []string{"claude"}, byKey["~/read-hidden|deny-read"].Harnesses)
	assert.Equal(t, []string{"codex"}, byKey["~/codex-cache|write"].Harnesses)
	assert.Equal(t, "~/.claude/settings.json", read.Origins[0].Source)
	assert.Equal(t, "~/.codex/tclaude-agent.config.toml", read.Origins[1].Source)
}

func TestSandboxGlobalFilesystemRulesReportMalformedLayersIndependently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(codexHome, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"sandbox":`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "tclaude-agent.config.toml"), []byte(`[permissions.`), 0o600))

	got := sandboxGlobalFilesystemRules()
	assert.Empty(t, got.Filesystem)
	require.Len(t, got.Warnings, 2)
	assert.Contains(t, got.Warnings[0], "Claude Code")
	assert.Contains(t, got.Warnings[1], "Codex")
}
