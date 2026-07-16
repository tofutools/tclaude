//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

const executionAnchorTailBytes = int64(64 << 10)

// WithExecutionView runs callback while holding the run lock followed by the
// immutable template lock. All persisted premises are read no-follow and
// validated before callback begins; neither lock is released until callback
// returns (or its panic unwinds the stack). This surface is intentionally not
// wired into the v6 planner/executor/viewer.
func (s *FS) WithExecutionView(ctx context.Context, runID string, callback func(ExecutionView) error) error {
	if callback == nil {
		return fmt.Errorf("execution view callback is required")
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

	unlockRun, err := s.lockRunView(ctx, runID)
	if err != nil {
		return err
	}
	defer unlockRun()
	if s.executionRunLockedHook != nil {
		s.executionRunLockedHook()
	}
	return s.withExecutionViewRunLocked(ctx, runID, callback)
}

// withExecutionViewRunLocked performs the coherent execution-view read while
// the caller holds the append/run lock. It preserves the public lock order by
// acquiring the immutable template lock exactly once and never re-entering
// WithExecutionView. Dormant schema migration uses this helper so proof
// re-derivation and the final atomic replacement share one run critical
// section.
func (s *FS) withExecutionViewRunLocked(ctx context.Context, runID string, callback func(ExecutionView) error) error {
	baseline, err := s.executionAnchorObservationAt(ctx, runID)
	if err != nil {
		return err
	}

	budget := s.newExecutionViewBudget(ctx)
	run, err := s.readExecutionRunRecordAt(ctx, runID, budget)
	if err != nil {
		if isExecutionBudgetError(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !IsDecodeError(err) {
			return err
		}
		return s.classifyExecutionDisagreement(ctx, runID, baseline, err)
	}
	id, templateHash, err := parseTemplateRef(run.TemplateRef)
	if err != nil || safeSegment(id) != nil {
		return s.classifyExecutionDisagreement(ctx, runID, baseline, fmt.Errorf("run has invalid exact template ref"))
	}
	templateExists, err := s.hasTemplateExactView(id, templateHash)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("%w: open exact template directory", ErrUnsafeRunPath)
		}
		return err
	}
	if !templateExists {
		return s.classifyExecutionDisagreement(ctx, runID, baseline, fmt.Errorf("exact template %q is unavailable", run.TemplateRef))
	}
	unlockTemplate, err := s.lockTemplateView(ctx, id)
	if err != nil {
		return err
	}
	defer unlockTemplate()
	if s.executionTemplateLockedHook != nil {
		s.executionTemplateLockedHook()
	}

	var legacyView pathv1.LegacyStatePredecode
	snapshot, err := s.loadRunViewSnapshotAtWith(ctx, runID, budget, &run, func(ctx context.Context, data []byte) (*state.State, error) {
		decoded, decodeErr := pathv1.PredecodeLegacyStateContext(ctx, data)
		if decodeErr == nil {
			legacyView = decoded
			return decoded.State, nil
		}
		var overBudget *pathv1.OverBudgetError
		if errors.As(decodeErr, &overBudget) {
			return nil, &ExecutionViewOverBudgetError{
				Limit: overBudget.Limit, Component: "state.json",
				Value: int64(overBudget.Value), Maximum: int64(overBudget.Maximum),
			}
		}
		return nil, decodeErr
	})
	if err != nil {
		if isExecutionBudgetError(err) || !isExecutionDataDisagreement(err) {
			return err
		}
		return s.classifyExecutionDisagreement(ctx, runID, baseline, err)
	}

	templateBody, err := s.getTemplateExactBodyWithBudget(ctx, id, templateHash, budget)
	if err != nil {
		if isExecutionBudgetError(err) {
			return err
		}
		if errors.Is(err, ErrTemplateSavePending) || errors.Is(err, os.ErrNotExist) {
			return &ExecutionViewInconsistentError{Err: err}
		}
		return err
	}
	var template model.Template
	if err := runViewDecode(ctx, budget.decodeHook, "execution template", func() error {
		return decodeViewJSON(ctx, templateBody, &template, true)
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template cannot be decoded: %w", err)}
	}
	semanticHash, err := model.SemanticHash(&template)
	if err != nil || template.ID != id || semanticHash != templateHash {
		return &ExecutionViewInconsistentError{Err: ErrContentMismatch}
	}
	templateSource, err := s.getTemplateExactSourceWithBudget(ctx, id, templateHash, budget, &template)
	if err != nil {
		if isExecutionBudgetError(err) {
			return err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template source cannot be read: %w", err)}
	}
	parsedSource, err := model.ParseExactSource(templateSource)
	if err != nil {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template source is invalid: %w", err)}
	}
	if parsedSource.Template == nil || parsedSource.Template.ID != template.ID ||
		parsedSource.SemanticHash != templateHash || parsedSource.Ref != run.TemplateRef {
		return &ExecutionViewInconsistentError{Err: fmt.Errorf("exact template source semantics do not match exact template")}
	}
	if snapshot.Run.Template != nil {
		embeddedHash, hashErr := model.SemanticHash(snapshot.Run.Template)
		if hashErr != nil || snapshot.Run.Template.ID != id || embeddedHash != templateHash {
			return &ExecutionViewInconsistentError{Err: fmt.Errorf("embedded run template does not match exact template ref")}
		}
	}

	evidenceDiagnostics := append(
		evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs),
		evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest)...,
	)
	semanticDiagnostics := append(state.CheckInvariants(snapshot.State), state.CheckTemplateInvariants(snapshot.State, &template)...)
	if evidenceDiagnostics.HasErrors() || semanticDiagnostics.HasErrors() {
		cause := fmt.Errorf("execution view invariant diagnostics: evidence=%v semantic=%v", evidenceDiagnostics.Errors(), semanticDiagnostics.Errors())
		return s.classifyExecutionDisagreement(ctx, runID, baseline, cause)
	}
	if err := s.confirmExecutionAnchorsStable(ctx, runID, baseline); err != nil {
		return err
	}

	return callback(ExecutionView{
		Snapshot:               snapshot,
		Template:               &template,
		TemplateSourceHash:     parsedSource.SourceHash,
		LegacyCheckpointJSON:   legacyView.CanonicalJSON,
		LegacyAdminRecords:     legacyView.AdminRecords,
		LegacyAdminResolutions: legacyView.AdminResolutions,
	})
}

func (s *FS) readExecutionRunRecordAt(ctx context.Context, runID string, budget *viewBudget) (RunRecord, error) {
	root, err := openViewDir(s.root)
	if err != nil {
		return RunRecord{}, err
	}
	defer root.Close()
	runs, err := openViewDirAt(root, "runs")
	if err != nil {
		return RunRecord{}, err
	}
	defer runs.Close()
	runDir, err := openViewDirAt(runs, runID)
	if err != nil {
		return RunRecord{}, err
	}
	defer runDir.Close()
	data, err := readViewRegularAt(budget, runDir, "run.json", false)
	if err != nil {
		return RunRecord{}, classifyRequiredViewFile("run record", err)
	}
	var run RunRecord
	if err := runViewDecode(ctx, budget.decodeHook, "execution run", func() error {
		return decodeViewJSON(ctx, data, &run, false)
	}); err != nil {
		return RunRecord{}, &DecodeError{Component: "run record", Err: err}
	}
	if run.ID != runID {
		return RunRecord{}, &DecodeError{Component: "run identity", Err: errors.New("record id does not match directory")}
	}
	return run, nil
}

func isExecutionBudgetError(err error) bool {
	var budgetErr *ExecutionViewOverBudgetError
	var pathBudgetErr *pathv1.OverBudgetError
	return errors.As(err, &budgetErr) || errors.As(err, &pathBudgetErr) || errors.Is(err, ErrExecutionViewOverBudget)
}

func isExecutionDataDisagreement(err error) bool {
	if IsDecodeError(err) || errors.Is(err, pathv1.ErrLegacyTimestampMalformed) ||
		errors.Is(err, pathv1.ErrLegacyAdminTimestampMissing) || errors.Is(err, ErrNotFound) {
		return true
	}
	var readErr *evidence.ReadError
	return errors.As(err, &readErr)
}

func (s *FS) classifyExecutionDisagreement(ctx context.Context, runID string, baseline [sha256.Size]byte, cause error) error {
	if err := s.confirmExecutionAnchorsStable(ctx, runID, baseline); err != nil {
		return err
	}
	return &ExecutionViewInconsistentError{Err: cause}
}

func (s *FS) confirmExecutionAnchorsStable(ctx context.Context, runID string, baseline [sha256.Size]byte) error {
	if s.executionReobserveHook != nil {
		s.executionReobserveHook()
	}
	after, err := s.executionAnchorObservationAt(ctx, runID)
	if err != nil {
		return err
	}
	if baseline != after {
		return fmt.Errorf("%w: execution anchors changed during bounded observation", ErrWriterInProgress)
	}
	return nil
}

func (s *FS) executionAnchorObservationAt(ctx context.Context, runID string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	root, err := openViewDir(s.root)
	if err != nil {
		return zero, err
	}
	defer root.Close()
	runs, err := openViewDirAt(root, "runs")
	if err != nil {
		return zero, err
	}
	defer runs.Close()
	runDir, err := openViewDirAt(runs, runID)
	if err != nil {
		return zero, err
	}
	defer runDir.Close()

	budget := s.newExecutionViewBudget(ctx)
	digest := sha256.New()
	for _, name := range []string{"run.json", "state.json"} {
		data, err := readViewRegularAt(budget, runDir, name, false)
		if err != nil {
			return zero, err
		}
		writeExecutionFingerprint(digest, name, data)
	}
	manifestTail, err := readExecutionTailAt(budget, runDir, "manifest.jsonl", true)
	if err != nil {
		return zero, err
	}
	writeExecutionFingerprint(digest, "manifest.jsonl", manifestTail)

	nodes, err := openViewDirAt(runDir, "nodes")
	if err == nil {
		var names []string
		for {
			if err := ctx.Err(); err != nil {
				nodes.Close()
				return zero, err
			}
			batch, readErr := nodes.Readdirnames(viewerDirectoryBatch)
			if len(batch) > 0 {
				budget.entries += len(batch)
				if budget.entries > budget.maxEntries {
					nodes.Close()
					return zero, budget.over("directory_entries", "nodes", int64(budget.entries), int64(budget.maxEntries))
				}
				names = append(names, batch...)
			}
			if errors.Is(readErr, io.EOF) && len(batch) == 0 {
				break
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				nodes.Close()
				return zero, readErr
			}
		}
		slices.Sort(names)
		for _, nodeID := range names {
			if safeSegment(nodeID) != nil {
				nodes.Close()
				return zero, ErrUnsafeRunPath
			}
			nodeDir, err := openViewDirAt(nodes, nodeID)
			if err != nil {
				nodes.Close()
				return zero, ErrUnsafeRunPath
			}
			tail, tailErr := readExecutionTailAt(budget, nodeDir, "log.jsonl", true)
			nodeDir.Close()
			if tailErr != nil {
				nodes.Close()
				return zero, tailErr
			}
			writeExecutionFingerprint(digest, "nodes/"+nodeID+"/log.jsonl", tail)
		}
		nodes.Close()
	} else if !errors.Is(err, unix.ENOENT) {
		return zero, ErrUnsafeRunPath
	}

	runLogDir, err := openViewDirAt(runDir, "run")
	if err == nil {
		tail, tailErr := readExecutionTailAt(budget, runLogDir, "log.jsonl", true)
		runLogDir.Close()
		if tailErr != nil {
			return zero, tailErr
		}
		writeExecutionFingerprint(digest, "run/log.jsonl", tail)
	} else if !errors.Is(err, unix.ENOENT) {
		return zero, ErrUnsafeRunPath
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func readExecutionTailAt(budget *viewBudget, parent *os.File, name string, missingEmpty bool) ([]byte, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) && missingEmpty {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrUnsafeRunPath
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrUnsafeRunPath
	}
	if before.Size > budget.maxFile {
		return nil, budget.over("file_bytes", name, before.Size, budget.maxFile)
	}
	readBytes := min(before.Size, executionAnchorTailBytes)
	if budget.bytes+readBytes > budget.maxTotal {
		return nil, budget.over("total_bytes", name, budget.bytes+readBytes, budget.maxTotal)
	}
	data := make([]byte, readBytes)
	if readBytes > 0 {
		n, readErr := file.ReadAt(data, before.Size-readBytes)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if int64(n) != readBytes {
			return nil, fmt.Errorf("%w: evidence tail changed during read", ErrWriterInProgress)
		}
	}
	if err := budget.ctx.Err(); err != nil {
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("%w: evidence tail identity changed", ErrWriterInProgress)
	}
	if after.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrUnsafeRunPath
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size {
		return nil, fmt.Errorf("%w: evidence tail changed during read", ErrWriterInProgress)
	}
	budget.bytes += readBytes
	return data, nil
}

func writeExecutionFingerprint(h hash.Hash, name string, data []byte) {
	_, _ = io.WriteString(h, strconv.Itoa(len(name)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, name)
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, strconv.Itoa(len(data)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.Copy(h, bytes.NewReader(data))
}
