package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestDescribeTclaudeLayerPlanReportsFourClassesWithoutMaterializing(t *testing.T) {
	root := t.TempDir()
	tmuxBase := filepath.Join(root, "tmux")
	require.NoError(t, os.MkdirAll(tmuxBase, 0o755))
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	missing := filepath.Join(root, "missing")
	spec := TclaudeLayerLaunchSpec{
		Version: TclaudeLayerLaunchSpecVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: root, Access: sandboxpolicy.AccessRead},
				{Path: missing, Access: sandboxpolicy.AccessWrite},
			},
		},
		Contract: TclaudeLayerLaunchContract{
			HarnessName: harness.DefaultName,
			StateRoot:   root,
			WriteDirs:   []string{missing},
		},
	}
	got, err := DescribeTclaudeLayerPlan(spec)
	require.NoError(t, err)
	assert.True(t, got.Applicable)
	assert.Equal(t, "host-open", got.NetworkPosture)
	assert.NotEmpty(t, got.Entries)
	classes := map[int]bool{}
	for _, entry := range got.Entries {
		classes[entry.Class] = true
		if entry.Target == missing {
			assert.Equal(t, SandboxPlanMissingWouldSkip, entry.Disposition)
		}
		if entry.Mode == "hide" {
			assert.Equal(t, SandboxPlanHidden, entry.Disposition)
		}
	}
	assert.Equal(t, map[int]bool{1: true, 2: true, 3: true, 4: true}, classes)
	assert.NoFileExists(t, missing, "inspection must not create a missing grant")
}
