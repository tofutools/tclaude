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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
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
	appliedEpoch := loaded.Checkpoint.View().Epochs[1].ID
	artifacts, err := restarted.ReadEpochV8AppliedArtifacts(t.Context(), runID, appliedEpoch)
	require.NoError(t, err)
	wantDiff, _, err := epochv8.EncodeAppliedEpochDiff(loaded.Checkpoint, appliedEpoch)
	require.NoError(t, err)
	assert.Equal(t, wantDiff, artifacts.Diff)
	assert.True(t, artifacts.HasReason)
	assert.Equal(t, []byte("why"), artifacts.Reason)
	_, err = restarted.ReadEpochV8AppliedArtifacts(t.Context(), runID, loaded.Checkpoint.View().Epochs[0].ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
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

func TestEpochV8AuthorizedPublicationLostAckReturnsOriginalProvenance(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record, source0 := putEpochV8Template(t, fs, "epoch-authorized-store", "zero")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
		ID: "epoch-authorized-store", TemplateRef: record.Ref,
	}, source0)
	require.NoError(t, err)
	_, source1 := putEpochV8Template(t, fs, "epoch-authorized-store", "one")
	classification, err := epochv8.ClassifyTemplateSource(source1)
	require.NoError(t, err)
	directives := make([]epochv8.HandoffDirective, 0, len(initialized.Checkpoint.View().ProtectedAuthorities))
	for _, authority := range initialized.Checkpoint.View().ProtectedAuthorities {
		directives = append(directives, epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain})
	}
	preview, err := epochv8.PreviewApply(initialized.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: initialized.Checkpoint.Binding(), Candidate: classification.Candidate(),
		ReasonDigest: digestText("restricted reason"), Handoffs: directives,
	})
	require.NoError(t, err)
	require.NotNil(t, preview.Plan)
	plan := preview.Plan
	lease, err := fs.AcquireMaintenanceLease(t.Context(), initialized.Run.ID, "authorized-maintainer", time.Minute)
	require.NoError(t, err)
	authorization := epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("a", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "agent:agt_store", AppliedAt: "2026-07-21T08:00:00.123Z",
	}

	for _, invalid := range []epochv8.ApplyAuthorization{
		{},
		{HandoffDirectiveDigest: strings.Repeat("a", 64), ReasonCode: "client_reason", Actor: authorization.Actor, AppliedAt: authorization.AppliedAt},
	} {
		_, publishErr := fs.PublishEpochV8Authorized(t.Context(), lease, plan, source1, []byte("restricted reason"), invalid)
		assert.ErrorIs(t, publishErr, epochv8.ErrInvalid)
	}
	loaded, err := fs.LoadEpochV8RunView(t.Context(), initialized.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, initialized.Checkpoint.Binding(), loaded.Checkpoint.Binding(), "invalid provenance must publish nothing")
	_, err = fs.ReadEpochV8AppliedArtifacts(t.Context(), initialized.Run.ID, plan.CandidateEpoch().ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	lostAck := errors.New("lost authorized acknowledgement")
	restore := fs.SetEpochV8PublishHooksForTest(nil, nil, nil, func() error { return lostAck })
	_, err = fs.PublishEpochV8Authorized(t.Context(), lease, plan, source1, []byte("restricted reason"), authorization)
	restore()
	assert.ErrorIs(t, err, lostAck)

	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), lease))
	engine, err := fs.AcquireEngineLease(t.Context(), initialized.Run.ID, "engine", time.Minute)
	require.NoError(t, err)
	_, err = fs.EnsureEpochV8Runtime(t.Context(), engine)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engine))
	beforeReplay, err := fs.LoadEpochV8RunView(t.Context(), initialized.Run.ID)
	require.NoError(t, err)

	replayLease, err := fs.AcquireMaintenanceLease(t.Context(), initialized.Run.ID, "replay-verifier", time.Minute)
	require.NoError(t, err)
	replayed, found, err := fs.VerifyCommittedEpochV8Apply(
		t.Context(), replayLease, plan.BaseBinding(), plan.ProposalDigest(), source1,
		[]byte("restricted reason"), authorization.HandoffDirectiveDigest,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, authorization, replayed.Provenance, "lost-ack replay must return the original server provenance")
	assert.Equal(t, beforeReplay.Checkpoint.Binding(), replayed.Binding)
	_, found, err = fs.VerifyCommittedEpochV8Apply(
		t.Context(), replayLease, plan.BaseBinding(), plan.ProposalDigest(), source1,
		[]byte("restricted reason"), strings.Repeat("b", 64),
	)
	require.NoError(t, err)
	assert.False(t, found, "changed directive identity must not verify as committed")
	afterReplay, err := fs.LoadEpochV8RunView(t.Context(), initialized.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeReplay.CheckpointJSON, afterReplay.CheckpointJSON)
	assert.Equal(t, beforeReplay.RuntimeJSON, afterReplay.RuntimeJSON)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), replayLease))
	artifacts, err := fs.ReadEpochV8AppliedArtifacts(t.Context(), initialized.Run.ID, plan.CandidateEpoch().ID)
	require.NoError(t, err)
	assert.True(t, artifacts.HasReason)
	assert.Equal(t, []byte("restricted reason"), artifacts.Reason)
}

func TestEpochV8AuthorizedStoreRejectsTransferBeforeRuntimeGenesis(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record, source0 := putEpochV8Template(t, fs, "epoch-pregen-transfer", "zero")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
		ID: "epoch-pregen-transfer", TemplateRef: record.Ref,
	}, source0)
	require.NoError(t, err)
	_, source1 := putEpochV8Template(t, fs, "epoch-pregen-transfer", "one")
	plan := previewEpochV8Apply(t, initialized.Checkpoint, source1, "")
	maintenance, err := fs.AcquireMaintenanceLease(t.Context(), initialized.Run.ID, "maintainer", time.Minute)
	require.NoError(t, err)
	_, err = fs.PublishEpochV8Authorized(t.Context(), maintenance, plan, source1, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("f", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T08:30:00Z",
	})
	assert.ErrorIs(t, err, epochv8.ErrInvalid)
	loaded, err := fs.LoadEpochV8RunView(t.Context(), initialized.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, initialized.Checkpoint.Binding(), loaded.Checkpoint.Binding())
	_, err = fs.ReadEpochV8AppliedArtifacts(t.Context(), initialized.Run.ID, plan.CandidateEpoch().ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance))

	engine, err := fs.AcquireEngineLease(t.Context(), initialized.Run.ID, "engine", time.Minute)
	require.NoError(t, err)
	_, err = fs.EnsureEpochV8Runtime(t.Context(), engine)
	require.NoError(t, err, "refused store publication must preserve genesis frontier")
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engine))
}

func TestEpochV8RuntimePublicationAndDurableEngineLeaseGeneration(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, "epoch-runtime", "runtime")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-runtime", TemplateRef: record.Ref}, source)
	require.NoError(t, err)

	now := time.Date(2026, 7, 20, 22, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
	lease1, err := fs.AcquireEngineLease(t.Context(), "epoch-runtime", "engine", time.Minute)
	require.NoError(t, err)
	require.NotZero(t, lease1.Generation)
	require.Len(t, lease1.Token, 64)
	_, err = fs.AcquireEngineLease(t.Context(), "epoch-runtime", "engine", time.Minute)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	assert.ErrorIs(t, fs.ReleaseRunLease(t.Context(), "epoch-runtime", "engine"), store.ErrLeaseHeld)

	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), attached.Checkpoint.View().RuntimeBinding.Revision)
	loaded, err := fs.LoadEpochV8RunView(t.Context(), "epoch-runtime")
	require.NoError(t, err)
	require.NotNil(t, loaded.Runtime)
	assert.Equal(t, attached.Artifact.Digest, loaded.Runtime.Digest)

	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease1))
	lease2, err := fs.AcquireEngineLease(t.Context(), "epoch-runtime", "engine", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, lease2.Generation, lease1.Generation)
	assert.ErrorIs(t, fs.ReleaseEngineLease(t.Context(), lease1), store.ErrLeaseHeld)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease2))

	generationBytes, err := os.ReadFile(filepath.Join(root, "runs", "epoch-runtime", "lease-generation.json"))
	require.NoError(t, err)
	assert.Contains(t, string(generationBytes), strconv.FormatUint(lease2.Generation, 10))
}

func TestEpochV8RuntimeArtifactFirstCrashAndLostAcknowledgementReplay(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, "epoch-runtime-crash", "runtime crash")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-runtime-crash", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	lease, err := fs.AcquireEngineLease(t.Context(), "epoch-runtime-crash", "engine", time.Minute)
	require.NoError(t, err)

	crash := errors.New("crash after runtime artifact")
	restore := fs.SetEpochV8PublishHooksForTest(nil, nil, func() error { return crash }, nil)
	_, err = fs.EnsureEpochV8Runtime(t.Context(), lease)
	assert.ErrorIs(t, err, crash)
	restore()
	orphaned, err := fs.LoadEpochV8RunView(t.Context(), "epoch-runtime-crash")
	require.NoError(t, err)
	assert.Nil(t, orphaned.Runtime)
	entries, err := os.ReadDir(filepath.Join(root, "runs", "epoch-runtime-crash", "runtime"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err)
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, attached.Artifact.Checkpoint))
	require.NoError(t, err)
	plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	require.NoError(t, err)
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	require.NoError(t, err)
	lostAck := errors.New("runtime state committed before acknowledgement")
	restore = fs.SetEpochV8PublishHooksForTest(nil, nil, nil, func() error { return lostAck })
	_, err = fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	assert.ErrorIs(t, err, lostAck)
	restore()
	replayed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, replayed.Disposition)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))
}

func TestEpochV8ExternalSettlementAcceptsLiveEngineAndRejectsMaintenance(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, "epoch-settlement", "fail then rescue")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-settlement", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	engine1, err := fs.AcquireEngineLease(t.Context(), "epoch-settlement", "engine", time.Minute)
	require.NoError(t, err)
	transition := failedEpochV8SettlementTransition(t, fs, "epoch-settlement", source, engine1)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engine1))

	maintenance, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-settlement", "maintainer", time.Minute)
	require.NoError(t, err)
	_, err = fs.AppendEpochV8Settlement(t.Context(), "epoch-settlement", transition)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance))
	unknownLease := store.LeaseRecord{
		RunID: "epoch-settlement", Holder: "future-writer", Kind: store.LeaseKind("future"),
		ExpiresAt: time.Now().UTC().Add(time.Minute), UpdatedAt: time.Now().UTC(),
	}
	unknownJSON, err := json.Marshal(unknownLease)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "epoch-settlement", "lease.json"), append(unknownJSON, '\n'), 0o644))
	_, err = fs.AppendEpochV8Settlement(t.Context(), "epoch-settlement", transition)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	require.NoError(t, os.Remove(filepath.Join(root, "runs", "epoch-settlement", "lease.json")))

	engine2, err := fs.AcquireEngineLease(t.Context(), "epoch-settlement", "engine", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, engine2.Generation, engine1.Generation)
	_, err = fs.EnsureEpochV8Runtime(t.Context(), engine1)
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	type settlementResult struct {
		result epochv8.RuntimeTransitionResult
		err    error
	}
	results := make(chan settlementResult, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, appendErr := fs.AppendEpochV8Settlement(t.Context(), "epoch-settlement", transition)
			results <- settlementResult{result: result, err: appendErr}
		}()
	}
	wg.Wait()
	close(results)
	dispositions := make([]epochv8.Disposition, 0, 2)
	var settled epochv8.RuntimeTransitionResult
	for value := range results {
		require.NoError(t, value.err)
		dispositions = append(dispositions, value.result.Disposition)
		settled = value.result
	}
	assert.ElementsMatch(t, []epochv8.Disposition{epochv8.DispositionApplied, epochv8.DispositionReplayed}, dispositions)
	verified := 0
	for _, authority := range settled.Checkpoint.View().Authorities {
		if authority.Kind == epochv8.AuthorityFrontier && authority.State == epochv8.AuthorityVerifiedUnclaimed {
			verified++
		}
	}
	assert.Equal(t, 1, verified)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engine2))
	runtimeDir := filepath.Join(root, "runs", "epoch-settlement", "runtime")
	entries, err := os.ReadDir(runtimeDir)
	require.NoError(t, err)
	old := time.Now().UTC().Add(-store.EpochV8GCMinOrphanAge - time.Minute)
	for _, entry := range entries {
		require.NoError(t, os.Chtimes(filepath.Join(runtimeDir, entry.Name()), old, old))
	}
	gcLease, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-settlement", "gc", time.Minute)
	require.NoError(t, err)
	gc, err := fs.CollectEpochV8RuntimeGarbage(t.Context(), gcLease, "")
	require.NoError(t, err)
	assert.Greater(t, gc.Removed, 0)
	assert.FileExists(t, filepath.Join(runtimeDir, settled.Artifact.Digest+".json"))
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), gcLease))
}

func TestEpochV8ExternalReportAndSignalAcceptLiveEngineLease(t *testing.T) {
	t.Run("report", func(t *testing.T) {
		fs, err := store.NewFS(t.TempDir())
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-live-report", "report while engine owns lease")
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-live-report", TemplateRef: record.Ref}, source)
		require.NoError(t, err)
		lease, err := fs.AcquireEngineLease(t.Context(), "epoch-live-report", "agentd", time.Minute)
		require.NoError(t, err)
		attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
		require.NoError(t, err)
		input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
		require.NoError(t, err)
		aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, attached.Artifact.Checkpoint))
		require.NoError(t, err)
		plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
		require.NoError(t, err)
		claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
		require.NoError(t, err)
		claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
		require.NoError(t, err)
		input, err = pathv1.VerifyExecutionInput(t.Context(), claimed.Artifact.Checkpoint, source)
		require.NoError(t, err)
		recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
		require.NoError(t, err)
		require.True(t, found)
		observation, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{
			Outcome: "pass", Actor: "agent:agt_report", EvidenceRef: "artifact:report",
		}, false)
		require.NoError(t, err)
		reported, err := fs.AppendEpochV8FinishExternal(t.Context(), "epoch-live-report", observation, observation.PostBinding().Digest)
		require.NoError(t, err)
		assert.Equal(t, epochv8.DispositionApplied, reported.Disposition)
		require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))
	})

	t.Run("signal", func(t *testing.T) {
		fs, err := store.NewFS(t.TempDir())
		require.NoError(t, err)
		tmpl := &model.Template{
			APIVersion: model.APIVersion, Kind: model.Kind, ID: "epoch-live-signal", Start: "wait",
			Nodes: map[string]model.Node{
				"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "release"}, Next: model.Next{"pass": "done"}},
				"done": {Type: model.NodeTypeEnd, Result: "completed"},
			},
		}
		record, err := fs.PutTemplate(t.Context(), tmpl)
		require.NoError(t, err)
		source, err := fs.GetTemplateSource(t.Context(), record.Ref)
		require.NoError(t, err)
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-live-signal", TemplateRef: record.Ref}, source)
		require.NoError(t, err)
		lease, err := fs.AcquireEngineLease(t.Context(), "epoch-live-signal", "agentd", time.Minute)
		require.NoError(t, err)
		attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
		require.NoError(t, err)
		input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
		require.NoError(t, err)
		aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, attached.Artifact.Checkpoint))
		require.NoError(t, err)
		wait, err := pathv1.PlanExclusiveWait(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, time.Now())
		require.NoError(t, err)
		claim, err := pathv1.ClaimExclusiveWait(t.Context(), input, wait)
		require.NoError(t, err)
		claimed, err := fs.AppendEpochV8Advance(t.Context(), lease, claim)
		require.NoError(t, err)
		input, err = pathv1.VerifyExecutionInput(t.Context(), claimed.Artifact.Checkpoint, source)
		require.NoError(t, err)
		wait, found, err := pathv1.RecoverExclusiveWait(t.Context(), input)
		require.NoError(t, err)
		require.True(t, found)
		observation, err := pathv1.ObserveExclusiveWait(t.Context(), input, wait, "agent:agt_signal", "signal:release")
		require.NoError(t, err)
		signaled, err := fs.AppendEpochV8Signal(t.Context(), "epoch-live-signal", observation)
		require.NoError(t, err)
		assert.Equal(t, epochv8.DispositionApplied, signaled.Disposition)
		require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))
	})
}

func failedEpochV8SettlementTransition(t *testing.T, fs *store.FS, runID string, source []byte, lease store.EngineLease) *pathv1.ExecutionTransition {
	t.Helper()
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err)
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, attached.Artifact.Checkpoint))
	require.NoError(t, err)
	plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	require.NoError(t, err)
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	require.NoError(t, err)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	claimReplay, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, claimReplay.Disposition)
	input, err = pathv1.VerifyExecutionInput(t.Context(), claimed.Artifact.Checkpoint, source)
	require.NoError(t, err)
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	require.NoError(t, err)
	require.True(t, found)
	observed, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "fail", Actor: "human:operator"}, false)
	require.NoError(t, err)
	finished, err := fs.AppendEpochV8FinishClaimed(t.Context(), lease, observed, digestText("failed observation"))
	require.NoError(t, err)
	finishReplay, err := fs.AppendEpochV8FinishClaimed(t.Context(), lease, observed, digestText("failed observation"))
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, finishReplay.Disposition)
	input, err = pathv1.VerifyExecutionInput(t.Context(), finished.Artifact.Checkpoint, source)
	require.NoError(t, err)
	transition, err := pathv1.SettleExclusiveAttempt(t.Context(), input, pathv1.AuditedSettlementInput{
		NodeID: "work", BlockedAttempt: 1, Decision: "retry", Actor: "human:operator",
		Reason: "approved rescue", EvidenceRef: "ticket:TCL-604", Timestamp: time.Date(2026, 7, 20, 23, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	return transition
}

func mustDecodePathV1(t *testing.T, data []byte) *pathv1.CheckpointV7 {
	t.Helper()
	checkpoint, err := pathv1.DecodeCheckpointV7(data)
	require.NoError(t, err)
	return checkpoint
}

func TestEpochV8MaintenanceRetainThenTransferPublishesOneRuntimeHead(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record0, source0 := putEpochV8HandoffTemplate(t, fs, "epoch-runtime-apply", "zero")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-runtime-apply", TemplateRef: record0.Ref}, source0)
	require.NoError(t, err)
	engineLease, err := fs.AcquireEngineLease(t.Context(), "epoch-runtime-apply", "engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), engineLease)
	require.NoError(t, err)
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source0)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, attached.Artifact.Checkpoint))
	require.NoError(t, err)
	attempt, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	require.NoError(t, err)
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, attempt)
	require.NoError(t, err)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), engineLease, claim)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engineLease))

	_, source1 := putEpochV8Template(t, fs, "epoch-runtime-apply", "one")
	classification1, err := epochv8.ClassifyTemplateSource(source1)
	require.NoError(t, err)
	directives := make([]epochv8.HandoffDirective, 0, len(claimed.Checkpoint.View().ProtectedAuthorities))
	for _, authority := range claimed.Checkpoint.View().ProtectedAuthorities {
		directives = append(directives, epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain})
	}
	preview1, err := epochv8.PreviewApply(claimed.Checkpoint, epochv8.ApplyDraft{BaseBinding: claimed.Checkpoint.Binding(), Candidate: classification1.Candidate(), Handoffs: directives})
	require.NoError(t, err)
	maintenance1, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-runtime-apply", "maintainer", time.Minute)
	require.NoError(t, err)
	retainAuthorization := epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("b", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T08:10:00Z",
	}
	retained, err := fs.PublishEpochV8RetainAuthorized(t.Context(), maintenance1, preview1.Plan, source1, nil, retainAuthorization)
	require.NoError(t, err)
	retainRetry := retainAuthorization
	retainRetry.AppliedAt = "2026-07-21T08:11:00Z"
	retainedReplay, err := fs.PublishEpochV8RetainAuthorized(t.Context(), maintenance1, preview1.Plan, source1, nil, retainRetry)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, retainedReplay.Disposition)
	assert.Equal(t, retainAuthorization, retained.Provenance)
	assert.Equal(t, retainAuthorization, retainedReplay.Provenance)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance1))
	assert.Equal(t, claimed.Artifact.Digest, retained.Artifact.Digest)

	finishLease, err := fs.AcquireEngineLease(t.Context(), "epoch-runtime-apply", "engine", time.Minute)
	require.NoError(t, err)
	input, err = pathv1.VerifyExecutionInput(t.Context(), retained.Artifact.Checkpoint, source0)
	require.NoError(t, err)
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	require.NoError(t, err)
	require.True(t, found)
	observation, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "pass", Actor: "human:operator"}, false)
	require.NoError(t, err)
	finished, err := fs.AppendEpochV8FinishClaimed(t.Context(), finishLease, observation, digestText("old owner result"))
	require.NoError(t, err)
	input, err = pathv1.VerifyExecutionInput(t.Context(), finished.Artifact.Checkpoint, source0)
	require.NoError(t, err)
	route, err := pathv1.AdvanceExclusiveRoute(t.Context(), input)
	require.NoError(t, err)
	routed, err := fs.AppendEpochV8Advance(t.Context(), finishLease, route)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), finishLease))

	_, source2 := putEpochV8Template(t, fs, "epoch-runtime-apply", "two")
	plan2 := previewEpochV8Apply(t, routed.Checkpoint, source2, "")
	maintenance2, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-runtime-apply", "maintainer", time.Minute)
	require.NoError(t, err)
	transferAuthorization := epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("c", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "agent:agt_store", AppliedAt: "2026-07-21T08:20:00Z",
	}
	transferred, err := fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance2, plan2, source2, nil, transferAuthorization)
	require.NoError(t, err)
	transferRetry := transferAuthorization
	transferRetry.AppliedAt = "2026-07-21T08:21:00Z"
	transferredReplay, err := fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance2, plan2, source2, nil, transferRetry)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, transferredReplay.Disposition)
	assert.Equal(t, transferAuthorization, transferred.Provenance)
	assert.Equal(t, transferAuthorization, transferredReplay.Provenance)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance2))
	assert.NotEqual(t, retained.Artifact.Digest, transferred.Artifact.Digest)
	assert.Equal(t, plan2.CandidateEpoch().ID, transferred.Artifact.EpochID)
	loaded, err := fs.LoadEpochV8RunView(t.Context(), initialized.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, transferred.Artifact.Digest, loaded.Runtime.Digest)
	replayLease, err := fs.AcquireMaintenanceLease(t.Context(), initialized.Run.ID, "replay-verifier", time.Minute)
	require.NoError(t, err)
	verifiedRetain, found, err := fs.VerifyCommittedEpochV8Apply(
		t.Context(), replayLease, preview1.Plan.BaseBinding(), preview1.Plan.ProposalDigest(), source1, nil,
		retainAuthorization.HandoffDirectiveDigest,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, epochv8.RuntimeApplyRetain, verifiedRetain.Kind)
	assert.Equal(t, retainAuthorization, verifiedRetain.Provenance)
	verifiedTransfer, found, err := fs.VerifyCommittedEpochV8Apply(
		t.Context(), replayLease, plan2.BaseBinding(), plan2.ProposalDigest(), source2, nil,
		transferAuthorization.HandoffDirectiveDigest,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, epochv8.RuntimeApplyTransfer, verifiedTransfer.Kind)
	assert.Equal(t, transferAuthorization, verifiedTransfer.Provenance)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), replayLease))
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

func TestEpochV8InitializationReplayRequiresExactInitialAuthority(t *testing.T) {
	t.Run("nil and empty params are equivalent", func(t *testing.T) {
		fs, err := store.NewFS(t.TempDir())
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-empty-params", "initial")
		_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "empty-params", TemplateRef: record.Ref}, source)
		require.NoError(t, err)
		replayed, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
			ID: "empty-params", TemplateRef: record.Ref, Params: map[string]string{},
		}, source)
		require.NoError(t, err)
		assert.Equal(t, store.EpochV8InitializationAlreadyApplied, replayed.Disposition)
	})

	legacyState := func(runID, ref string) state.State {
		checkpoint := state.New(runID, ref, ref, []state.NodeInit{
			{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
			{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		})
		checkpoint.Status = state.RunStatusRunning
		return checkpoint
	}
	t.Run("same run record with legacy state", func(t *testing.T) {
		fs, err := store.NewFS(t.TempDir())
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-legacy", "initial")
		run := store.RunRecord{ID: "legacy-collision", TemplateRef: record.Ref}
		_, err = fs.CreateRun(t.Context(), run, legacyState(run.ID, record.Ref))
		require.NoError(t, err)
		_, err = fs.InitializeEpochV8Run(t.Context(), run, source)
		assert.ErrorIs(t, err, store.ErrRunExists)
	})
	t.Run("same run record with schema-7 state", func(t *testing.T) {
		fs, err := store.NewFS(t.TempDir())
		require.NoError(t, err)
		record, source := putEpochV8Template(t, fs, "epoch-init-v7", "initial")
		run := store.RunRecord{ID: "v7-collision", TemplateRef: record.Ref}
		_, err = fs.CreateRun(t.Context(), run, legacyState(run.ID, record.Ref))
		require.NoError(t, err)
		proof, err := fs.UpgradeNeeded(t.Context(), run.ID)
		require.NoError(t, err)
		_, err = fs.InitializePathV1(t.Context(), run.ID, proof)
		require.NoError(t, err)
		_, err = fs.InitializeEpochV8Run(t.Context(), run, source)
		assert.ErrorIs(t, err, store.ErrRunExists)
	})

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, string, []byte)
	}{
		{name: "tampered checkpoint", mutate: func(t *testing.T, root, runID string, _ []byte) {
			path := filepath.Join(root, "runs", runID, "state.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(data, ' '), 0o644))
		}},
		{name: "tampered source artifact", mutate: func(t *testing.T, root, runID string, source []byte) {
			classification, err := epochv8.ClassifyTemplateSource(source)
			require.NoError(t, err)
			checkpoint, err := epochv8.Initialize(runID, classification.Candidate(), []epochv8.AuthoritySeed{{
				LocalID: store.EpochV8InitialFrontierLocalID, ReservationID: store.EpochV8InitialFrontierReservationID,
				NodeID: "work", Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityVerifiedUnclaimed,
			}})
			require.NoError(t, err)
			epochID := checkpoint.View().OriginalEpoch
			require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "epochs", string(epochID), "source.yaml"), append(source, '\n'), 0o644))
		}},
		{name: "alternate valid initial authority", mutate: func(t *testing.T, root, runID string, source []byte) {
			classification, err := epochv8.ClassifyTemplateSource(source)
			require.NoError(t, err)
			checkpoint, err := epochv8.Initialize(runID, classification.Candidate(), []epochv8.AuthoritySeed{{
				LocalID: "alternate-frontier", ReservationID: "alternate-reservation", NodeID: "work",
				Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityVerifiedUnclaimed,
			}})
			require.NoError(t, err)
			encoded, err := epochv8.EncodeCheckpointV8(checkpoint)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), encoded, 0o644))
		}},
		{name: "valid one-epoch finish history", mutate: func(t *testing.T, root, runID string, source []byte) {
			classification, err := epochv8.ClassifyTemplateSource(source)
			require.NoError(t, err)
			checkpoint, err := epochv8.Initialize(runID, classification.Candidate(), []epochv8.AuthoritySeed{{
				LocalID: store.EpochV8InitialFrontierLocalID, ReservationID: store.EpochV8InitialFrontierReservationID,
				NodeID: "work", Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityClaimed,
			}})
			require.NoError(t, err)
			finished, err := epochv8.FinishClaimed(checkpoint, epochv8.FinishClaim{
				BaseBinding: checkpoint.Binding(), Identity: checkpoint.View().Authorities[0].Identity,
				Result: epochv8.FinishCompleted, EvidenceDigest: digestText("finished"),
			})
			require.NoError(t, err)
			encoded, err := epochv8.EncodeCheckpointV8(finished.Checkpoint)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), encoded, 0o644))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fs, err := store.NewFS(root)
			require.NoError(t, err)
			record, source := putEpochV8Template(t, fs, "epoch-init-adversarial", "initial")
			run := store.RunRecord{ID: "adversarial-replay", TemplateRef: record.Ref}
			_, err = fs.InitializeEpochV8Run(t.Context(), run, source)
			require.NoError(t, err)
			test.mutate(t, root, run.ID, source)
			_, err = fs.InitializeEpochV8Run(t.Context(), run, source)
			assert.ErrorIs(t, err, store.ErrRunExists)
		})
	}
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

func TestEpochV8PublicationProspectiveCoherentReadBudget(t *testing.T) {
	for _, test := range []struct {
		name        string
		limitOffset int64
		wantError   bool
	}{
		{name: "exact boundary remains loadable"},
		{name: "one byte over budget is not published", limitOffset: -1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fs, checkpoint, runID := initializedEpochV8Run(t, root)
			_, nextSource := putEpochV8Template(t, fs, "epoch-budget-next", "next")
			reason := []byte("bounded reason")
			plan := previewEpochV8Apply(t, checkpoint, nextSource, digestBytesForTest(reason))
			transition, err := epochv8.Apply(checkpoint, plan)
			require.NoError(t, err)
			nextCheckpointJSON, err := epochv8.EncodeCheckpointV8(transition.Checkpoint)
			require.NoError(t, err)
			diffJSON, _, err := epochv8.EncodeAppliedEpochDiff(transition.Checkpoint, plan.CandidateEpoch().ID)
			require.NoError(t, err)

			initialEpochID := checkpoint.View().OriginalEpoch
			var currentTotal int64
			for _, path := range []string{
				filepath.Join(root, "runs", runID, "run.json"),
				filepath.Join(root, "runs", runID, "state.json"),
				filepath.Join(root, "runs", runID, "epochs", string(initialEpochID), "source.yaml"),
			} {
				info, err := os.Stat(path)
				require.NoError(t, err)
				currentTotal += info.Size()
			}
			oldCheckpointInfo, err := os.Stat(filepath.Join(root, "runs", runID, "state.json"))
			require.NoError(t, err)
			prospectiveTotal := currentTotal - oldCheckpointInfo.Size() + int64(len(nextCheckpointJSON)) +
				int64(len(nextSource)) + int64(len(diffJSON)) + int64(len(reason))
			limit := prospectiveTotal + test.limitOffset
			restore := fs.SetViewerResourceLimitsForTest(16<<20, limit, 100_000, 4_096)
			defer restore()
			lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
			require.NoError(t, err)
			published, err := fs.PublishEpochV8(t.Context(), lease, plan, nextSource, reason)
			if test.wantError {
				assert.ErrorIs(t, err, store.ErrExecutionViewOverBudget)
				assert.Nil(t, published.Checkpoint)
				_, statErr := os.Stat(filepath.Join(root, "runs", runID, "epochs", string(plan.CandidateEpoch().ID)))
				assert.ErrorIs(t, statErr, os.ErrNotExist)
				loaded, loadErr := fs.LoadEpochV8RunView(t.Context(), runID)
				require.NoError(t, loadErr)
				assert.Len(t, loaded.Checkpoint.View().Epochs, 1)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, published.Checkpoint)
			loaded, err := fs.LoadEpochV8RunView(t.Context(), runID)
			require.NoError(t, err)
			assert.Equal(t, published.Binding, loaded.Checkpoint.Binding())
			assert.Len(t, loaded.Checkpoint.View().Epochs, 2)
		})
	}
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

func TestEpochV8ExactBaseLeaseGenerationUpgrade(t *testing.T) {
	now := time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC)

	t.Run("first_engine", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-first-engine")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		lease, err := fs.AcquireEngineLease(t.Context(), runID, "engine", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, uint64(1), lease.Generation)
		_, err = fs.EnsureEpochV8Runtime(t.Context(), lease)
		require.NoError(t, err)
		require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))
	})

	t.Run("first_maintenance", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-first-maintenance")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, uint64(1), lease.Generation)
		require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), lease))
	})

	t.Run("active_legacy_lease_remains_held", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-active-legacy")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		writeLeaseFixture(t, root, store.LeaseRecord{RunID: runID, Holder: "legacy", ExpiresAt: now.Add(time.Minute), UpdatedAt: now})
		_, err := fs.AcquireEngineLease(t.Context(), runID, "engine", time.Minute)
		assert.ErrorIs(t, err, store.ErrLeaseHeld)
		_, err = os.Stat(filepath.Join(root, "runs", runID, "lease-generation.json"))
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("stale_base_maintenance_lease_sets_floor", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-stale-maintenance")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		writeLeaseFixture(t, root, store.LeaseRecord{
			RunID: runID, Holder: "old-maintainer", Kind: store.LeaseKindMaintenance, Token: strings.Repeat("a", 64),
			ExpiresAt: now.Add(-time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
		})
		lease, err := fs.AcquireEngineLease(t.Context(), runID, "engine", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, uint64(1), lease.Generation)
	})

	t.Run("stale_tokenized_lease_preserves_generation_floor", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-stale-tokenized")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		writeLeaseFixture(t, root, store.LeaseRecord{
			RunID: runID, Holder: "old-engine", Kind: store.LeaseKindEngine, Token: strings.Repeat("b", 64), Generation: 7,
			ExpiresAt: now.Add(-time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
		})
		lease, err := fs.AcquireMaintenanceLease(t.Context(), runID, "maintainer", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, uint64(8), lease.Generation)
	})

	t.Run("crash_after_burn_never_reuses_generation", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := exactBaseEpochV8Fixture(t, root, "base-burn-crash")
		t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
		crash := errors.New("crash after generation burn")
		restore := fs.SetLeaseGenerationAfterBurnHookForTest(func() error { return crash })
		_, err := fs.AcquireEngineLease(t.Context(), runID, "engine", time.Minute)
		restore()
		assert.ErrorIs(t, err, crash)
		_, err = os.Stat(filepath.Join(root, "runs", runID, "lease.json"))
		assert.ErrorIs(t, err, os.ErrNotExist)
		generation, err := os.ReadFile(filepath.Join(root, "runs", runID, "lease-generation.json"))
		require.NoError(t, err)
		assert.JSONEq(t, `{"generation":1}`, string(generation))
		lease, err := fs.AcquireEngineLease(t.Context(), runID, "engine", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, uint64(2), lease.Generation)
	})
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
	youngPath := filepath.Join(epochsDir, ".epochv8-young-orphan")
	require.NoError(t, os.Mkdir(youngPath, 0o755))
	forged := lease
	forged.Token = strings.Repeat("f", len(lease.Token))
	_, err = fs.CollectEpochV8Garbage(t.Context(), forged, "")
	assert.ErrorIs(t, err, store.ErrLeaseHeld)
	removed := 0
	cursor := ""
	for calls := 0; calls < 10; calls++ {
		result, err := fs.CollectEpochV8Garbage(t.Context(), lease, cursor)
		require.NoError(t, err)
		assert.LessOrEqual(t, result.Scanned, store.EpochV8GCMaxEntries)
		assert.LessOrEqual(t, result.Removed, result.Scanned)
		removed += result.Removed
		if result.Complete {
			break
		}
		require.NotEmpty(t, result.NextCursor)
		cursor = result.NextCursor
	}
	assert.Equal(t, store.EpochV8GCMaxEntries+5, removed)
	assert.DirExists(t, filepath.Join(epochsDir, string(checkpoint.View().OriginalEpoch)))
	assert.DirExists(t, youngPath)
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

// exactBaseEpochV8Fixture removes only the runtime directory and generation
// tombstone added after 0c84f616, leaving the schema-8 bytes created by that
// exact base for upgrade compatibility coverage.
func exactBaseEpochV8Fixture(t *testing.T, root, runID string) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, runID, "exact base fixture")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	runDir := filepath.Join(root, "runs", runID)
	require.NoError(t, os.Remove(filepath.Join(runDir, "lease-generation.json")))
	require.NoError(t, os.RemoveAll(filepath.Join(runDir, "runtime")))
	return fs, runID
}

func writeLeaseFixture(t *testing.T, root string, lease store.LeaseRecord) {
	t.Helper()
	data, err := json.Marshal(lease)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", lease.RunID, "lease.json"), append(data, '\n'), 0o644))
}

func putEpochV8Template(t *testing.T, fs *store.FS, id, prompt string) (store.TemplateRecord, []byte) {
	t.Helper()
	tmpl := epochV8Template(id, prompt)
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	source, err := fs.GetTemplateSource(t.Context(), record.Ref)
	require.NoError(t, err)
	classification, err := epochv8.ClassifyTemplateSource(source)
	require.NoError(t, err)
	require.NotNil(t, classification.Candidate(), "template eligibility: %s", classification.Reason)
	return record, source
}

func putEpochV8HandoffTemplate(t *testing.T, fs *store.FS, id, prompt string) (store.TemplateRecord, []byte) {
	t.Helper()
	tmpl := epochV8Template(id, prompt)
	tmpl.Nodes["work"] = model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "handoff"}}
	tmpl.Nodes["handoff"] = model.Node{Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "new work"}, Next: model.Next{"pass": "done"}}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	source, err := fs.GetTemplateSource(t.Context(), record.Ref)
	require.NoError(t, err)
	classification, err := epochv8.ClassifyTemplateSource(source)
	require.NoError(t, err)
	require.NotNil(t, classification.Candidate())
	return record, source
}

func epochV8Template(id, prompt string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
}

func previewEpochV8Apply(t *testing.T, checkpoint *epochv8.CheckpointV8, source []byte, reasonDigest string) *epochv8.ApplyPlan {
	t.Helper()
	classification, err := epochv8.ClassifyTemplateSource(source)
	require.NoError(t, err)
	view := checkpoint.View()
	var frontier epochv8.AuthorityRecord
	for _, authority := range view.ProtectedAuthorities {
		if authority.State == epochv8.AuthorityVerifiedUnclaimed {
			frontier = authority
		}
	}
	require.NotEmpty(t, frontier.Identity)
	targetLocalID := "next-frontier"
	targetReservationID := "next-reservation"
	if len(view.Epochs) > 1 {
		suffix := "-" + strconv.Itoa(len(view.Epochs))
		targetLocalID += suffix
		targetReservationID += suffix
	}
	handoffs := make([]epochv8.HandoffDirective, 0, len(view.ProtectedAuthorities))
	for _, authority := range view.ProtectedAuthorities {
		directive := epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain}
		if authority.Identity == frontier.Identity {
			directive = epochv8.HandoffDirective{
				Source: authority.Identity, Action: epochv8.HandoffTransfer,
				TargetLocalID: targetLocalID, TargetReservationID: targetReservationID, TargetNodeID: "work",
			}
		}
		handoffs = append(handoffs, directive)
	}
	preview, err := epochv8.PreviewApply(checkpoint, epochv8.ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: classification.Candidate(), ReasonDigest: reasonDigest,
		Handoffs: handoffs,
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
