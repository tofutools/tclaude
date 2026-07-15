//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

type PathV1InitializationResult struct {
	Disposition pathv1.InitializationDisposition
	Checkpoint  *pathv1.CheckpointV7
}

// InitializePathV1 is the concrete, deploy-disabled schema-7 transition. It
// is intentionally absent from Store and from all Host/planner/executor/viewer
// wiring. The append lock is acquired once; exact-template locking retains the
// established run-then-template order through proof re-derivation and rename.
func (s *FS) InitializePathV1(ctx context.Context, runID string, supplied pathv1.UpgradeNeeded) (PathV1InitializationResult, error) {
	if err := ctx.Err(); err != nil {
		return PathV1InitializationResult{}, err
	}
	if err := safeSegment(runID); err != nil {
		return PathV1InitializationResult{}, fmt.Errorf("invalid run id: %w", err)
	}
	exists, err := s.HasRunView(runID)
	if err != nil {
		return PathV1InitializationResult{}, err
	}
	if !exists {
		return PathV1InitializationResult{}, ErrNotFound
	}

	unlockRun, err := s.lockRun(ctx, runID)
	if err != nil {
		return PathV1InitializationResult{}, err
	}
	defer unlockRun()
	if s.executionRunLockedHook != nil {
		s.executionRunLockedHook()
	}

	runDir, err := s.openPathV1InitializationRunDir(runID)
	if err != nil {
		return PathV1InitializationResult{}, err
	}
	defer runDir.Close()
	data, err := readPathV1InitializationStateAt(ctx, runDir, s.newExecutionViewBudget(ctx))
	if err != nil {
		return PathV1InitializationResult{}, err
	}
	var header struct {
		StateSchemaVersion int `json:"stateSchemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return PathV1InitializationResult{}, &DecodeError{Component: "run state header", Err: err}
	}
	if header.StateSchemaVersion == pathv1.CheckpointStateSchemaVersion {
		checkpoint, err := pathv1.DecodeCheckpointV7(data)
		if err != nil {
			return PathV1InitializationResult{}, fmt.Errorf("%w: %v", pathv1.ErrInitializationInconsistent, err)
		}
		if err := pathv1.ExactInitializationReplay(checkpoint, supplied); err != nil {
			return PathV1InitializationResult{}, err
		}
		tmpl, err := s.validatePathV1ReplayTemplateRunLocked(ctx, runID, checkpoint)
		if err != nil {
			return PathV1InitializationResult{}, err
		}
		expected, err := pathv1.BuildInitialization(ctx, checkpoint.Initialize.UpgradeNeeded, tmpl)
		if err != nil {
			return PathV1InitializationResult{}, fmt.Errorf("%w: rebuild installed initialization: %v", pathv1.ErrInitializationInconsistent, err)
		}
		expectedBytes, err := pathv1.EncodeCheckpointV7(expected)
		if err != nil || !bytes.Equal(data, expectedBytes) {
			return PathV1InitializationResult{}, fmt.Errorf("%w: installed initialization differs from deterministic exact-template replay", pathv1.ErrInitializationInconsistent)
		}
		if err := s.syncPathV1InitializationDir(runDir); err != nil {
			return PathV1InitializationResult{}, err
		}
		return PathV1InitializationResult{Disposition: pathv1.InitializationAlreadyApplied, Checkpoint: checkpoint}, nil
	}
	if header.StateSchemaVersion <= 0 || header.StateSchemaVersion > pathv1.CheckpointStateSchemaVersion {
		_, decodeErr := pathv1.DecodeCheckpointV7(data)
		return PathV1InitializationResult{}, decodeErr
	}
	if err := pathv1.ValidateUpgradeNeeded(supplied); err != nil {
		return PathV1InitializationResult{}, fmt.Errorf("%w: %v", pathv1.ErrInitializationInvalid, err)
	}
	if supplied.Reason != pathv1.UpgradeMigrationRequired || len(supplied.ActiveLegacyIDs) != 0 || len(supplied.CheckpointAdminRecords) != 0 {
		return PathV1InitializationResult{}, fmt.Errorf("%w: proof is not a zero-admin migration authority", pathv1.ErrInitializationInvalid)
	}
	if supplied.RunID != runID {
		return PathV1InitializationResult{}, fmt.Errorf("%w: proof run differs from requested run", pathv1.ErrInitializationInconsistent)
	}

	var result PathV1InitializationResult
	err = s.withExecutionViewRunLocked(ctx, runID, func(view ExecutionView) error {
		derived, err := pathv1.AssessUpgradeNeeded(
			ctx, view.LegacyCheckpointJSON, view.Snapshot.State, view.Snapshot.Run.TemplateRef,
			view.TemplateSourceHash, view.LegacyAdminRecords, view.LegacyAdminResolutions,
		)
		if err != nil {
			return err
		}
		if err := pathv1.RequireExactUpgradeNeeded(supplied, derived); err != nil {
			return err
		}
		if err := pathv1.ValidateUnambiguousLegacyInitialization(view.Snapshot.State, view.Template); err != nil {
			return err
		}
		checkpoint, err := pathv1.BuildInitialization(ctx, derived, view.Template)
		if err != nil {
			return err
		}
		encoded, err := pathv1.EncodeCheckpointV7(checkpoint)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.pathV1InitializeBeforeCommit != nil {
			if err := s.pathV1InitializeBeforeCommit(); err != nil {
				return err
			}
		}
		current, err := readPathV1InitializationStateAt(ctx, runDir, s.newExecutionViewBudget(ctx))
		if err != nil {
			return err
		}
		if !bytes.Equal(current, data) {
			return fmt.Errorf("%w: validated legacy checkpoint changed before initialization commit", ErrWriterInProgress)
		}
		if err := s.requirePathV1RunDirCurrent(runID, runDir); err != nil {
			return err
		}
		if err := writeFileAtomicAt(runDir, "state.json", encoded, 0o644); err != nil {
			return err
		}
		if err := s.syncPathV1InitializationDir(runDir); err != nil {
			return err
		}
		result = PathV1InitializationResult{Disposition: pathv1.InitializationApplied, Checkpoint: checkpoint}
		if s.pathV1InitializeAfterCommit != nil {
			if err := s.pathV1InitializeAfterCommit(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PathV1InitializationResult{}, err
	}
	return result, nil
}

func (s *FS) openPathV1InitializationRunDir(runID string) (*os.File, error) {
	root, err := openViewDir(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	runs, err := openViewDirAt(root, "runs")
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	run, err := openViewDirAt(runs, runID)
	if err != nil {
		return nil, err
	}
	return run, nil
}

func readPathV1InitializationStateAt(ctx context.Context, runDir *os.File, budget *viewBudget) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := readViewRegularAt(budget, runDir, "state.json", false)
	if err != nil {
		return nil, classifyRequiredViewFile("run state", err)
	}
	return data, nil
}

func (s *FS) requirePathV1RunDirCurrent(runID string, held *os.File) error {
	current, err := s.openPathV1InitializationRunDir(runID)
	if err != nil {
		return ErrUnsafeRunPath
	}
	defer current.Close()
	var heldStat, currentStat unix.Stat_t
	if err := unix.Fstat(int(held.Fd()), &heldStat); err != nil {
		return err
	}
	if err := unix.Fstat(int(current.Fd()), &currentStat); err != nil {
		return err
	}
	if heldStat.Dev != currentStat.Dev || heldStat.Ino != currentStat.Ino {
		return ErrUnsafeRunPath
	}
	return nil
}

func writeFileAtomicAt(dir *os.File, name string, data []byte, perm os.FileMode) error {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create atomic temp name: %w", err)
	}
	tmpName := fmt.Sprintf(".%s-%x.tmp", name, nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return fmt.Errorf("create atomic temp file: %w", err)
	}
	tmp := os.NewFile(uintptr(fd), tmpName)
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), tmpName, 0) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write atomic temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod atomic temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync atomic temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close atomic temp file: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), tmpName, int(dir.Fd()), name); err != nil {
		return fmt.Errorf("rename atomic temp file: %w", err)
	}
	return nil
}

func (s *FS) syncPathV1InitializationDir(dir *os.File) error {
	if s.pathV1InitializeDirSync != nil {
		if err := s.pathV1InitializeDirSync(); err != nil {
			return fmt.Errorf("sync initialized checkpoint directory: %w", err)
		}
	}
	if err := unix.Fsync(int(dir.Fd())); err != nil {
		return fmt.Errorf("sync initialized checkpoint directory: %w", err)
	}
	return nil
}

func (s *FS) validatePathV1ReplayTemplateRunLocked(ctx context.Context, runID string, checkpoint *pathv1.CheckpointV7) (*model.Template, error) {
	budget := s.newExecutionViewBudget(ctx)
	run, err := s.readExecutionRunRecordAt(ctx, runID, budget)
	if err != nil {
		return nil, err
	}
	proof := checkpoint.Initialize.UpgradeNeeded
	if run.ID != proof.RunID || run.TemplateRef != proof.TemplateRef {
		return nil, fmt.Errorf("%w: installed checkpoint differs from run record", pathv1.ErrInitializationInconsistent)
	}
	id, hash, err := parseTemplateRef(proof.TemplateRef)
	if err != nil {
		return nil, fmt.Errorf("%w: installed exact template ref is invalid", pathv1.ErrInitializationInconsistent)
	}
	if run.Template != nil {
		embeddedHash, hashErr := model.SemanticHash(run.Template)
		if hashErr != nil || run.Template.ID != id || embeddedHash != hash {
			return nil, fmt.Errorf("%w: embedded run template does not match installed exact template ref", pathv1.ErrInitializationInconsistent)
		}
	}
	unlockTemplate, err := s.lockTemplateView(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlockTemplate()
	if s.executionTemplateLockedHook != nil {
		s.executionTemplateLockedHook()
	}
	body, err := s.getTemplateExactBodyWithBudget(ctx, id, hash, budget)
	if err != nil {
		return nil, err
	}
	var tmpl model.Template
	if err := decodeViewJSON(ctx, body, &tmpl, true); err != nil {
		return nil, &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template cannot be decoded: %w", err)}
	}
	semanticHash, err := model.SemanticHash(&tmpl)
	if err != nil || tmpl.ID != id || semanticHash != hash {
		return nil, &ExecutionViewInconsistentError{Err: ErrContentMismatch}
	}
	source, err := s.getTemplateExactSourceWithBudget(ctx, id, hash, budget, &tmpl)
	if err != nil {
		return nil, err
	}
	parsed, err := model.Parse(source)
	if err != nil || parsed.Template == nil || parsed.Ref != proof.TemplateRef || parsed.SourceHash != proof.TemplateSourceHash {
		return nil, fmt.Errorf("%w: installed checkpoint exact template/source mismatch", pathv1.ErrInitializationInconsistent)
	}
	return &tmpl, nil
}
