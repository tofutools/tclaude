//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// RunStateSchemaVersion reads only the bounded checkpoint header under the
// run lock. It is the schema switch used by the live scheduler and API; the
// legacy decoder cap remains fixed at schema 6.
func (s *FS) RunStateSchemaVersion(ctx context.Context, runID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := safeSegment(runID); err != nil {
		return 0, fmt.Errorf("invalid run id: %w", err)
	}
	exists, err := s.HasRunView(runID)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotFound
	}
	unlock, err := s.lockRunView(ctx, runID)
	if err != nil {
		return 0, err
	}
	defer unlock()
	runDir, err := s.openPathV1InitializationRunDir(runID)
	if err != nil {
		return 0, err
	}
	defer runDir.Close()
	data, err := readPathV1InitializationStateAt(ctx, runDir, s.newExecutionViewBudget(ctx))
	if err != nil {
		return 0, err
	}
	var header struct {
		StateSchemaVersion int `json:"stateSchemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, &DecodeError{Component: "run state header", Err: err}
	}
	if header.StateSchemaVersion <= 0 {
		return 0, &DecodeError{Component: "run state header", Err: fmt.Errorf("invalid state schema version %d", header.StateSchemaVersion)}
	}
	return header.StateSchemaVersion, nil
}

// LoadPathV1RunView returns a fully verified, detached schema-7 checkpoint and
// exact template source. Evidence is intentionally not an input.
func (s *FS) LoadPathV1RunView(ctx context.Context, runID string) (PathV1RunSnapshot, error) {
	return s.loadPathV1RunView(ctx, runID, false)
}

// LoadPathV1RunHistoryView adds bounded legacy evidence only for checkpoints
// carrying migration projection metadata. Native schema-7 runs never touch
// the legacy evidence tree.
func (s *FS) LoadPathV1RunHistoryView(ctx context.Context, runID string) (PathV1RunSnapshot, error) {
	return s.loadPathV1RunView(ctx, runID, true)
}

func (s *FS) loadPathV1RunView(ctx context.Context, runID string, includeLegacyEvidence bool) (PathV1RunSnapshot, error) {
	var snapshot PathV1RunSnapshot
	budget := s.newExecutionViewBudget(ctx)
	err := s.withPathV1ExecutionViewBudget(ctx, runID, budget, func(view PathV1ExecutionView) error {
		encoded, err := pathv1.EncodeCheckpointV7(view.Checkpoint)
		if err != nil {
			return err
		}
		checkpoint, err := pathv1.DecodeCheckpointV7(encoded)
		if err != nil {
			return err
		}
		snapshot = PathV1RunSnapshot{
			Run: view.Run, CheckpointJSON: bytes.Clone(view.CheckpointJSON),
			TemplateSource: bytes.Clone(view.TemplateSource), Checkpoint: checkpoint,
		}
		if !includeLegacyEvidence || checkpoint.Execution == nil || checkpoint.Execution.LegacyProjection == nil {
			return nil
		}
		runDir, err := s.openPathV1InitializationRunDir(runID)
		if err != nil {
			return err
		}
		defer runDir.Close()
		manifest, logs, err := readPathV1LegacyEvidenceAt(ctx, runDir, budget)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			snapshot.LegacyEvidenceFailure = classifyPathV1LegacyEvidenceFailure(err)
			return nil
		}
		if err := s.requirePathV1RunDirCurrent(runID, runDir); err != nil {
			return err
		}
		snapshot.LegacyEvidence = &PathV1LegacyEvidence{Manifest: manifest, NodeLogs: logs}
		return nil
	})
	return snapshot, err
}

func classifyPathV1LegacyEvidenceFailure(err error) PathV1LegacyEvidenceFailure {
	if errors.Is(err, ErrExecutionViewOverBudget) {
		return PathV1LegacyEvidenceResourceLimit
	}
	var readErr *evidence.ReadError
	if errors.As(err, &readErr) {
		return PathV1LegacyEvidenceInvalid
	}
	return PathV1LegacyEvidenceUnavailable
}

// WithPathV1ExecutionView exposes the coherent schema-7 read
// boundary. The callback is pure: external adapters must never be invoked
// while its run/template locks are held.
func (s *FS) WithPathV1ExecutionView(ctx context.Context, runID string, callback func(PathV1ExecutionView) error) error {
	return s.withPathV1ExecutionViewBudget(ctx, runID, s.newExecutionViewBudget(ctx), callback)
}

func (s *FS) withPathV1ExecutionViewBudget(ctx context.Context, runID string, budget *viewBudget, callback func(PathV1ExecutionView) error) error {
	if callback == nil {
		return fmt.Errorf("path-v1 execution view callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := safeSegment(runID); err != nil {
		return fmt.Errorf("invalid run id: %w", err)
	}
	exists, err := s.HasRunView(runID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	unlockRun, err := s.lockRun(ctx, runID)
	if err != nil {
		return err
	}
	defer unlockRun()
	if s.executionRunLockedHook != nil {
		s.executionRunLockedHook()
	}
	return s.withPathV1ExecutionViewRunLocked(ctx, runID, budget, callback)
}

func (s *FS) withPathV1ExecutionViewRunLocked(ctx context.Context, runID string, budget *viewBudget, callback func(PathV1ExecutionView) error) error {
	run, err := s.readExecutionRunRecordAt(ctx, runID, budget)
	if err != nil {
		return pathV1ExecutionReadError("run record", err)
	}
	id, templateHash, err := parseTemplateRef(run.TemplateRef)
	if err != nil || safeSegment(id) != nil {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("run has invalid exact template ref")}
	}
	templateExists, err := s.hasTemplateExactView(id, templateHash)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return ErrUnsafeRunPath
		}
		return err
	}
	if !templateExists {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template %q is unavailable", run.TemplateRef)}
	}
	unlockTemplate, err := s.lockTemplateView(ctx, id)
	if err != nil {
		return err
	}
	defer unlockTemplate()
	if s.executionTemplateLockedHook != nil {
		s.executionTemplateLockedHook()
	}

	runDir, err := s.openPathV1InitializationRunDir(runID)
	if err != nil {
		return err
	}
	defer runDir.Close()
	checkpointJSON, err := readPathV1InitializationStateAt(ctx, runDir, budget)
	if err != nil {
		return pathV1ExecutionReadError("state.json", err)
	}
	checkpoint, err := pathv1.DecodeCheckpointV7(checkpointJSON)
	if err != nil {
		var over *pathv1.OverBudgetError
		if errors.As(err, &over) {
			return &ExecutionViewOverBudgetError{Limit: over.Limit, Component: "state.json", Value: int64(over.Value), Maximum: int64(over.Maximum)}
		}
		return &ExecutionViewInconsistentError{Err: err}
	}
	if checkpoint.Initialize.UpgradeNeeded.RunID != run.ID || checkpoint.Initialize.UpgradeNeeded.TemplateRef != run.TemplateRef {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("checkpoint differs from exact run record")}
	}

	templateBody, err := s.getTemplateExactBodyWithBudget(ctx, id, templateHash, budget)
	if err != nil {
		return pathV1ExecutionReadError("exact template", err)
	}
	var template model.Template
	if err := decodeViewJSON(ctx, templateBody, &template, true); err != nil {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template cannot be decoded: %w", err)}
	}
	semanticHash, err := model.SemanticHash(&template)
	if err != nil || template.ID != id || semanticHash != templateHash {
		return &ExecutionViewInconsistentError{Err: ErrContentMismatch}
	}
	if run.Template != nil {
		embeddedHash, hashErr := model.SemanticHash(run.Template)
		if hashErr != nil || run.Template.ID != id || embeddedHash != templateHash {
			return &ExecutionViewInconsistentError{Err: fmt.Errorf("embedded run template does not match exact template ref")}
		}
	}
	templateSource, err := s.getTemplateExactSourceWithBudget(ctx, id, templateHash, budget, &template)
	if err != nil {
		return pathV1ExecutionReadError("exact template source", err)
	}
	verified, err := pathv1.VerifyExecutionInput(ctx, checkpointJSON, templateSource)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &ExecutionViewInconsistentError{Err: err}
	}
	if err := s.requirePathV1RunDirCurrent(runID, runDir); err != nil {
		return err
	}
	return callback(PathV1ExecutionView{
		Run: run, Template: &template, TemplateSource: bytes.Clone(templateSource),
		CheckpointJSON: bytes.Clone(checkpointJSON), Checkpoint: checkpoint,
		Binding: pathv1.CurrentCheckpointBinding(checkpoint), Input: verified,
	})
}

// AppendPathV1 atomically installs exactly one sealed planner/reducer
// transition. It is deliberately concrete-FS-only and absent from Store so
// live schema-v6 schedulers cannot discover or execute path-v1.
func (s *FS) AppendPathV1(ctx context.Context, runID string, transition *pathv1.ExecutionTransition) (PathV1AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return PathV1AppendResult{}, err
	}
	if transition == nil {
		return PathV1AppendResult{}, fmt.Errorf("sealed path-v1 execution transition is required")
	}
	if err := safeSegment(runID); err != nil {
		return PathV1AppendResult{}, fmt.Errorf("invalid run id: %w", err)
	}
	expected := transition.PreBinding()
	if err := expected.Validate(); err != nil {
		return PathV1AppendResult{}, fmt.Errorf("invalid expected path-v1 binding: %w", err)
	}
	unlockRun, err := s.lockRun(ctx, runID)
	if err != nil {
		return PathV1AppendResult{}, err
	}
	defer unlockRun()
	if s.executionRunLockedHook != nil {
		s.executionRunLockedHook()
	}

	var result PathV1AppendResult
	err = s.withPathV1ExecutionViewRunLocked(ctx, runID, s.newExecutionViewBudget(ctx), func(view PathV1ExecutionView) error {
		current := view.Binding
		_, desiredBytes, desired, validationErr := pathv1.ValidateExecutionTransitionForAppend(ctx, view.CheckpointJSON, view.TemplateSource, transition)
		if validationErr != nil {
			if current != expected && current != transition.PostBinding() {
				return &ConflictError{RunID: runID, ExpectedSeq: int64(expected.Generation), ActualSeq: int64(current.Generation)}
			}
			return &ExecutionViewInconsistentError{Err: validationErr}
		}
		if bytes.Equal(view.CheckpointJSON, desiredBytes) {
			runDir, err := s.openPathV1InitializationRunDir(runID)
			if err != nil {
				return err
			}
			defer runDir.Close()
			if err := s.syncPathV1AppendDir(runDir); err != nil {
				return err
			}
			result = PathV1AppendResult{Disposition: PathV1AppendAlreadyApplied, Binding: current, Checkpoint: desired}
			return nil
		}
		if current != expected {
			return &ConflictError{RunID: runID, ExpectedSeq: int64(expected.Generation), ActualSeq: int64(current.Generation)}
		}
		runDir, err := s.openPathV1InitializationRunDir(runID)
		if err != nil {
			return err
		}
		defer runDir.Close()
		currentBytes, err := readPathV1InitializationStateAt(ctx, runDir, s.newExecutionViewBudget(ctx))
		if err != nil {
			return err
		}
		if !bytes.Equal(currentBytes, view.CheckpointJSON) {
			return &ConflictError{RunID: runID, ExpectedSeq: int64(expected.Generation), ActualSeq: int64(current.Generation)}
		}
		if err := s.requirePathV1RunDirCurrent(runID, runDir); err != nil {
			return err
		}
		if s.pathV1AppendBeforeCommit != nil {
			if err := s.pathV1AppendBeforeCommit(); err != nil {
				return err
			}
		}
		if err := writeFileAtomicAt(runDir, "state.json", desiredBytes, 0o644); err != nil {
			return err
		}
		if err := s.syncPathV1AppendDir(runDir); err != nil {
			return err
		}
		result = PathV1AppendResult{Disposition: PathV1AppendApplied, Binding: pathv1.CurrentCheckpointBinding(desired), Checkpoint: desired}
		if s.pathV1AppendAfterCommit != nil {
			return s.pathV1AppendAfterCommit()
		}
		return nil
	})
	if err != nil {
		return PathV1AppendResult{}, err
	}
	return result, nil
}

// ReconfirmPathV1Durability verifies the exact current terminal checkpoint
// under the run/template lock order and fsyncs its held run directory. The
// executor uses this after an ambiguous terminal rename so visibility
// alone is never reported as durable success.
func (s *FS) ReconfirmPathV1Durability(ctx context.Context, runID string, expected pathv1.CheckpointBinding) (*pathv1.CheckpointV7, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := expected.Validate(); err != nil {
		return nil, fmt.Errorf("invalid expected path-v1 binding: %w", err)
	}
	if err := safeSegment(runID); err != nil {
		return nil, fmt.Errorf("invalid run id: %w", err)
	}
	unlockRun, err := s.lockRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer unlockRun()
	var checkpoint *pathv1.CheckpointV7
	err = s.withPathV1ExecutionViewRunLocked(ctx, runID, s.newExecutionViewBudget(ctx), func(view PathV1ExecutionView) error {
		if view.Binding != expected {
			return &ConflictError{RunID: runID, ExpectedSeq: int64(expected.Generation), ActualSeq: int64(view.Binding.Generation)}
		}
		runDir, err := s.openPathV1InitializationRunDir(runID)
		if err != nil {
			return err
		}
		defer runDir.Close()
		current, err := readPathV1InitializationStateAt(ctx, runDir, s.newExecutionViewBudget(ctx))
		if err != nil {
			return err
		}
		if !bytes.Equal(current, view.CheckpointJSON) {
			return &ConflictError{RunID: runID, ExpectedSeq: int64(expected.Generation), ActualSeq: int64(view.Binding.Generation)}
		}
		if err := s.requirePathV1RunDirCurrent(runID, runDir); err != nil {
			return err
		}
		if err := s.syncPathV1AppendDir(runDir); err != nil {
			return err
		}
		encoded, err := pathv1.EncodeCheckpointV7(view.Checkpoint)
		if err != nil {
			return err
		}
		checkpoint, err = pathv1.DecodeCheckpointV7(encoded)
		return err
	})
	return checkpoint, err
}

func (s *FS) syncPathV1AppendDir(dir *os.File) error {
	if s.pathV1AppendDirSync != nil {
		if err := s.pathV1AppendDirSync(); err != nil {
			return fmt.Errorf("sync path-v1 append directory: %w", err)
		}
	}
	if skipDurabilitySyncs {
		return nil
	}
	if err := unix.Fsync(int(dir.Fd())); err != nil {
		return fmt.Errorf("sync path-v1 append directory: %w", err)
	}
	return nil
}

func pathV1ExecutionReadError(component string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isExecutionBudgetError(err) || errors.Is(err, ErrUnsafeRunPath) {
		return err
	}
	if IsDecodeError(err) || errors.Is(err, ErrContentMismatch) || errors.Is(err, ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrTemplateSavePending) {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("%s: %w", component, err)}
	}
	return err
}
