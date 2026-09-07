package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type StageTerminalFileRequest struct {
	Context     RequestContext
	ExecutionID model.ExecutionID
	Filename    string
	Content     []byte
}
type TerminalFileResult struct {
	Operation model.Operation
	File      model.TerminalFile
	Repeated  bool
}
type TerminalFileAdmission struct {
	Operation model.Operation
	Authority model.AuthorityRequest
	File      model.TerminalFile
}
type TerminalFileStore interface {
	AdmitTerminalFile(context.Context, TerminalFileAdmission) (TerminalFileResult, error)
	ConsumeTerminalFile(context.Context, model.OperationID, time.Time) error
	CompleteTerminalFile(context.Context, OperationCompletion, model.TerminalFile) (TerminalFileResult, error)
}
type TerminalFileAPI interface {
	StageTerminalFile(context.Context, StageTerminalFileRequest) (TerminalFileResult, error)
}

func (s *Service) StageTerminalFile(ctx context.Context, in StageTerminalFileRequest) (TerminalFileResult, error) {
	if err := validateEffectContext(in.Context); err != nil {
		return TerminalFileResult{}, err
	}
	if in.ExecutionID.Validate() != nil || in.Filename == "" || len(in.Filename) > 240 || !utf8.ValidString(in.Filename) || strings.ContainsAny(in.Filename, "/\\") || strings.IndexFunc(in.Filename, unicode.IsControl) >= 0 || len(in.Content) == 0 || len(in.Content) > 8<<20 {
		return TerminalFileResult{}, ErrInvalid
	}
	store, ok := s.store.(TerminalFileStore)
	if !ok {
		return TerminalFileResult{}, ErrUnsupported
	}
	at := s.now().UTC()
	digest := sha256.Sum256(in.Content)
	operation := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: in.Context.RequestID, Kind: model.OperationStageTerminalFile, Principal: in.Context.Principal, ExecutionID: in.ExecutionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: at, UpdatedAt: at}
	file := model.TerminalFile{OperationID: operation.ID, ExecutionID: in.ExecutionID, Filename: in.Filename, Size: int64(len(in.Content)), SHA256: hex.EncodeToString(digest[:])}
	result, err := store.AdmitTerminalFile(ctx, TerminalFileAdmission{Operation: operation, File: file, Authority: model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionStageTerminalFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: in.ExecutionID}}})
	if err != nil {
		return result, err
	}
	if result.Repeated {
		return result, terminalFileOutcome(result.Operation)
	}
	workflow, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	execution, err := s.store.Execution(workflow, in.ExecutionID)
	var owned any
	if err == nil {
		if execution.Workload == model.ExecutionWorkloadShell {
			owned, err = s.hostRuntimeFor(execution)
		} else {
			owned, err = s.runtimeFor(workflow, execution)
		}
	}
	staged := ports.StageTerminalFileResult{Disposition: ports.EffectUnsupported}
	if err == nil {
		if stager, supported := owned.(ports.TerminalFileStager); supported {
			staged, err = stager.StageTerminalFile(workflow, ports.StageTerminalFileRequest{ExecutionID: in.ExecutionID, OperationID: operation.ID, Filename: in.Filename, Content: in.Content, Permit: &terminalFilePermit{store: store, execution: in.ExecutionID, operation: operation.ID, now: s.now}})
		}
	}
	completion := OperationCompletion{OperationID: operation.ID, ExecutionID: in.ExecutionID, OperationState: model.OperationRefused, ResultCode: "file_refused", At: s.now().UTC()}
	switch staged.Disposition {
	case ports.EffectAccepted:
		if err != nil || staged.NativePath == "" || len(staged.NativePath) > 4096 || !utf8.ValidString(staged.NativePath) || strings.IndexFunc(staged.NativePath, unicode.IsControl) >= 0 {
			completion.OperationState = model.OperationUncertain
			completion.ResultCode = "file_unknown"
		} else {
			completion.OperationState = model.OperationSucceeded
			completion.ResultCode = "file_staged"
			file.NativePath = staged.NativePath
			file.StagedAt = completion.At
		}
	case ports.EffectRefused: // A proven refusal leaves the workload state intact.
	case ports.EffectUnsupported:
		completion.ResultCode = "file_unsupported"
	default:
		completion.OperationState = model.OperationUncertain
		completion.ResultCode = "file_unknown"
	}
	if err != nil {
		completion.Detail = err.Error()
		if errors.Is(err, ErrUnavailable) {
			completion.ResultCode = "file_unavailable"
		}
	}
	settlement, finish := settlementContext(ctx)
	defer finish()
	result, persistErr := store.CompleteTerminalFile(settlement, completion, file)
	if persistErr != nil {
		return result, errors.Join(ErrUncertain, persistErr)
	}
	return result, terminalFileOutcome(result.Operation)
}
func terminalFileOutcome(operation model.Operation) error {
	switch operation.State {
	case model.OperationSucceeded:
		return nil
	case model.OperationRefused:
		return ErrConflict
	default:
		return ErrUncertain
	}
}

type terminalFilePermit struct {
	store     TerminalFileStore
	execution model.ExecutionID
	operation model.OperationID
	now       func() time.Time
	used      atomic.Bool
}

func (p *terminalFilePermit) ExecutionID() model.ExecutionID { return p.execution }
func (p *terminalFilePermit) OperationID() model.OperationID { return p.operation }
func (p *terminalFilePermit) Consume(ctx context.Context) error {
	if !p.used.CompareAndSwap(false, true) {
		return ErrConflict
	}
	return p.store.ConsumeTerminalFile(ctx, p.operation, p.now().UTC())
}
