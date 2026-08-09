package agentd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexStateRootForLaunchUsesExplicitCodexHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", root)

	got, source, err := codexStateRootForLaunch(harness.CodexName, nil)
	require.NoError(t, err)
	assert.Equal(t, root, got)
	assert.Equal(t, codexStateRootSourceCodexHome, source)
}

func TestCodexStateRootNeverEntersSessionArgv(t *testing.T) {
	root := "/host/private/codex-state"
	newArgv := sessionNewArgs(clcommon.SpawnArgs{Label: "new", Harness: harness.CodexName, CodexStateRoot: root})
	resumeArgv := sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv", Harness: harness.CodexName, CodexStateRoot: root})
	assert.NotContains(t, strings.Join(newArgv, "\x00"), root)
	assert.NotContains(t, strings.Join(resumeArgv, "\x00"), root)
}

func TestCodexStateRootForLaunchMapsGuestRootBackToWrapperHost(t *testing.T) {
	host := t.TempDir()
	snapshot := &sandboxpolicy.Snapshot{Version: sandboxpolicy.SnapshotVersion}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "CODEX_HOME", Value: "/agent/state/codex"}}
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{{
		Path: host, MountPath: "/agent/state", Access: sandboxpolicy.AccessWrite,
	}}

	got, source, err := codexStateRootForLaunch(harness.CodexName, snapshot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(host, "codex"), got)
	assert.Equal(t, codexStateRootSourceCodexHome, source)
}

func TestEnvironmentWithCodexStateRootReplacesAmbientValue(t *testing.T) {
	got := environmentWithCodexStateRoot([]string{"HOME=/home/operator", "CODEX_HOME=/wrong", "PATH=/bin"}, "/right")
	assert.Equal(t, []string{"HOME=/home/operator", "PATH=/bin", "CODEX_HOME=/right"}, got)
}
