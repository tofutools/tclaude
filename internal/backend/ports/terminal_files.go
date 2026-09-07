package ports

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// TerminalFilePermit is consumed once immediately before exclusive publication.
type TerminalFilePermit interface {
	ExecutionID() model.ExecutionID
	OperationID() model.OperationID
	Consume(context.Context) error
}
type StageTerminalFileRequest struct {
	ExecutionID model.ExecutionID
	OperationID model.OperationID
	Filename    string
	Content     []byte
	Permit      TerminalFilePermit
}
type StageTerminalFileResult struct {
	Disposition EffectDisposition
	NativePath  string
}

// TerminalFileStager is optional and owned by the exact recovered runtime.
// Implementations retain uploads under their own resource root and never send input.
type TerminalFileStager interface {
	StageTerminalFile(context.Context, StageTerminalFileRequest) (StageTerminalFileResult, error)
}
