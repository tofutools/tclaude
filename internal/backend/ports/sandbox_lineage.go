package ports

import "github.com/tofutools/tclaude/internal/backend/model"

// SandboxLineageProvider owns native interpretation and private evidence
// decoding. Requested postures remain conditional on successful preparation;
// recorded postures require matching provider evidence for the execution.
type SandboxLineageProvider interface {
	RequestedSandboxPosture(model.ResolvedExecutionSpec) model.SandboxPosture
	RecordedSandboxPosture(model.Execution) model.SandboxPosture
}
