package state

import (
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

type Diagnostics = model.Diagnostics
type Diagnostic = model.Diagnostic
type Severity = model.Severity

const (
	SeverityError   = model.SeverityError
	SeverityWarning = model.SeverityWarning
)

func diagError(code, path, message string) Diagnostic {
	return Diagnostic{Severity: SeverityError, Code: code, Path: path, Message: message}
}
