//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

type epochRuntimeAppendAuthority struct {
	engine   *EngineLease
	external bool
}

func (s *FS) EnsureEpochV8Runtime(ctx context.Context, lease EngineLease) (epochv8.RuntimeTransitionResult, error) {
	return s.mutateEpochV8Runtime(ctx, lease.RunID, epochRuntimeAppendAuthority{engine: &lease}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		if snapshot.Runtime != nil {
			return epochv8.RuntimeTransitionResult{Checkpoint: snapshot.Checkpoint, Artifact: snapshot.Runtime, ArtifactJSON: snapshot.RuntimeJSON, Disposition: epochv8.DispositionReplayed, Binding: snapshot.Checkpoint.Binding()}, nil
		}
		return epochv8.AttachGenesis(ctx, snapshot.Checkpoint, source)
	})
}

func (s *FS) AppendEpochV8Advance(ctx context.Context, lease EngineLease, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	return s.mutateEpochV8Runtime(ctx, lease.RunID, epochRuntimeAppendAuthority{engine: &lease}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		return epochv8.AdvanceHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition)
	})
}

func (s *FS) AppendEpochV8ClaimExternal(ctx context.Context, lease EngineLease, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	return s.mutateEpochV8Runtime(ctx, lease.RunID, epochRuntimeAppendAuthority{engine: &lease}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		return epochv8.ClaimExternal(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition)
	})
}

func (s *FS) AppendEpochV8FinishClaimed(ctx context.Context, lease EngineLease, transition *pathv1.ExecutionTransition, evidenceDigest string) (epochv8.RuntimeTransitionResult, error) {
	return s.mutateEpochV8Runtime(ctx, lease.RunID, epochRuntimeAppendAuthority{engine: &lease}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		return epochv8.FinishClaimedHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition, evidenceDigest)
	})
}

// AppendEpochV8Signal is the external no-engine-credential append authority.
// A live engine lease is deliberately compatible; maintenance is not.
func (s *FS) AppendEpochV8Signal(ctx context.Context, runID string, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	if transition == nil || transition.Kind() != pathv1.TransitionObserveWait {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("exact signal observation transition is required")
	}
	return s.mutateEpochV8Runtime(ctx, runID, epochRuntimeAppendAuthority{external: true}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		return epochv8.AdvanceHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition)
	})
}

func (s *FS) AppendEpochV8FinishExternal(ctx context.Context, runID string, transition *pathv1.ExecutionTransition, evidenceDigest string) (epochv8.RuntimeTransitionResult, error) {
	return s.mutateEpochV8Runtime(ctx, runID, epochRuntimeAppendAuthority{external: true}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		return epochv8.FinishClaimedHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition, evidenceDigest)
	})
}

// AppendEpochV8Settlement is explicit non-engine operator authority. It may
// race a live engine lease under the run flock, but maintenance excludes it.
func (s *FS) AppendEpochV8Settlement(ctx context.Context, runID string, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	return s.appendEpochV8Settlement(ctx, runID, epochv8.Binding{}, transition)
}

// AppendEpochV8SettlementAtBinding adds the outer checkpoint CAS required by
// daemon-carried opaque settlement tokens. The inner transition retains its
// own path-v1 prebinding.
func (s *FS) AppendEpochV8SettlementAtBinding(ctx context.Context, runID string, expected epochv8.Binding, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	if expected == (epochv8.Binding{}) {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("exact schema-8 settlement binding is required")
	}
	return s.appendEpochV8Settlement(ctx, runID, expected, transition)
}

func (s *FS) appendEpochV8Settlement(ctx context.Context, runID string, expected epochv8.Binding, transition *pathv1.ExecutionTransition) (epochv8.RuntimeTransitionResult, error) {
	if transition == nil || transition.Kind() != pathv1.TransitionAuditedSettlement {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("exact audited settlement transition is required")
	}
	return s.mutateEpochV8Runtime(ctx, runID, epochRuntimeAppendAuthority{external: true}, func(snapshot EpochV8RunSnapshot, source []byte) (epochv8.RuntimeTransitionResult, error) {
		if expected != (epochv8.Binding{}) && snapshot.Checkpoint.Binding() != expected {
			return epochv8.RuntimeTransitionResult{}, ErrWriterInProgress
		}
		return epochv8.AuditedSettlement(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, source, transition)
	})
}

func (s *FS) PublishEpochV8Retain(ctx context.Context, lease MaintenanceLease, plan *epochv8.ApplyPlan, candidateSource, reason []byte) (epochv8.RuntimeTransitionResult, error) {
	return s.publishEpochV8RuntimeApply(ctx, lease, plan, candidateSource, reason, false)
}

func (s *FS) PublishEpochV8Transfer(ctx context.Context, lease MaintenanceLease, plan *epochv8.ApplyPlan, candidateSource, reason []byte) (epochv8.RuntimeTransitionResult, error) {
	return s.publishEpochV8RuntimeApply(ctx, lease, plan, candidateSource, reason, true)
}

func (s *FS) publishEpochV8RuntimeApply(ctx context.Context, lease MaintenanceLease, plan *epochv8.ApplyPlan, candidateSource, reason []byte, transfer bool) (epochv8.RuntimeTransitionResult, error) {
	if err := validateMaintenanceLeaseInput(lease); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	defer unlock()
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, lease.RunID, s.newEpochV8Budget(ctx))
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if snapshot.Runtime == nil {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("schema-8 runtime is not attached")
	}
	ownerSource := snapshot.EpochSources[snapshot.Runtime.EpochID]
	var result epochv8.RuntimeTransitionResult
	if transfer {
		result, err = epochv8.ApplyTransferHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, ownerSource, candidateSource, plan)
	} else {
		result, err = epochv8.ApplyRetainHead(ctx, snapshot.Checkpoint, snapshot.RuntimeJSON, ownerSource, plan)
	}
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	candidate := plan.CandidateEpoch()
	classification, _, err := classifyEpochV8Source(candidateSource)
	if err != nil || classification.Candidate().TemplateRef() != candidate.TemplateRef || classification.Candidate().SourceDigest() != candidate.TemplateSourceDigest {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("%w: candidate source differs from typed runtime apply", ErrContentMismatch)
	}
	diff, reasonDigest, err := epochv8.EncodeAppliedEpochDiff(result.Checkpoint, candidate.ID)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if len(reason) > EpochV8MaxReasonBytes || (reasonDigest == "") != (reason == nil) || reasonDigest != "" && digestBytes(reason) != reasonDigest {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("%w: apply reason differs from typed runtime apply", ErrContentMismatch)
	}
	if result.Disposition == epochv8.DispositionReplayed {
		return result, s.verifyEpochV8ArtifactDirUnlocked(ctx, lease.RunID, candidate.ID, candidateSource, diff, reason, s.newEpochV8Budget(ctx))
	}
	nextJSON, err := epochv8.EncodeCheckpointV8(result.Checkpoint)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if err := s.installEpochV8ArtifactDirUnlocked(ctx, lease.RunID, candidate.ID, candidateSource, diff, reason); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if transfer {
		if err := s.installEpochV8RuntimeArtifactUnlocked(lease.RunID, result.Artifact.Digest, result.ArtifactJSON); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if s.epochV8PublishBeforeState != nil {
		if err := s.epochV8PublishBeforeState(); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	runDir, err := openViewDir(s.runDir(lease.RunID))
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	defer runDir.Close()
	current, err := readViewRegularAt(s.newEpochV8Budget(ctx), runDir, "state.json", false)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if !bytes.Equal(current, snapshot.CheckpointJSON) {
		return epochv8.RuntimeTransitionResult{}, ErrWriterInProgress
	}
	if err := writeFileAtomicAt(runDir, "state.json", nextJSON, 0o644); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if !skipDurabilitySyncs {
		if err := unix.Fsync(int(runDir.Fd())); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	if s.epochV8PublishAfterState != nil {
		if err := s.epochV8PublishAfterState(); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	return result, nil
}

func (s *FS) WithEpochV8ExecutionView(ctx context.Context, runID string, callback func(EpochV8ExecutionView) error) error {
	if callback == nil {
		return fmt.Errorf("schema-8 execution callback is required")
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, runID, s.newEpochV8Budget(ctx))
	if err != nil {
		return err
	}
	return callback(EpochV8ExecutionView{
		Run: snapshot.Run, CheckpointJSON: bytes.Clone(snapshot.CheckpointJSON), Checkpoint: snapshot.Checkpoint,
		EpochSources: cloneEpochSources(snapshot.EpochSources), RuntimeJSON: bytes.Clone(snapshot.RuntimeJSON), Runtime: snapshot.Runtime,
	})
}

func (s *FS) mutateEpochV8Runtime(ctx context.Context, runID string, authority epochRuntimeAppendAuthority, build func(EpochV8RunSnapshot, []byte) (epochv8.RuntimeTransitionResult, error)) (epochv8.RuntimeTransitionResult, error) {
	if err := ctx.Err(); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if safeSegment(runID) != nil || build == nil || (authority.engine == nil) == !authority.external {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("invalid schema-8 runtime append authority")
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	defer unlock()
	if err := s.requireEpochRuntimeAuthorityUnlocked(runID, authority); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, runID, s.newEpochV8Budget(ctx))
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	var source []byte
	if snapshot.Runtime == nil {
		source = snapshot.EpochSources[snapshot.Checkpoint.View().OriginalEpoch]
	} else {
		source = snapshot.EpochSources[snapshot.Runtime.EpochID]
	}
	if len(source) == 0 {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("%w: runtime owner source is absent", ErrRunInconsistent)
	}
	result, err := build(snapshot, source)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if result.Disposition == epochv8.DispositionReplayed {
		return result, nil
	}
	nextJSON, err := epochv8.EncodeCheckpointV8(result.Checkpoint)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if len(result.ArtifactJSON) == 0 || result.Artifact == nil || result.Artifact.Digest != result.Checkpoint.View().RuntimeBinding.Digest {
		return epochv8.RuntimeTransitionResult{}, fmt.Errorf("%w: runtime successor artifact is incomplete", ErrRunInconsistent)
	}
	if err := s.requireEpochRuntimeAuthorityUnlocked(runID, authority); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if err := s.installEpochV8RuntimeArtifactUnlocked(runID, result.Artifact.Digest, result.ArtifactJSON); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if err := s.requireEpochRuntimeAuthorityUnlocked(runID, authority); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if s.epochV8PublishBeforeState != nil {
		if err := s.epochV8PublishBeforeState(); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	runDir, err := openViewDir(s.runDir(runID))
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	defer runDir.Close()
	current, err := readViewRegularAt(s.newEpochV8Budget(ctx), runDir, "state.json", false)
	if err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if !bytes.Equal(current, snapshot.CheckpointJSON) {
		return epochv8.RuntimeTransitionResult{}, ErrWriterInProgress
	}
	if err := writeFileAtomicAt(runDir, "state.json", nextJSON, 0o644); err != nil {
		return epochv8.RuntimeTransitionResult{}, err
	}
	if !skipDurabilitySyncs {
		if err := unix.Fsync(int(runDir.Fd())); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	if s.epochV8PublishAfterState != nil {
		if err := s.epochV8PublishAfterState(); err != nil {
			return epochv8.RuntimeTransitionResult{}, err
		}
	}
	return result, nil
}

func (s *FS) requireEpochRuntimeAuthorityUnlocked(runID string, authority epochRuntimeAppendAuthority) error {
	if authority.engine != nil {
		if authority.engine.RunID != runID {
			return fmt.Errorf("engine lease run differs")
		}
		_, err := s.requireEngineLeaseUnlocked(*authority.engine)
		return err
	}
	current, err := s.readLease(runID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.ExpiresAt.After(s.now().UTC()) {
		return nil
	}
	switch current.normalizedKind() {
	case LeaseKindEngine:
		return nil
	case LeaseKindMaintenance:
		return fmt.Errorf("%w: maintenance lease excludes external append", ErrLeaseHeld)
	default:
		return fmt.Errorf("%w: unknown live lease kind excludes external append", ErrLeaseHeld)
	}
}

func (s *FS) installEpochV8RuntimeArtifactUnlocked(runID, digest string, data []byte) error {
	if !isHexSHA256(digest) {
		return fmt.Errorf("runtime artifact digest is invalid")
	}
	dir := filepath.Join(s.runDir(runID), "runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, digest+".json")
	if current, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(current, data) {
			return fmt.Errorf("%w: runtime artifact digest collision", ErrContentMismatch)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return err
	}
	return syncDir(dir)
}

func cloneEpochSources(source map[epochv8.EpochID][]byte) map[epochv8.EpochID][]byte {
	result := make(map[epochv8.EpochID][]byte, len(source))
	for epochID, data := range source {
		result[epochID] = bytes.Clone(data)
	}
	return result
}

func (s *FS) CollectEpochV8RuntimeGarbage(ctx context.Context, lease MaintenanceLease, cursor string) (EpochV8GCResult, error) {
	if err := validateMaintenanceLeaseInput(lease); err != nil {
		return EpochV8GCResult{}, err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return EpochV8GCResult{}, err
	}
	defer unlock()
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return EpochV8GCResult{}, err
	}
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, lease.RunID, s.newEpochV8Budget(ctx))
	if err != nil {
		return EpochV8GCResult{}, err
	}
	entries, err := os.ReadDir(filepath.Join(s.runDir(lease.RunID), "runtime"))
	if err != nil {
		return EpochV8GCResult{}, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && isHexSHA256(strings.TrimSuffix(entry.Name(), ".json")) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	start := 0
	if cursor != "" {
		start, _ = slices.BinarySearch(names, cursor)
		for start < len(names) && names[start] <= cursor {
			start++
		}
	}
	end := min(len(names), start+EpochV8GCMaxEntries)
	result := EpochV8GCResult{Scanned: end - start, Complete: end == len(names)}
	if !result.Complete && end > start {
		result.NextCursor = names[end-1]
	}
	protected := ""
	if snapshot.Runtime != nil {
		protected = snapshot.Runtime.Digest + ".json"
	}
	cutoff := s.now().UTC().Add(-EpochV8GCMinOrphanAge)
	for _, name := range names[start:end] {
		if name == protected {
			continue
		}
		path := filepath.Join(s.runDir(lease.RunID), "runtime", name)
		info, statErr := os.Stat(path)
		if statErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
			return result, err
		}
		if err := os.Remove(path); err != nil {
			return result, err
		}
		result.Removed++
	}
	if result.Removed > 0 {
		if err := syncDir(filepath.Join(s.runDir(lease.RunID), "runtime")); err != nil {
			return result, err
		}
	}
	return result, nil
}
