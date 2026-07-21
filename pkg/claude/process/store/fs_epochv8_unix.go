//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
)

func (s *FS) InitializeEpochV8Run(ctx context.Context, run RunRecord, source []byte) (EpochV8InitializationResult, error) {
	if err := ctx.Err(); err != nil {
		return EpochV8InitializationResult{}, err
	}
	if run.AllowPrograms || safeSegment(run.ID) != nil || strings.TrimSpace(run.TemplateRef) == "" {
		return EpochV8InitializationResult{}, fmt.Errorf("invalid schema-8 run record")
	}
	classification, parsed, err := classifyEpochV8Source(source)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	candidate := classification.Candidate()
	if candidate.TemplateRef() != run.TemplateRef {
		return EpochV8InitializationResult{}, fmt.Errorf("%w: source ref differs from run template ref", ErrContentMismatch)
	}
	unlockRun, err := s.lockRun(ctx, run.ID)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	defer unlockRun()
	templateID, templateHash, err := parseTemplateRef(run.TemplateRef)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	unlockTemplate, err := s.lockTemplate(ctx, templateID)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	defer unlockTemplate()
	if err := s.recoverAttributedTemplateSaveUnlocked(ctx, templateID); err != nil {
		return EpochV8InitializationResult{}, err
	}
	pinned, err := s.getTemplateUnlocked(ctx, templateID, templateHash, run.TemplateRef)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	templateBudget := s.newEpochV8Budget(ctx)
	templateBudget.maxFile = min(templateBudget.maxFile, int64(EpochV8MaxSourceBytes))
	storedSource, err := s.getTemplateExactSourceWithBudget(ctx, templateID, templateHash, templateBudget, pinned)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	if !bytes.Equal(storedSource, source) || !templateMatchesRef(pinned, run.TemplateRef) || !templateMatchesRef(parsed.Template, run.TemplateRef) {
		return EpochV8InitializationResult{}, fmt.Errorf("%w: exact source differs from stored template", ErrContentMismatch)
	}
	run.Template = pinned
	checkpoint, err := epochv8.Initialize(run.ID, candidate, []epochv8.AuthoritySeed{{
		LocalID: EpochV8InitialFrontierLocalID, ReservationID: EpochV8InitialFrontierReservationID,
		NodeID: parsed.Template.Start, Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	checkpointJSON, err := epochv8.EncodeCheckpointV8(checkpoint)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}

	if _, statErr := os.Lstat(s.runDir(run.ID)); statErr == nil {
		budget := s.newEpochV8Budget(ctx)
		snapshot, readErr := s.loadEpochV8RunViewUnlocked(ctx, run.ID, budget)
		if readErr != nil {
			return EpochV8InitializationResult{}, fmt.Errorf("%w: existing run is not an exact schema-8 initialization: %v", ErrRunExists, readErr)
		}
		view := snapshot.Checkpoint.View()
		if snapshot.Run.ID != run.ID || snapshot.Run.TemplateRef != run.TemplateRef ||
			!maps.Equal(snapshot.Run.Params, run.Params) || snapshot.Run.AllowPrograms != run.AllowPrograms ||
			!templateMatchesRef(snapshot.Run.Template, run.TemplateRef) || len(view.Epochs) != 1 ||
			!bytes.Equal(snapshot.CheckpointJSON, checkpointJSON) || !bytes.Equal(snapshot.EpochSources[view.OriginalEpoch], source) {
			return EpochV8InitializationResult{}, fmt.Errorf("%w: existing schema-8 initialization differs", ErrRunExists)
		}
		if err := syncDir(s.runDir(run.ID)); err != nil {
			return EpochV8InitializationResult{}, err
		}
		if err := s.syncEpochV8InitializationParent(filepath.Join(s.root, "runs")); err != nil {
			return EpochV8InitializationResult{}, err
		}
		return EpochV8InitializationResult{Disposition: EpochV8InitializationAlreadyApplied, Run: snapshot.Run, Checkpoint: snapshot.Checkpoint}, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return EpochV8InitializationResult{}, statErr
	}

	if run.CreatedAt.IsZero() {
		run.CreatedAt = s.now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	runJSON, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	runJSON = append(runJSON, '\n')
	runsDir := filepath.Join(s.root, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return EpochV8InitializationResult{}, err
	}
	staging, err := epochV8StagingPath(runsDir, run.ID)
	if err != nil {
		return EpochV8InitializationResult{}, err
	}
	if err := os.Mkdir(staging, 0o755); err != nil {
		return EpochV8InitializationResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	epochID := string(checkpoint.View().OriginalEpoch)
	epochDir := filepath.Join(staging, "epochs", epochID)
	for _, dir := range []string{filepath.Join(staging, "nodes"), filepath.Join(staging, "artifacts"), filepath.Join(staging, "runtime"), epochDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return EpochV8InitializationResult{}, err
		}
	}
	for path, data := range map[string][]byte{
		filepath.Join(staging, "run.json"):              runJSON,
		filepath.Join(staging, "state.json"):            checkpointJSON,
		filepath.Join(staging, "lease-generation.json"): []byte("{\"generation\":0}\n"),
		filepath.Join(epochDir, "source.yaml"):          source,
	} {
		if err := writeFileAtomic(path, data, 0o644); err != nil {
			return EpochV8InitializationResult{}, err
		}
	}
	for _, dir := range []string{epochDir, filepath.Join(staging, "epochs"), filepath.Join(staging, "nodes"), filepath.Join(staging, "artifacts"), filepath.Join(staging, "runtime"), staging} {
		if err := syncDir(dir); err != nil {
			return EpochV8InitializationResult{}, err
		}
	}
	if s.epochV8InitializeBeforeCommit != nil {
		if err := s.epochV8InitializeBeforeCommit(); err != nil {
			return EpochV8InitializationResult{}, err
		}
	}
	if err := os.Rename(staging, s.runDir(run.ID)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return EpochV8InitializationResult{}, fmt.Errorf("%w: %q", ErrRunExists, run.ID)
		}
		return EpochV8InitializationResult{}, fmt.Errorf("publish schema-8 run: %w", err)
	}
	committed = true
	if err := s.syncEpochV8InitializationParent(runsDir); err != nil {
		return EpochV8InitializationResult{}, err
	}
	if s.epochV8InitializeAfterCommit != nil {
		if err := s.epochV8InitializeAfterCommit(); err != nil {
			return EpochV8InitializationResult{}, err
		}
	}
	return EpochV8InitializationResult{Disposition: EpochV8InitializationApplied, Run: run, Checkpoint: checkpoint}, nil
}

func (s *FS) syncEpochV8InitializationParent(runsDir string) error {
	if s.epochV8InitializeDirSync != nil {
		if err := s.epochV8InitializeDirSync(); err != nil {
			return fmt.Errorf("sync schema-8 initialization parent: %w", err)
		}
	}
	return syncDir(runsDir)
}

func classifyEpochV8Source(source []byte) (epochv8.TemplateClassification, *model.ParsedTemplate, error) {
	if len(source) == 0 || len(source) > EpochV8MaxSourceBytes {
		return epochv8.TemplateClassification{}, nil, &ExecutionViewOverBudgetError{Limit: "source_bytes", Component: "source.yaml", Value: int64(len(source)), Maximum: EpochV8MaxSourceBytes}
	}
	classification, err := epochv8.ClassifyTemplateSource(source)
	if err != nil {
		return epochv8.TemplateClassification{}, nil, err
	}
	if classification.Candidate() == nil {
		return classification, nil, fmt.Errorf("schema-8 template is ineligible: %s", classification.Reason)
	}
	parsed, err := model.ParseExactSource(source)
	if err != nil || parsed == nil || parsed.Template == nil || parsed.Diagnostics.HasErrors() {
		return classification, nil, fmt.Errorf("schema-8 exact source is invalid")
	}
	return classification, parsed, nil
}

func epochV8StagingPath(parent, prefix string) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return filepath.Join(parent, fmt.Sprintf(".epochv8-%s-%x", prefix, nonce[:])), nil
}

func (s *FS) newEpochV8Budget(ctx context.Context) *viewBudget {
	budget := s.newExecutionViewBudget(ctx)
	budget.maxFile = min(budget.maxFile, int64(epochv8.MaxCheckpointBytes))
	budget.maxTotal = min(budget.maxTotal, int64(EpochV8MaxTotalReadBytes))
	return budget
}

func (s *FS) LoadEpochV8RunView(ctx context.Context, runID string) (EpochV8RunSnapshot, error) {
	if err := safeSegment(runID); err != nil {
		return EpochV8RunSnapshot{}, err
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return EpochV8RunSnapshot{}, err
	}
	defer unlock()
	return s.loadEpochV8RunViewUnlocked(ctx, runID, s.newEpochV8Budget(ctx))
}

// ReadEpochV8AppliedArtifacts returns only exact bytes reverified against the
// coherent checkpoint while holding the run lock. The second secure open is
// not trusted merely because the flock is held: its bytes and reason presence
// are checked again before anything is returned.
func (s *FS) ReadEpochV8AppliedArtifacts(ctx context.Context, runID string, epochID epochv8.EpochID) (EpochV8AppliedArtifacts, error) {
	if err := safeSegment(runID); err != nil || !isHexSHA256(string(epochID)) {
		return EpochV8AppliedArtifacts{}, ErrNotFound
	}
	unlock, err := s.lockRun(ctx, runID)
	if err != nil {
		return EpochV8AppliedArtifacts{}, err
	}
	defer unlock()
	budget := s.newEpochV8Budget(ctx)
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, runID, budget)
	if err != nil {
		return EpochV8AppliedArtifacts{}, err
	}
	view := snapshot.Checkpoint.View()
	found := false
	for index, epoch := range view.Epochs {
		if epoch.ID != epochID {
			continue
		}
		if found || index == 0 || epoch.Ordinal == 0 {
			return EpochV8AppliedArtifacts{}, ErrNotFound
		}
		found = true
	}
	if !found {
		return EpochV8AppliedArtifacts{}, ErrNotFound
	}
	wantDiff, reasonDigest, err := epochv8.EncodeAppliedEpochDiff(snapshot.Checkpoint, epochID)
	if err != nil {
		return EpochV8AppliedArtifacts{}, &ExecutionViewInconsistentError{Err: err}
	}
	runDir, err := openViewDir(s.runDir(runID))
	if err != nil {
		return EpochV8AppliedArtifacts{}, err
	}
	defer runDir.Close()
	epochsDir, err := openViewDirAt(runDir, "epochs")
	if err != nil {
		return EpochV8AppliedArtifacts{}, err
	}
	defer epochsDir.Close()
	epochDir, err := openViewDirAt(epochsDir, string(epochID))
	if err != nil {
		return EpochV8AppliedArtifacts{}, err
	}
	defer epochDir.Close()
	diff, err := readEpochV8RegularAt(budget, epochDir, "diff.json", false, epochv8.MaxApplyPlanBytes)
	if err != nil || !bytes.Equal(diff, wantDiff) {
		return EpochV8AppliedArtifacts{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("applied diff failed exact revalidation")}
	}
	hasReason, err := hasViewRegularAt(epochDir, "reason.txt")
	if err != nil || hasReason != (reasonDigest != "") {
		return EpochV8AppliedArtifacts{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("applied reason presence failed revalidation")}
	}
	result := EpochV8AppliedArtifacts{Diff: bytes.Clone(diff), HasReason: hasReason}
	if hasReason {
		reason, readErr := readEpochV8RegularAt(budget, epochDir, "reason.txt", false, EpochV8MaxReasonBytes)
		if readErr != nil || digestBytes(reason) != reasonDigest {
			return EpochV8AppliedArtifacts{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("applied reason failed exact revalidation")}
		}
		result.Reason = bytes.Clone(reason)
	}
	return result, nil
}

func (s *FS) loadEpochV8RunViewUnlocked(ctx context.Context, runID string, budget *viewBudget) (EpochV8RunSnapshot, error) {
	run, err := s.readExecutionRunRecordAt(ctx, runID, budget)
	if err != nil {
		return EpochV8RunSnapshot{}, err
	}
	runDir, err := openViewDir(s.runDir(runID))
	if err != nil {
		return EpochV8RunSnapshot{}, err
	}
	defer runDir.Close()
	checkpointJSON, err := readViewRegularAt(budget, runDir, "state.json", false)
	if err != nil {
		return EpochV8RunSnapshot{}, err
	}
	checkpoint, err := epochv8.DecodeCheckpointV8(checkpointJSON)
	if err != nil {
		return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: err}
	}
	view := checkpoint.View()
	if view.RunID != run.ID || len(view.Epochs) == 0 || view.Epochs[0].TemplateRef != run.TemplateRef {
		return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("checkpoint differs from run anchor")}
	}
	epochsDir, err := openViewDirAt(runDir, "epochs")
	if err != nil {
		return EpochV8RunSnapshot{}, err
	}
	defer epochsDir.Close()
	sources := make(map[epochv8.EpochID][]byte, len(view.Epochs))
	for i, epoch := range view.Epochs {
		if !isHexSHA256(string(epoch.ID)) {
			return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch id is not canonical")}
		}
		epochDir, err := openViewDirAt(epochsDir, string(epoch.ID))
		if err != nil {
			return EpochV8RunSnapshot{}, err
		}
		source, readErr := readEpochV8RegularAt(budget, epochDir, "source.yaml", false, EpochV8MaxSourceBytes)
		if readErr != nil {
			epochDir.Close()
			return EpochV8RunSnapshot{}, readErr
		}
		classification, parsed, classifyErr := classifyEpochV8Source(source)
		if classifyErr != nil {
			epochDir.Close()
			return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: classifyErr}
		}
		candidate := classification.Candidate()
		if candidate.TemplateRef() != epoch.TemplateRef || candidate.SourceDigest() != epoch.TemplateSourceDigest || parsed.SourceHash != epoch.TemplateSourceDigest {
			epochDir.Close()
			return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch source differs from checkpoint metadata")}
		}
		if i == 0 && !templateMatchesRef(run.Template, epoch.TemplateRef) {
			epochDir.Close()
			return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch zero differs from pinned template")}
		}
		if i > 0 {
			wantDiff, reasonDigest, encodeErr := epochv8.EncodeAppliedEpochDiff(checkpoint, epoch.ID)
			if encodeErr != nil {
				epochDir.Close()
				return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: encodeErr}
			}
			diff, diffErr := readViewRegularAt(budget, epochDir, "diff.json", false)
			if diffErr != nil || !bytes.Equal(diff, wantDiff) {
				epochDir.Close()
				return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch diff differs from checkpoint: %v", diffErr)}
			}
			hasReason, reasonStatErr := hasViewRegularAt(epochDir, "reason.txt")
			if reasonStatErr != nil || hasReason != (reasonDigest != "") {
				epochDir.Close()
				return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch reason presence differs from checkpoint")}
			}
			if hasReason {
				reason, reasonErr := readEpochV8RegularAt(budget, epochDir, "reason.txt", false, EpochV8MaxReasonBytes)
				if reasonErr != nil || len(reason) > EpochV8MaxReasonBytes || digestBytes(reason) != reasonDigest {
					epochDir.Close()
					return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("epoch reason differs from checkpoint")}
				}
			}
		}
		epochDir.Close()
		sources[epoch.ID] = bytes.Clone(source)
	}
	result := EpochV8RunSnapshot{Run: run, CheckpointJSON: bytes.Clone(checkpointJSON), Checkpoint: checkpoint, EpochSources: sources}
	if view.RuntimeBinding != (epochv8.RuntimeBinding{}) {
		runtimeDir, openErr := openViewDirAt(runDir, "runtime")
		if openErr != nil {
			return EpochV8RunSnapshot{}, openErr
		}
		defer runtimeDir.Close()
		name := view.RuntimeBinding.Digest + ".json"
		runtimeJSON, readErr := readEpochV8RegularAt(budget, runtimeDir, name, false, epochv8.MaxRuntimeArtifactBytes)
		if readErr != nil {
			return EpochV8RunSnapshot{}, readErr
		}
		ownerEpoch := epochv8.EpochID("")
		for i := len(view.History) - 1; i >= 0; i-- {
			receipt := view.History[i].Runtime
			if receipt != nil && receipt.PostRuntime == view.RuntimeBinding {
				ownerEpoch = receipt.EpochID
				break
			}
		}
		source, sourceOK := sources[ownerEpoch]
		artifact, verifyErr := epochv8.VerifyRuntimeArtifact(ctx, checkpoint, runtimeJSON, source)
		if !sourceOK || verifyErr != nil || artifact.EpochID != ownerEpoch {
			return EpochV8RunSnapshot{}, &ExecutionViewInconsistentError{Err: fmt.Errorf("runtime artifact has no exact owner source")}
		}
		result.RuntimeJSON, result.Runtime = bytes.Clone(runtimeJSON), artifact
	}
	return result, nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func readEpochV8RegularAt(budget *viewBudget, dir *os.File, name string, missingEmpty bool, maximum int) ([]byte, error) {
	oldMax := budget.maxFile
	budget.maxFile = min(oldMax, int64(maximum))
	defer func() { budget.maxFile = oldMax }()
	return readViewRegularAt(budget, dir, name, missingEmpty)
}

func templateMatchesRef(tmpl *model.Template, ref string) bool {
	if tmpl == nil {
		return false
	}
	digest, err := model.SemanticHash(tmpl)
	if err != nil {
		return false
	}
	return model.TemplateRef(tmpl.ID, digest) == ref
}

func (s *FS) PublishEpochV8(ctx context.Context, lease MaintenanceLease, plan *epochv8.ApplyPlan, source, reason []byte) (EpochV8PublicationResult, error) {
	if err := validateMaintenanceLeaseInput(lease); err != nil {
		return EpochV8PublicationResult{}, err
	}
	unlock, err := s.lockRun(ctx, lease.RunID)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	defer unlock()
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return EpochV8PublicationResult{}, err
	}
	budget := s.newEpochV8Budget(ctx)
	snapshot, err := s.loadEpochV8RunViewUnlocked(ctx, lease.RunID, budget)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	transition, err := epochv8.Apply(snapshot.Checkpoint, plan)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	result := EpochV8PublicationResult{Disposition: transition.Disposition, Binding: transition.Binding, Checkpoint: transition.Checkpoint}
	if transition.Disposition == epochv8.DispositionStale {
		return result, nil
	}
	candidate := plan.CandidateEpoch()
	diff, reasonDigest, err := epochv8.EncodeAppliedEpochDiff(transition.Checkpoint, candidate.ID)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	if len(reason) > EpochV8MaxReasonBytes {
		return EpochV8PublicationResult{}, &ExecutionViewOverBudgetError{Limit: "reason_bytes", Component: "reason.txt", Value: int64(len(reason)), Maximum: EpochV8MaxReasonBytes}
	}
	classification, _, err := classifyEpochV8Source(source)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	classifiedCandidate := classification.Candidate()
	if candidate.TemplateRef != classifiedCandidate.TemplateRef() || candidate.TemplateSourceDigest != classifiedCandidate.SourceDigest() {
		return EpochV8PublicationResult{}, fmt.Errorf("%w: candidate source differs from applied epoch", ErrContentMismatch)
	}
	if (reasonDigest == "") != (reason == nil) || reasonDigest != "" && digestBytes(reason) != reasonDigest {
		return EpochV8PublicationResult{}, fmt.Errorf("%w: reason content does not match applied digest", ErrContentMismatch)
	}
	if transition.Disposition == epochv8.DispositionReplayed {
		return result, s.verifyEpochV8ArtifactDirUnlocked(ctx, lease.RunID, candidate.ID, source, diff, reason, s.newEpochV8Budget(ctx))
	}
	nextJSON, err := epochv8.EncodeCheckpointV8(transition.Checkpoint)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	prospectiveBytes := budget.bytes - int64(len(snapshot.CheckpointJSON)) + int64(len(nextJSON)) +
		int64(len(source)) + int64(len(diff)) + int64(len(reason))
	if prospectiveBytes > budget.maxTotal {
		return EpochV8PublicationResult{}, &ExecutionViewOverBudgetError{
			Limit: "total_bytes", Component: "published schema-8 coherent view",
			Value: prospectiveBytes, Maximum: budget.maxTotal,
		}
	}
	if s.epochV8PublishBeforeEpoch != nil {
		if err := s.epochV8PublishBeforeEpoch(); err != nil {
			return EpochV8PublicationResult{}, err
		}
	}
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return EpochV8PublicationResult{}, err
	}
	if err := s.installEpochV8ArtifactDirUnlocked(ctx, lease.RunID, candidate.ID, source, diff, reason); err != nil {
		return EpochV8PublicationResult{}, err
	}
	if s.epochV8PublishAfterEpoch != nil {
		if err := s.epochV8PublishAfterEpoch(); err != nil {
			return EpochV8PublicationResult{}, err
		}
	}
	if s.epochV8PublishBeforeState != nil {
		if err := s.epochV8PublishBeforeState(); err != nil {
			return EpochV8PublicationResult{}, err
		}
	}
	if _, err := s.requireMaintenanceLeaseUnlocked(lease); err != nil {
		return EpochV8PublicationResult{}, err
	}
	runDir, err := openViewDir(s.runDir(lease.RunID))
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	defer runDir.Close()
	current, err := readViewRegularAt(s.newEpochV8Budget(ctx), runDir, "state.json", false)
	if err != nil {
		return EpochV8PublicationResult{}, err
	}
	if !bytes.Equal(current, snapshot.CheckpointJSON) {
		return EpochV8PublicationResult{}, ErrWriterInProgress
	}
	if err := writeFileAtomicAt(runDir, "state.json", nextJSON, 0o644); err != nil {
		return EpochV8PublicationResult{}, err
	}
	if !skipDurabilitySyncs {
		if err := unix.Fsync(int(runDir.Fd())); err != nil {
			return EpochV8PublicationResult{}, err
		}
	}
	if s.epochV8PublishAfterState != nil {
		if err := s.epochV8PublishAfterState(); err != nil {
			return EpochV8PublicationResult{}, err
		}
	}
	return result, nil
}

func (s *FS) installEpochV8ArtifactDirUnlocked(ctx context.Context, runID string, epochID epochv8.EpochID, source, diff, reason []byte) error {
	final := filepath.Join(s.runDir(runID), "epochs", string(epochID))
	if _, err := os.Lstat(final); err == nil {
		return s.verifyEpochV8ArtifactDirUnlocked(ctx, runID, epochID, source, diff, reason, s.newEpochV8Budget(ctx))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(final)
	staging, err := epochV8StagingPath(parent, string(epochID))
	if err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0o755); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	files := []struct {
		name string
		data []byte
		perm os.FileMode
	}{{"source.yaml", source, 0o644}, {"diff.json", diff, 0o644}}
	if reason != nil {
		files = append(files, struct {
			name string
			data []byte
			perm os.FileMode
		}{"reason.txt", reason, 0o600})
	}
	for _, file := range files {
		if err := writeFileAtomic(filepath.Join(staging, file.name), file.data, file.perm); err != nil {
			return err
		}
	}
	if err := syncDir(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	committed = true
	return syncDir(parent)
}

func (s *FS) verifyEpochV8ArtifactDirUnlocked(ctx context.Context, runID string, epochID epochv8.EpochID, source, diff, reason []byte, budget *viewBudget) error {
	dir, err := openViewDir(filepath.Join(s.runDir(runID), "epochs", string(epochID)))
	if err != nil {
		return err
	}
	defer dir.Close()
	for name, want := range map[string][]byte{"source.yaml": source, "diff.json": diff} {
		maximum := epochv8.MaxApplyPlanBytes
		if name == "source.yaml" {
			maximum = EpochV8MaxSourceBytes
		}
		got, err := readEpochV8RegularAt(budget, dir, name, false, maximum)
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("%w: epoch artifact %s differs", ErrContentMismatch, name)
		}
	}
	hasReason, err := hasViewRegularAt(dir, "reason.txt")
	if err != nil || hasReason != (reason != nil) {
		return fmt.Errorf("%w: epoch reason presence differs", ErrContentMismatch)
	}
	if hasReason {
		got, err := readEpochV8RegularAt(budget, dir, "reason.txt", false, EpochV8MaxReasonBytes)
		if err != nil || !bytes.Equal(got, reason) {
			return fmt.Errorf("%w: epoch reason differs", ErrContentMismatch)
		}
	}
	return nil
}

func (s *FS) CollectEpochV8Garbage(ctx context.Context, lease MaintenanceLease, cursor string) (EpochV8GCResult, error) {
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
	referenced := make(map[string]struct{})
	for _, epoch := range snapshot.Checkpoint.View().Epochs {
		referenced[string(epoch.ID)] = struct{}{}
	}
	dirPath := filepath.Join(s.runDir(lease.RunID), "epochs")
	return collectEpochV8GarbageBatch(ctx, dirPath, referenced, s.now().UTC(), cursor, func() error {
		_, err := s.requireMaintenanceLeaseUnlocked(lease)
		return err
	})
}

func collectEpochV8GarbageBatch(
	ctx context.Context,
	dirPath string,
	referenced map[string]struct{},
	now time.Time,
	cursor string,
	requireLease func() error,
) (EpochV8GCResult, error) {
	if requireLease == nil {
		return EpochV8GCResult{}, fmt.Errorf("schema-8 GC lease authority is required")
	}
	dir, err := openViewDir(dirPath)
	if err != nil {
		return EpochV8GCResult{}, err
	}
	defer dir.Close()
	names, nextCursor, complete, err := readEpochV8GCBatch(ctx, dir, cursor, EpochV8GCMaxEntries)
	if err != nil {
		return EpochV8GCResult{}, err
	}
	result := EpochV8GCResult{Scanned: len(names), NextCursor: nextCursor, Complete: complete}
	cutoff := now.UTC().Add(-EpochV8GCMinOrphanAge)
	for _, name := range names {
		safeName := name
		if strings.HasPrefix(name, ".epochv8-") {
			safeName = strings.TrimPrefix(name, ".")
		}
		if safeSegment(safeName) != nil {
			continue
		}
		if _, ok := referenced[name]; ok {
			continue
		}
		if !strings.HasPrefix(name, ".epochv8-") && !isHexSHA256(name) {
			continue
		}
		entry, openErr := openViewDirAt(dir, name)
		if openErr != nil {
			continue
		}
		info, err := entry.Stat()
		entry.Close()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := requireLease(); err != nil {
			return result, err
		}
		if err := os.RemoveAll(filepath.Join(dirPath, name)); err != nil {
			return result, err
		}
		result.Removed++
	}
	if result.Removed > 0 {
		if err := syncDir(dirPath); err != nil {
			return result, err
		}
	}
	return result, nil
}

const (
	epochV8GCDirentBufferBytes = 4 << 10
	epochV8GCCursorMaxBytes    = 16 << 10
)

type epochV8GCCursor struct {
	Version int    `json:"version"`
	Offset  int64  `json:"offset"`
	Pending []byte `json:"pending,omitempty"`
}

func readEpochV8GCBatch(ctx context.Context, dir *os.File, encoded string, maximum int) ([]string, string, bool, error) {
	if dir == nil || maximum <= 0 {
		return nil, "", false, fmt.Errorf("invalid schema-8 GC traversal")
	}
	cursor, err := decodeEpochV8GCCursor(encoded)
	if err != nil {
		return nil, "", false, err
	}
	if cursor.Offset > 0 {
		if _, err := unix.Seek(int(dir.Fd()), cursor.Offset, 0); err != nil {
			return nil, "", false, fmt.Errorf("seek schema-8 GC cursor: %w", err)
		}
	}
	names := make([]string, 0, maximum)
	pending := cursor.Pending
	for len(names) < maximum {
		if err := ctx.Err(); err != nil {
			return nil, "", false, err
		}
		if len(pending) == 0 {
			buffer := make([]byte, epochV8GCDirentBufferBytes)
			n, readErr := unix.ReadDirent(int(dir.Fd()), buffer)
			if readErr != nil {
				return nil, "", false, fmt.Errorf("read schema-8 GC directory: %w", readErr)
			}
			if n == 0 {
				return names, "", true, nil
			}
			pending = buffer[:n]
			cursor.Offset, err = unix.Seek(int(dir.Fd()), 0, 1)
			if err != nil {
				return nil, "", false, fmt.Errorf("record schema-8 GC cursor: %w", err)
			}
		}
		consumed, _, parsed := unix.ParseDirent(pending, maximum-len(names), names)
		if consumed <= 0 {
			return nil, "", false, fmt.Errorf("decode schema-8 GC directory entry")
		}
		names = parsed
		pending = pending[consumed:]
	}
	cursor.Pending = bytes.Clone(pending)
	next, err := encodeEpochV8GCCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	return names, next, false, nil
}

func encodeEpochV8GCCursor(cursor epochV8GCCursor) (string, error) {
	cursor.Version = 1
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	if len(data) > epochV8GCCursorMaxBytes {
		return "", fmt.Errorf("schema-8 GC cursor is over budget")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeEpochV8GCCursor(encoded string) (epochV8GCCursor, error) {
	if encoded == "" {
		return epochV8GCCursor{Version: 1}, nil
	}
	if len(encoded) > base64.RawURLEncoding.EncodedLen(epochV8GCCursorMaxBytes) {
		return epochV8GCCursor{}, fmt.Errorf("schema-8 GC cursor is over budget")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return epochV8GCCursor{}, fmt.Errorf("decode schema-8 GC cursor: %w", err)
	}
	var cursor epochV8GCCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != 1 || cursor.Offset < 0 || len(cursor.Pending) > epochV8GCDirentBufferBytes {
		return epochV8GCCursor{}, fmt.Errorf("schema-8 GC cursor is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return epochV8GCCursor{}, fmt.Errorf("schema-8 GC cursor is invalid")
	}
	return cursor, nil
}
