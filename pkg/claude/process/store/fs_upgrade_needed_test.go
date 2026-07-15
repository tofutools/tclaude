//go:build linux || darwin

package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestUpgradeNeededBindsCoherentCheckpointTemplateAndSource(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	needed, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	require.NoError(t, pathv1.ValidateUpgradeNeeded(needed))
	assert.Equal(t, pathv1.UpgradeMigrationRequired, needed.Reason)
	assert.Equal(t, uint64(0), needed.Checkpoint.Generation)
	assert.NotEmpty(t, needed.Checkpoint.Digest)
	assert.Empty(t, needed.ActiveLegacyIDs)

	source, err := fs.GetTemplateSource(t.Context(), needed.TemplateRef)
	require.NoError(t, err)
	parsed, err := model.Parse(source)
	require.NoError(t, err)
	assert.Equal(t, parsed.SourceHash, needed.TemplateSourceHash)
}

func TestUpgradeNeededCancellationAndSourceMismatchFailClosed(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fs.UpgradeNeeded(canceled, runID)
	assert.ErrorIs(t, err, context.Canceled)

	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	_, hash, ok := strings.Cut(snapshot.Run.TemplateRef, "@sha256:")
	require.True(t, ok)
	sourcePath := filepath.Join(root, "templates", "demo", "sha256-"+hash, "template.yaml")
	require.NoError(t, os.WriteFile(sourcePath, []byte("id: unrelated\n"), 0o644))
	_, err = fs.UpgradeNeeded(t.Context(), runID)
	assert.ErrorIs(t, err, store.ErrRunInconsistent)

	semanticTamper := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: demo
start: other
nodes:
  other:
    type: end
`)
	require.NoError(t, os.WriteFile(sourcePath, semanticTamper, 0o644))
	_, err = fs.UpgradeNeeded(t.Context(), runID)
	assert.ErrorIs(t, err, store.ErrRunInconsistent)
}

func TestUpgradeNeededLegacyTemplateSourceFallbackIsBounded(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	_, hash, ok := strings.Cut(snapshot.Run.TemplateRef, "@sha256:")
	require.True(t, ok)
	versionDir := filepath.Join(root, "templates", "demo", "sha256-"+hash)
	source, err := os.ReadFile(filepath.Join(versionDir, "template.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(versionDir, "template.yaml")))

	needed, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	parsed, err := model.Parse(source)
	require.NoError(t, err)
	assert.Equal(t, parsed.SourceHash, needed.TemplateSourceHash)

	// Find the minimum cumulative budget for the complete fallback view, then
	// prove that one byte less is attributed to the generated source itself.
	low, high := int64(1), int64(64<<20)
	for low < high {
		middle := low + (high-low)/2
		restore := fs.SetViewerResourceLimitsForTest(16<<20, middle, 100_000, 4_096)
		_, probeErr := fs.UpgradeNeeded(t.Context(), runID)
		restore()
		if probeErr == nil {
			high = middle
		} else {
			low = middle + 1
		}
	}
	restore := fs.SetViewerResourceLimitsForTest(16<<20, low-1, 100_000, 4_096)
	_, err = fs.UpgradeNeeded(t.Context(), runID)
	restore()
	var over *store.ExecutionViewOverBudgetError
	require.ErrorAs(t, err, &over)
	assert.Equal(t, "total_bytes", over.Limit)
	assert.Equal(t, "template.yaml fallback", over.Component)
}

func TestUpgradeNeededAppendContentionHasNoFalseTornClassification(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	restore := fs.SetExecutionViewHooksForTest(func() {
		once.Do(func() { close(entered) })
		<-release
	}, nil, nil)
	defer restore()

	type result struct {
		needed pathv1.UpgradeNeeded
		err    error
	}
	upgradeDone := make(chan result, 1)
	go func() {
		needed, err := fs.UpgradeNeeded(t.Context(), runID)
		upgradeDone <- result{needed: needed, err: err}
	}()
	<-entered

	appendDone := make(chan error, 1)
	go func() {
		_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
		appendDone <- err
	}()
	assertStillBlocked(t, appendDone, "append during upgrade readiness")
	close(release)

	upgrade := <-upgradeDone
	require.NoError(t, upgrade.err)
	assert.NotErrorIs(t, upgrade.err, store.ErrWriterInProgress)
	require.NoError(t, pathv1.ValidateUpgradeNeeded(upgrade.needed))
	require.NoError(t, <-appendDone)
}

func TestUpgradeNeededRejectsContextWhileClassifying(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	restore := fs.SetExecutionViewHooksForTest(nil, nil, cancel)
	defer restore()
	_, err := fs.UpgradeNeeded(ctx, runID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}
