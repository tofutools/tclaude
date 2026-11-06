//go:build linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// A Claude subscription login refreshes its access token from inside the
// sandbox and writes the rotated token back to $CLAUDE_CONFIG_DIR/
// .credentials.json, which every tclaude Claude launch pins to ~/.claude. This
// smoke is the real-bubblewrap evidence that the write lands on the host: the
// state root is a read-write bind and the credentials file is not in the
// harness-config floor. If it did not land, the next launch would read a
// refresh token the server has already rotated away.
//
// It rides on the floor/policy flow's fixture only because runProxyEngineLaunch
// is the one construction site for a real tclaude-layer Claude launch spec; the
// network policy plays no part in what it checks.

const claudeCredentialWriteBackHelperEnv = "TCLAUDE_CLAUDE_CREDENTIAL_WRITEBACK_HELPER"

const (
	claudeCredentialsBefore = `{"claudeAiOauth":{"refreshToken":"before"}}`
	claudeCredentialsDirect = `{"claudeAiOauth":{"refreshToken":"direct"}}`
	claudeCredentialsRename = `{"claudeAiOauth":{"refreshToken":"renamed"}}`
)

func TestTclaudeLayerClaudeCredentialWriteBackSmoke(t *testing.T) {
	if os.Getenv(proxySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_PROXY_SMOKE=1 on the executing Linux CI boundary")
	}
	fixture := proxySmokeRunnerFixture(t)
	var credentials string
	helperBinaryName := "credential-writeback-helper"
	launch := runProxyEngineLaunch(t, proxyEngineLaunchInput{
		Rules:    proxySmokeRules(fixture),
		ExtraEnv: []string{claudeCredentialWriteBackHelperEnv + "=1"},
		Command: func(workspace string) string {
			return clcommon.ShellQuoteArg(filepath.Join(workspace, helperBinaryName)) +
				" -test.run=^TestTclaudeLayerClaudeCredentialWriteBackHelper$ -test.v"
		},
		WorkspaceBinaries: map[string]string{helperBinaryName: os.Args[0]},
		PrepareHome: func(t *testing.T, home, _ string) map[string]string {
			stateRoot := filepath.Join(home, ".claude")
			require.NoError(t, os.MkdirAll(stateRoot, 0o700))
			credentials = filepath.Join(stateRoot, ".credentials.json")
			require.NoError(t, os.WriteFile(credentials, []byte(claudeCredentialsBefore), 0o600))
			// Production pins this for every Claude launch (ApplyClaudeConfigDirEnv).
			return map[string]string{"CLAUDE_CONFIG_DIR": stateRoot}
		},
	})
	assert.Contains(t, launch.Output, "credential-writeback: direct overwrite: written")
	assert.Contains(t, launch.Output, "credential-writeback: atomic replace: written")
	got, err := os.ReadFile(credentials)
	require.NoError(t, err)
	assert.Equal(t, claudeCredentialsRename, string(got),
		"the token the sandboxed Claude Code wrote must be what the host reads next")
}

// TestTclaudeLayerClaudeCredentialWriteBackHelper runs inside the sandbox and
// rewrites the credentials file both ways a refresh may: in place, and through
// a sibling temporary file renamed over it.
func TestTclaudeLayerClaudeCredentialWriteBackHelper(t *testing.T) {
	if os.Getenv(claudeCredentialWriteBackHelperEnv) != "1" {
		t.Skip("claude credential write-back smoke helper")
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	require.NotEmpty(t, configDir)
	path := filepath.Join(configDir, ".credentials.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err, "the stored login must be readable inside the sandbox")
	require.Equal(t, claudeCredentialsBefore, string(before))

	require.NoError(t, os.WriteFile(path, []byte(claudeCredentialsDirect), 0o600))
	fmt.Println("credential-writeback: direct overwrite: written")

	temporary := filepath.Join(configDir, fmt.Sprintf(".credentials.json.tmp.%d", os.Getpid()))
	require.NoError(t, os.WriteFile(temporary, []byte(claudeCredentialsRename), 0o600))
	require.NoError(t, os.Rename(temporary, path))
	fmt.Println("credential-writeback: atomic replace: written")
}
