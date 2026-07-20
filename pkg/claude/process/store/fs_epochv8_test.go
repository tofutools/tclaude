//go:build linux || darwin

package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestEpochV8InitializeReadPublishAndExactReplay(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	runID := "epoch-run"
	initialRecord, initialSource := putEpochV8Template(t, fs, "epoch-demo", "initial")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
		ID: runID, TemplateRef: initialRecord.Ref, Params: map[string]string{"scope": "test"},
	}, initialSource)
	require.NoError(t, err)
	assert.Equal(t, store.EpochV8InitializationApplied, initialized.Disposition)
	view := initialized.Checkpoint.View()
	require.Len(t, view.Epochs, 1)
	require.Len(t, view.Authorities, 1)
	assert.Equal(t, store.EpochV8InitialFrontierLocalID, view.Authorities[0].LocalID)
	assert.Equal(t, store.EpochV8InitialFrontierReservationID, view.Authorities[0].ReservationID)
	assert.Equal(t, "work", view.Authorities[0].NodeID)
	assert.Equal(t, epochv8.AuthorityVerifiedUnclaimed, view.Authorities[0].State)

	replayInit, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
		ID: runID, TemplateRef: initialRecord.Ref, Params: map[string]string{"scope": "test"},
	}, initialSource)
	require.NoError(t, err)
	assert.Equal(t, store.EpochV8InitializationAlreadyApplied, replayInit.Disposition)

	nextRecord, nextSource := putEpochV8Template(t, fs, "epoch-demo", "next")
	plan := previewEpochV8Apply(t, initialized.Checkpoint, nextSource, digestText("why"))
	require.Equal(t, nextRecord.Ref, plan.CandidateEpoch().TemplateRef)

	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	restoreClock := fs.SetNowForTest(func() time.Time { return now })
	t.Cleanup(restoreClock)
	lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer-a", time.Minute)
	require.NoError(t, err)
	require.Len(t, lease.Token, 64)
	_, err = fs.AcquireRunLease(t.Context(), runID, "engine", time.Minute)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)

	lostAck := errors.New("lost acknowledgement")
	restoreHooks := fs.SetEpochV8PublishHooksForTest(nil, nil, nil, func() error { return lostAck })
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, []byte("why"))
	restoreHooks()
	assert.ErrorIs(t, err, lostAck)

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	t.Cleanup(restarted.SetNowForTest(func() time.Time { return now }))
	loaded, err := restarted.LoadEpochV8RunView(t.Context(), runID)
	require.NoError(t, err)
	require.Len(t, loaded.Checkpoint.View().Epochs, 2)
	owner := findEpochOwner(t, loaded.Checkpoint, "next-frontier")
	ownerSource, err := loaded.SourceForOwner(owner.Identity)
	require.NoError(t, err)
	assert.Equal(t, nextSource, ownerSource)

	now = now.Add(2 * time.Minute)
	lease2, err := restarted.AcquireMaintenanceLease(t.Context(), runID, "maintainer-b", time.Minute)
	require.NoError(t, err)
	assert.NotEqual(t, lease.Token, lease2.Token)
	replayed, err := restarted.PublishEpochV8(t.Context(), lease2, plan, nextSource, []byte("why"))
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, replayed.Disposition)
	assert.ErrorIs(t, restarted.ReleaseMaintenanceLease(t.Context(), lease), store.ErrLeaseHeld)
	require.NoError(t, restarted.ReleaseMaintenanceLease(t.Context(), lease2))
	reasonInfo, err := os.Stat(filepath.Join(root, "runs", runID, "epochs", string(plan.CandidateEpoch().ID), "reason.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), reasonInfo.Mode().Perm())
}

func TestEpochV8InitializationCrashBoundariesReplayDurability(t *testing.T) {
	t.Run("before rename leaves no run", func(t *testing.T) {
		root := t.TempDir()
		fs, err := store.NewFS(root)
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-before", "initial")
		injected := errors.New("before rename")
		restore := fs.SetEpochV8InitializeHooksForTest(func() error { return injected }, nil)
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-before", TemplateRef: record.Ref}, source)
		restore()
		assert.ErrorIs(t, err, injected)
		_, statErr := os.Stat(filepath.Join(root, "runs", "epoch-init-before"))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("lost acknowledgement exact replay", func(t *testing.T) {
		root := t.TempDir()
		fs, err := store.NewFS(root)
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-after", "initial")
		injected := errors.New("after rename")
		restore := fs.SetEpochV8InitializeHooksForTest(nil, func() error { return injected })
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-after", TemplateRef: record.Ref}, source)
		restore()
		assert.ErrorIs(t, err, injected)
		replay, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-after", TemplateRef: record.Ref}, source)
		require.NoError(t, err)
		assert.Equal(t, store.EpochV8InitializationAlreadyApplied, replay.Disposition)
	})

	t.Run("replay repeats parent durability", func(t *testing.T) {
		root := t.TempDir()
		fs, err := store.NewFS(root)
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-sync", "initial")
		injected := errors.New("parent sync")
		restore := fs.SetEpochV8InitializeDirSyncHookForTest(func() error { return injected })
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-sync", TemplateRef: record.Ref}, source)
		assert.ErrorIs(t, err, injected)
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-sync", TemplateRef: record.Ref}, source)
		assert.ErrorIs(t, err, injected)
		restore()
		replay, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-init-sync", TemplateRef: record.Ref}, source)
		require.NoError(t, err)
		assert.Equal(t, store.EpochV8InitializationAlreadyApplied, replay.Disposition)
	})
}

func TestEpochV8PublicationCrashBeforeCheckpointRetriesExactAndStaleNeverPublishes(t *testing.T) {
	root := t.TempDir()
	fs, checkpoint, runID := initializedEpochV8Run(t, root)
	_, nextSource := putEpochV8Template(t, fs, "epoch-demo", "crash-next")
	plan := previewEpochV8Apply(t, checkpoint, nextSource, "")
	lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
	require.NoError(t, err)

	crash := errors.New("crash after epoch rename")
	restore := fs.SetEpochV8PublishHooksForTest(nil, func() error { return crash }, nil, nil)
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, nil)
	restore()
	assert.ErrorIs(t, err, crash)
	beforeRetry, err := fs.LoadEpochV8RunView(t.Context(), runID)
	require.NoError(t, err)
	assert.Len(t, beforeRetry.Checkpoint.View().Epochs, 1)
	assert.DirExists(t, filepath.Join(root, "runs", runID, "epochs", string(plan.CandidateEpoch().ID)))

	applied, err := fs.PublishEpochV8(t.Context(), lease, plan, nextSource, nil)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionApplied, applied.Disposition)

	_, staleSource := putEpochV8Template(t, fs, "epoch-demo", "stale")
	stalePlan := previewEpochV8Apply(t, checkpoint, staleSource, "")
	stale, err := fs.PublishEpochV8(t.Context(), lease, stalePlan, staleSource, nil)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionStale, stale.Disposition)
	staleAgain, err := fs.PublishEpochV8(t.Context(), lease, stalePlan,
		make([]byte, store.EpochV8MaxSourceBytes+1), make([]byte, store.EpochV8MaxReasonBytes+1))
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionStale, staleAgain.Disposition, "stale CAS must be decided before artifact validation")
	_, statErr := os.Stat(filepath.Join(root, "runs", runID, "epochs", string(stalePlan.CandidateEpoch().ID)))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestEpochV8ReadFailsClosedOnArtifactAndCheckpointTampering(t *testing.T) {
	root := t.TempDir()
	fs, checkpoint, runID := initializedEpochV8Run(t, root)
	_, nextSource := putEpochV8Template(t, fs, "epoch-demo", "tamper")
	plan := previewEpochV8Apply(t, checkpoint, nextSource, digestText("reason"))
	lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
	require.NoError(t, err)
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, []byte("reason"))
	require.NoError(t, err)
	epochDir := filepath.Join(root, "runs", runID, "epochs", string(plan.CandidateEpoch().ID))
	require.NoError(t, os.WriteFile(filepath.Join(epochDir, "diff.json"), []byte("{}\n"), 0o644))
	_, err = fs.LoadEpochV8RunView(t.Context(), runID)
	assert.ErrorIs(t, err, store.ErrRunInconsistent)
}

func TestEpochV8CoherentReadUsesOneCumulativeBudget(t *testing.T) {
	root := t.TempDir()
	fs, checkpoint, runID := initializedEpochV8Run(t, root)
	epochID := checkpoint.View().OriginalEpoch
	paths := []string{
		filepath.Join(root, "runs", runID, "run.json"),
		filepath.Join(root, "runs", runID, "state.json"),
		filepath.Join(root, "runs", runID, "epochs", string(epochID), "source.yaml"),
	}
	var total int64
	for _, path := range paths {
		info, err := os.Stat(path)
		require.NoError(t, err)
		total += info.Size()
	}
	restore := fs.SetViewerResourceLimitsForTest(16<<20, total, 100_000, 4_096)
	_, err := fs.LoadEpochV8RunView(t.Context(), runID)
	require.NoError(t, err)
	restore()
	restore = fs.SetViewerResourceLimitsForTest(16<<20, total-1, 100_000, 4_096)
	defer restore()
	_, err = fs.LoadEpochV8RunView(t.Context(), runID)
	assert.ErrorIs(t, err, store.ErrExecutionViewOverBudget)
}

func TestEpochV8ReasonBoundsAndMaintenanceTokenChecks(t *testing.T) {
	root := t.TempDir()
	fs, checkpoint, runID := initializedEpochV8Run(t, root)
	_, nextSource := putEpochV8Template(t, fs, "epoch-demo", "bounded")
	reason := bytes.Repeat([]byte("r"), store.EpochV8MaxReasonBytes)
	plan := previewEpochV8Apply(t, checkpoint, nextSource, digestBytesForTest(reason))
	lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, strings.Repeat("h", store.MaxLeaseHolderBytes), store.MaxLeaseTTL)
	require.NoError(t, err)
	forged := lease
	forged.Token = strings.Repeat("0", 64)
	_, err = fs.PublishEpochV8(t.Context(), forged, plan, nextSource, reason)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, append(reason, 'x'))
	assert.ErrorIs(t, err, store.ErrExecutionViewOverBudget)
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, reason)
	require.NoError(t, err)

	_, err = fs.AcquireMaintenanceLease(t.Context(), runID, "too-long", store.MaxLeaseTTL+time.Nanosecond)
	assert.Error(t, err)
}

func TestTypedLeaseDomainTreatsLegacyUntypedAsEngine(t *testing.T) {
	root := t.TempDir()
	fs, _, runID := initializedEpochV8Run(t, root)
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
	legacy := map[string]any{
		"runId": runID, "holder": "legacy-engine",
		"expiresAt": now.Add(time.Minute), "updatedAt": now,
	}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "lease.json"), append(data, '\n'), 0o644))
	_, err = fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	engineLease, err := fs.AcquireRunLease(t.Context(), runID, "legacy-engine", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, store.LeaseKindEngine, engineLease.Kind)
	require.NoError(t, fs.ReleaseRunLease(t.Context(), runID, "legacy-engine"))

	maintenance, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
	require.NoError(t, err)
	now = now.Add(30 * time.Second)
	renewed, err := fs.RenewMaintenanceLease(t.Context(), maintenance, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Minute), renewed.ExpiresAt)
	forged := renewed
	forged.Token = strings.Repeat("f", 64)
	_, err = fs.RenewMaintenanceLease(t.Context(), forged, time.Minute)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	assert.ErrorIs(t, fs.ReleaseMaintenanceLease(t.Context(), forged), store.ErrLeaseHeld)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), renewed))
}

func TestEpochV8GarbageCollectionIsLeaseBoundedAndPreservesReferences(t *testing.T) {
	root := t.TempDir()
	fs, checkpoint, runID := initializedEpochV8Run(t, root)
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
	lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
	require.NoError(t, err)
	epochsDir := filepath.Join(root, "runs", runID, "epochs")
	old := now.Add(-store.EpochV8GCMinOrphanAge - time.Second)
	for i := range store.EpochV8GCMaxEntries + 5 {
		path := filepath.Join(epochsDir, ".epochv8-orphan-"+strings.Repeat("x", 3)+"-"+time.Unix(int64(i), 0).Format("150405"))
		require.NoError(t, os.Mkdir(path, 0o755))
		require.NoError(t, os.Chtimes(path, old, old))
	}
	result, err := fs.CollectEpochV8Garbage(t.Context(), lease)
	require.NoError(t, err)
	assert.Equal(t, store.EpochV8GCMaxEntries, result.Scanned)
	assert.Equal(t, store.EpochV8GCMaxEntries, result.Removed)
	assert.DirExists(t, filepath.Join(epochsDir, string(checkpoint.View().OriginalEpoch)))
}

func TestEpochV8TemplateDeletionTracksEveryEpochAndTamperingBlocksGlobally(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	initial, initialSource := putEpochV8Template(t, fs, "epoch-original", "initial")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-delete-run", TemplateRef: initial.Ref}, initialSource)
	require.NoError(t, err)
	_, nextSource := putEpochV8Template(t, fs, "epoch-successor", "next")
	plan := previewEpochV8Apply(t, initialized.Checkpoint, nextSource, "")
	lease, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-delete-run", "maintainer", time.Minute)
	require.NoError(t, err)
	_, err = fs.PublishEpochV8(t.Context(), lease, plan, nextSource, nil)
	require.NoError(t, err)
	assert.ErrorIs(t, fs.DeleteTemplate(t.Context(), "epoch-original"), store.ErrTemplateInUse)
	assert.ErrorIs(t, fs.DeleteTemplate(t.Context(), "epoch-successor"), store.ErrTemplateInUse)

	unrelated, _ := putEpochV8Template(t, fs, "epoch-unrelated", "unrelated")
	diffPath := filepath.Join(root, "runs", "epoch-delete-run", "epochs", string(plan.CandidateEpoch().ID), "diff.json")
	require.NoError(t, os.WriteFile(diffPath, []byte("tampered\n"), 0o644))
	err = fs.DeleteTemplate(t.Context(), unrelated.ID)
	assert.ErrorIs(t, err, store.ErrTemplateInUse)
	var inUse *store.TemplateInUseError
	require.ErrorAs(t, err, &inUse)
	assert.Contains(t, inUse.UnreadableRunIDs, "epoch-delete-run")
}

func TestRunStateSchemaClassifierIsExhaustive(t *testing.T) {
	for version := 1; version <= 6; version++ {
		kind, err := store.ClassifyRunStateSchema(version)
		require.NoError(t, err)
		assert.Equal(t, store.RunSchemaLegacy, kind)
	}
	kind, err := store.ClassifyRunStateSchema(7)
	require.NoError(t, err)
	assert.Equal(t, store.RunSchemaResetRequired, kind)
	kind, err = store.ClassifyRunStateSchema(8)
	require.NoError(t, err)
	assert.Equal(t, store.RunSchemaEpochV8, kind)
	for _, version := range []int{0, -1, 9, 999} {
		_, err := store.ClassifyRunStateSchema(version)
		assert.ErrorIs(t, err, store.ErrUnsupportedRunSchema)
	}
}

func initializedEpochV8Run(t *testing.T, root string) (*store.FS, *epochv8.CheckpointV8, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, "epoch-demo", "initial")
	result, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-run", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	return fs, result.Checkpoint, "epoch-run"
}

func putEpochV8Template(t *testing.T, fs *store.FS, id, prompt string) (store.TemplateRecord, []byte) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	source, err := fs.GetTemplateSource(t.Context(), record.Ref)
	require.NoError(t, err)
	classification, err := epochv8.ClassifyTemplateSource(source)
	require.NoError(t, err)
	require.NotNil(t, classification.Candidate(), "template eligibility: %s", classification.Reason)
	return record, source
}

func previewEpochV8Apply(t *testing.T, checkpoint *epochv8.CheckpointV8, source []byte, reasonDigest string) *epochv8.ApplyPlan {
	t.Helper()
	classification, err := epochv8.ClassifyTemplateSource(source)
	require.NoError(t, err)
	frontier := checkpoint.View().Authorities[0]
	preview, err := epochv8.PreviewApply(checkpoint, epochv8.ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: classification.Candidate(), ReasonDigest: reasonDigest,
		Handoffs: []epochv8.HandoffDirective{{
			Source: frontier.Identity, Action: epochv8.HandoffTransfer,
			TargetLocalID: "next-frontier", TargetReservationID: "next-reservation", TargetNodeID: "work",
		}},
	})
	require.NoError(t, err)
	require.Empty(t, preview.Blockers)
	require.NotNil(t, preview.Plan)
	return preview.Plan
}

func findEpochOwner(t *testing.T, checkpoint *epochv8.CheckpointV8, localID string) epochv8.AuthorityRecord {
	t.Helper()
	for _, authority := range checkpoint.View().Authorities {
		if authority.LocalID == localID {
			return authority
		}
	}
	t.Fatalf("authority %q not found", localID)
	return epochv8.AuthorityRecord{}
}

func digestText(value string) string { return digestBytesForTest([]byte(value)) }

func digestBytesForTest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
