package ports

import "github.com/tofutools/tclaude/internal/backend/model"

// ContextUsageProjector interprets its provider's already-persisted evidence.
// It performs no collection, native IO, or state repair during a query.
type ContextUsageProjector interface {
	ProjectContextUsage(model.Execution) *model.ContextUsage
}
