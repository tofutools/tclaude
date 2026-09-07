package app

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type ReadExecutionFileRequest struct {
	Principal   model.Principal
	ExecutionID model.ExecutionID
	Path        string
}
type ExecutionFileAPI interface {
	ReadExecutionFile(context.Context, ReadExecutionFileRequest) (ports.ExecutionFileContent, error)
}
type ExecutionFileStore interface {
	ExecutionFileTarget(context.Context, model.Principal, model.ExecutionID, time.Time) (model.Execution, error)
}

func (s *Service) ReadExecutionFile(ctx context.Context, in ReadExecutionFileRequest) (ports.ExecutionFileContent, error) {
	var empty ports.ExecutionFileContent
	if in.ExecutionID.Validate() != nil || in.Path == "" || len(in.Path) > 4096 || !utf8.ValidString(in.Path) || strings.IndexFunc(in.Path, unicode.IsControl) >= 0 {
		return empty, ErrInvalid
	}
	store, ok := s.store.(ExecutionFileStore)
	if !ok {
		return empty, ErrUnsupported
	}
	execution, err := store.ExecutionFileTarget(ctx, in.Principal, in.ExecutionID, s.now().UTC())
	if err != nil {
		return empty, err
	}
	reader, ok := s.directoryBrowser.(ports.ExecutionFileReader)
	if !ok {
		return empty, ErrUnsupported
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := reader.ReadExecutionFile(bounded, ports.ExecutionFileReadRequest{Root: execution.Spec.WorkingDirectory, Path: in.Path})
	if err != nil {
		return empty, fail(ErrUnavailable, "execution file is unavailable: %v", err)
	}
	current, err := store.ExecutionFileTarget(bounded, in.Principal, in.ExecutionID, s.now().UTC())
	if err != nil {
		return empty, err
	}
	if current.Attempt != execution.Attempt || current.Spec.WorkingDirectory != execution.Spec.WorkingDirectory {
		return empty, ErrConflict
	}
	if len(result.Content) > 32<<20 || result.Filename == "" || len(result.Filename) > 255 || !utf8.ValidString(result.Filename) || strings.ContainsAny(result.Filename, "/\\") || strings.IndexFunc(result.Filename, unicode.IsControl) >= 0 {
		return empty, ErrUnavailable
	}
	return result, nil
}
