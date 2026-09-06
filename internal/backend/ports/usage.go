package ports

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type UsageCapabilities struct {
	CollectCounters bool
	CollectCost     bool
	CostKind        model.UsageCostKind
	CoverageNote    string
}

// UsageCollectionRequest is assembled from durable application state. Native
// references and history selections are never accepted from a public caller.
type UsageCollectionRequest struct {
	Execution model.Execution
	Native    model.NativeConversationEvidence
	History   *HistorySourceSelection
}

// CollectedUsage carries a private stable source key used for replacement and
// deduplication. Only Source and SourceRevision are copied to public records.
type CollectedUsage struct {
	SourceKey      string
	Source         string
	SourceRevision string
	ObservedAt     time.Time
	Counters       []model.UsageCounter
	Cost           *model.UsageCost
	Coverage       model.UsageCoverage
	Cumulative     bool
}

type UsageReader interface {
	Capabilities() UsageCapabilities
	Collect(context.Context, UsageCollectionRequest) (CollectedUsage, error)
}

type UsageProvider interface {
	Provider
	Usage() UsageReader
}
