package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// UsageStore is separate from Store so the usage capability can evolve without
// widening unrelated orchestration implementations.
type UsageStore interface {
	ResolveUsageTarget(context.Context, UsageTarget) (UsageTargetRecord, error)
	RecordUsage(context.Context, UsageWrite) (model.UsageObservation, bool, error)
	QueryUsage(context.Context, UsageFilter) (UsageResult, error)
}

type UsageTarget struct {
	ConversationID model.ConversationID
	ExecutionID    model.ExecutionID
}

type UsageTargetRecord struct {
	Execution model.Execution
	History   *HistorySelectionRecord
}

type UsageWrite struct {
	Observation model.UsageObservation
	SourceKey   string
	Cumulative  bool
}

type UsageFilter struct {
	Target UsageTarget
	After  time.Time
	Before time.Time
	Limit  int
	Cursor string
}

type UsageResult struct {
	Observations []model.UsageObservation
	NextCursor   string
}

type RefreshUsageRequest struct {
	Principal model.Principal
	Target    UsageTarget
}

type RefreshUsageResult struct {
	Observation  model.UsageObservation
	Repeated     bool
	Capabilities ports.UsageCapabilities
}

type QueryUsageRequest struct {
	Principal model.Principal
	Filter    UsageFilter
}
