package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// UsageSummaryFilter selects source observations, not a claim of consumption
// timing. Cumulative deltas are located at their newer observation time.
type UsageSummaryFilter struct {
	After          time.Time
	Before         time.Time
	Harness        string
	AgentID        model.AgentID
	ConversationID model.ConversationID
}

type UsageSummarySample struct {
	Observation model.UsageObservation
	Cumulative  bool
	SourceKey   string `json:"-"`
}

type UsageSummaryStore interface {
	UsageSummarySamples(context.Context, model.Principal, UsageSummaryFilter) ([]UsageSummarySample, error)
}

type UsageSummaryRequest struct {
	Principal model.Principal
	Filter    UsageSummaryFilter
}

type ExactUsageCounter struct {
	Unit  model.UsageUnit
	Value string
}
type ExactUsageCost struct {
	Amount   string
	Currency string
	Kind     model.UsageCostKind
}

type UsageSummaryRow struct {
	Cumulative           bool
	Harness              string
	Source               string
	Historical           bool
	Attribution          model.UsageAttribution
	Day                  string
	Counters             []ExactUsageCounter
	Costs                []ExactUsageCost
	Observations         int
	MissingBaselines     int
	Resets               int
	PartialObservations  int
	UnpricedObservations int
}

type UsageSummaryGap struct {
	ObservationID model.UsageObservationID
	ObservedAt    time.Time
	Attribution   model.UsageAttribution
	Harness       string
	Source        string
	Reason        string
}

type UsageSummaryResult struct {
	Gaps         []UsageSummaryGap
	Filter       UsageSummaryFilter
	Rows         []UsageSummaryRow
	Observations int
	Basis        string
}

func (s *Service) SummarizeUsage(ctx context.Context, req UsageSummaryRequest) (UsageSummaryResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return UsageSummaryResult{}, err
	}
	f := req.Filter
	if f.After.IsZero() || f.Before.IsZero() || !f.After.Before(f.Before) || f.Before.Sub(f.After) > 366*24*time.Hour {
		return UsageSummaryResult{}, fail(ErrInvalid, "choose an observed-time range of at most 366 days")
	}
	if len(f.Harness) > 128 {
		return UsageSummaryResult{}, ErrInvalid
	}
	if f.AgentID != "" && f.AgentID.Validate() != nil {
		return UsageSummaryResult{}, ErrInvalid
	}
	if f.ConversationID != "" && f.ConversationID.Validate() != nil {
		return UsageSummaryResult{}, ErrInvalid
	}
	store, ok := s.store.(UsageSummaryStore)
	if !ok {
		return UsageSummaryResult{}, ErrUnsupported
	}
	samples, err := store.UsageSummarySamples(ctx, req.Principal, f)
	if err != nil {
		return UsageSummaryResult{}, err
	}
	return summarizeUsage(f, samples)
}
