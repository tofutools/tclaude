package session

import (
	"encoding/json"
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
	workspace := filepath.Join(root, "workspace")
	stateRoot := filepath.Join(root, "state")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(stateRoot, 0o755))
	missing := filepath.Join(root, "missing")
	snapshot := sandboxpolicy.NewSnapshot(
		sandboxpolicy.EffectiveProfile{
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: workspace, Access: sandboxpolicy.AccessRead},
				{Path: missing, Access: sandboxpolicy.AccessWrite},
			},
		},
		nil,
	)
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName,
		Cwd:         workspace,
		StateRoot:   stateRoot,
		Snapshot:    &snapshot,
	})
	require.NoError(t, err)
	before, err := json.Marshal(spec)
	require.NoError(t, err)
	assert.NotContains(t, spec.Effective.Filesystem,
		sandboxpolicy.FilesystemGrant{Path: missing, Access: sandboxpolicy.AccessWrite},
		"the production launch spec keeps filtering missing positive binds")

	got, err := DescribeTclaudeLayerPlan(spec, snapshot.Effective)
	require.NoError(t, err)
	after, err := json.Marshal(spec)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"inspection must not mutate the byte-identical production launch spec")
	assert.True(t, got.Applicable)
	assert.Equal(t, "composed", got.Coverage)
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

func TestDescribeRecordedEffectivePlanMarksUnpersistedContractUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMUX_TMPDIR", root)
	presentNow := filepath.Join(root, "present-now")
	require.NoError(t, os.MkdirAll(presentNow, 0o755))
	got, err := DescribeRecordedEffectivePlan(sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: presentNow, Access: sandboxpolicy.AccessWrite},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "recorded-effective-only", got.Coverage)
	require.Len(t, got.Unavailable, 3)
	assert.Contains(t, got.Unavailable[0], "not recorded at launch — unavailable")
	assert.Contains(t, got.Unavailable[0], "hypothetical mode")
	require.Len(t, got.UnavailableEntries, 1)
	assert.Equal(t, presentNow, got.UnavailableEntries[0].Target)
	assert.Contains(t, got.UnavailableEntries[0].Reason,
		"launch-time presence was not recorded")
	for _, entry := range got.Entries {
		assert.NotEqual(t, 1, entry.Class,
			"recorded mode must not reconstruct launch-contract rows")
		assert.NotContains(t, entry.Origin, "daemon-final")
		assert.NotEqual(t, presentNow, entry.Target,
			"current path presence must not become a recorded disposition")
	}
}
