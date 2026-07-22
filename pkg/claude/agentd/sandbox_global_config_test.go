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

	sharedDeny := filepath.Join(home, ".tclaude", "data")
	sharedRead := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	claudeReadDeny := filepath.Join(home, "read-hidden")
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
	// A hand-edited installed convenience profile is deliberately ignored: the
	// launch-specific managed profile is regenerated from code every time.
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "tclaude-agent.config.toml"), []byte(`[permissions.tclaude-agent.filesystem]
"/not/the/launch/baseline" = "write"
`), 0o600))

	got := sandboxGlobalFilesystemRules(home)
	require.Empty(t, got.Warnings)
	byKey := map[string]sandboxGlobalFilesystemRuleJSON{}
	for _, rule := range got.Filesystem {
		byKey[rule.Path+"|"+rule.Access] = rule
	}

	deny := byKey["~/.tclaude/data|deny"]
	assert.Equal(t, []string{"claude", "codex"}, deny.Harnesses)
	require.Len(t, deny.Origins, 2)
	assert.Contains(t, deny.Origins[0].Note+deny.Origins[1].Note, "inactive unless the launch forces")
	read := byKey["~/.tclaude/api/agentd.sock|read"]
	assert.Equal(t, []string{"claude", "codex"}, read.Harnesses)
	assert.Equal(t, []string{"claude"}, byKey["~/read-hidden|deny-read"].Harnesses)
	_, staleInstalledRowShown := byKey["/not/the/launch/baseline|write"]
	assert.False(t, staleInstalledRowShown)
	assert.Equal(t, "~/.claude/settings.json", read.Origins[0].Source)
	assert.Equal(t, "generated tclaude-agent-<launch-id>.config.toml", read.Origins[1].Source)
}

func TestSandboxGlobalFilesystemRulesKeepCanonicalCodexBaselineWhenClaudeConfigIsMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"sandbox":`), 0o600))

	got := sandboxGlobalFilesystemRules(home)
	assert.NotEmpty(t, got.Filesystem)
	require.Len(t, got.Warnings, 1)
	assert.Contains(t, got.Warnings[0], "Claude Code")
	assert.NotContains(t, got.Warnings[0], `{"sandbox":`, "parser source excerpts must not reach the endpoint")
}

func TestSandboxGlobalFilesystemRulesMergeAcrossSymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	linkedHome := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.Symlink(realHome, linkedHome))
	t.Setenv("HOME", linkedHome)
	require.NoError(t, os.MkdirAll(filepath.Join(linkedHome, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(linkedHome, ".tclaude", "data"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(linkedHome, ".claude", "settings.json"), []byte(`{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/.tclaude/data"],"denyWrite":["~/.tclaude/data"]}}}`), 0o600))

	got := sandboxGlobalFilesystemRules(linkedHome)
	byKey := map[string]sandboxGlobalFilesystemRuleJSON{}
	for _, rule := range got.Filesystem {
		byKey[rule.Path+"|"+rule.Access] = rule
	}
	merged := byKey["~/.tclaude/data|deny"]
	assert.Equal(t, []string{"claude", "codex"}, merged.Harnesses)
	require.Len(t, merged.Origins, 2)
	assert.Equal(t, "~/.claude/settings.json", merged.Origins[0].Source)
}

func TestMergeSandboxGlobalFilesystemRulesWriteIncludesRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "shared")
	candidates := []sandboxGlobalFilesystemRuleCandidate{
		{path: path, access: "read", origin: sandboxGlobalFilesystemRuleOriginJSON{Harness: "claude", Setting: "read-setting"}},
		{path: path, access: "write", origin: sandboxGlobalFilesystemRuleOriginJSON{Harness: "codex", Setting: "write-setting"}},
	}

	got := mergeSandboxGlobalFilesystemRules(home, candidates)
	require.Len(t, got, 1)
	assert.Equal(t, "~/shared", got[0].Path)
	assert.Equal(t, "write", got[0].Access)
	assert.Equal(t, []string{"claude", "codex"}, got[0].Harnesses)
	require.Len(t, got[0].Origins, 2, "both settings remain visible as provenance")
}
