package model

import "time"

type UsageObservationID string

func (id UsageObservationID) Validate() error {
	return ValidateStableID("usage observation id", string(id))
}

// UsageCoverageState says what a native source established. Unsupported and
// unknown are deliberately distinct from a complete observation containing
// zero counters.
type UsageCoverageState string

const (
	UsageCoverageComplete    UsageCoverageState = "complete"
	UsageCoveragePartial     UsageCoverageState = "partial"
	UsageCoverageUnknown     UsageCoverageState = "unknown"
	UsageCoverageUnsupported UsageCoverageState = "unsupported"
)

type UsageCoverage struct {
	Counters UsageCoverageState
	Cost     UsageCoverageState
	Reason   string
}

type UsageUnit string

const (
	UsageInputTokens      UsageUnit = "input_tokens"
	UsageOutputTokens     UsageUnit = "output_tokens"
	UsageCacheReadTokens  UsageUnit = "cache_read_tokens"
	UsageCacheWriteTokens UsageUnit = "cache_write_tokens"
	UsageReasoningTokens  UsageUnit = "reasoning_tokens"
	UsageRequests         UsageUnit = "requests"
	UsageNanoAIU          UsageUnit = "nano_aiu"
)

type UsageCounter struct {
	Unit  UsageUnit
	Value int64
}

type UsageCostKind string

const (
	UsageCostNativeReported     UsageCostKind = "native_reported"
	UsageCostHistoricalEstimate UsageCostKind = "historical_estimate"
)

// UsageCost keeps the source's exact decimal spelling. Currency is required;
// the platform never converts currencies or derives a price from token counts.
type UsageCost struct {
	Amount   string
	Currency string
	Kind     UsageCostKind
}

type UsageAttribution struct {
	AgentID        AgentID
	ConversationID ConversationID
	ExecutionID    ExecutionID
}

// UsageObservation is the public durable view. Provider paths, native tokens,
// and receipts are intentionally absent.
type UsageObservation struct {
	ID             UsageObservationID
	Attribution    UsageAttribution
	Harness        string
	Source         string
	SourceRevision string
	ObservedAt     time.Time
	CollectedAt    time.Time
	Counters       []UsageCounter
	Cost           *UsageCost
	Coverage       UsageCoverage
	Historical     bool
}
