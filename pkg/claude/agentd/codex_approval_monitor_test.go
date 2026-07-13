package agentd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexApprovalMonitor_PromotesAlwaysAllow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if agentd.StartCodexApprovalMonitorForTest(t, 15*time.Millisecond) == nil {
		t.Skip("fsnotify watcher unavailable in this environment")
	}

	_, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "7777777777777777")
	require.NoError(t, err)
	f, err := os.OpenFile(profilePath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n[tui.model_availability_nux]\n\"gpt-test\" = 1\n\n" +
		"[apps.asdk_app_69a089a326dc8191b32a3f2553f5be2c.tools.\"linear.save_issue\"]\n" +
		"approval_mode = \"approve\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	configPath := filepath.Join(home, ".codex", "config.toml")
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(configPath)
		return readErr == nil && stringContainsAll(string(data), "linear.save_issue", `approval_mode = "approve"`)
	}, 3*time.Second, 10*time.Millisecond)
}

func TestCodexApprovalMonitor_RefusesTamperedBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if agentd.StartCodexApprovalMonitorForTest(t, 15*time.Millisecond) == nil {
		t.Skip("fsnotify watcher unavailable in this environment")
	}

	_, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "8888888888888888")
	require.NoError(t, err)
	data, err := os.ReadFile(profilePath)
	require.NoError(t, err)
	data[0] = 'X'
	data = append(data, []byte("\n[apps.asdk_app_69a089a326dc8191b32a3f2553f5be2c.tools.\"linear.save_issue\"]\napproval_mode = \"approve\"\n")...)
	require.NoError(t, os.WriteFile(profilePath, data, 0o600))

	configPath := filepath.Join(home, ".codex", "config.toml")
	require.Never(t, func() bool {
		_, statErr := os.Stat(configPath)
		return statErr == nil
	}, 300*time.Millisecond, 20*time.Millisecond)
	assert.NoFileExists(t, configPath)
}

func stringContainsAll(s string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(s, want) {
			return false
		}
	}
	return true
}
