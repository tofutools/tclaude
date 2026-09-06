package app

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// UsageStore is separate from Store so the usage capability can evolve without
// widening unrelated orchestration implementations.
type UsageStore interface {
	ResolveUsageTarget(context.Context, UsageTarget, model.AuthorityRequest, time.Time) (UsageTargetRecord, error)
	RecordUsage(context.Context, UsageWrite) (model.UsageObservation, bool, error)
	ImportHistoricalUsage(context.Context, HistoricalUsageWrite) (model.UsageObservation, bool, error)
	QueryUsage(context.Context, UsageFilter, model.AuthorityRequest, time.Time) (UsageResult, error)
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

// HistoricalUsageWrite is consumed only by the offline importer. Provenance is
// required and the record remains data; it grants no execution authority.
type HistoricalUsageWrite struct {
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

func (s *Service) RefreshUsage(ctx context.Context, req RefreshUsageRequest) (RefreshUsageResult, error) {
	store, ok := s.store.(UsageStore)
	if !ok {
		return RefreshUsageResult{}, fail(ErrUnavailable, "usage persistence is unavailable")
	}
	if err := validateUsageTarget(req.Target); err != nil {
		return RefreshUsageResult{}, err
	}
	target, err := store.ResolveUsageTarget(ctx, req.Target, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionRefreshUsage}, s.now().UTC())
	if err != nil {
		return RefreshUsageResult{}, err
	}
	provider, ok := s.providers.Provider(target.Execution.Spec.Harness)
	if !ok {
		return RefreshUsageResult{}, fail(ErrUnavailable, "usage provider unavailable")
	}
	usageProvider, ok := provider.(ports.UsageProvider)
	if !ok || usageProvider.Usage() == nil {
		return RefreshUsageResult{}, fail(ErrUnsupported, "harness %q does not expose usage collection", provider.Name())
	}
	reader := usageProvider.Usage()
	native := model.NativeConversationEvidence{}
	if target.Execution.NativeConversation != nil {
		native = *target.Execution.NativeConversation
	}
	var history *ports.HistorySourceSelection
	if target.History != nil {
		history = &target.History.Source
	}
	collected, err := reader.Collect(ctx, ports.UsageCollectionRequest{Execution: target.Execution, Native: native, History: history})
	if err != nil {
		if errors.Is(err, ports.ErrUsageUnsupported) {
			return RefreshUsageResult{}, fail(ErrUnsupported, "%v", err)
		}
		return RefreshUsageResult{}, err
	}
	if err = validateCollectedUsage(collected); err != nil {
		return RefreshUsageResult{}, err
	}
	now := s.now().UTC()
	observation := model.UsageObservation{
		ID: model.UsageObservationID(s.newID("usage_")), Attribution: model.UsageAttribution{
			ConversationID: target.Execution.ConversationID, Precision: collected.Attribution,
		}, Harness: provider.Name(), Source: collected.Source, SourceRevision: collected.SourceRevision,
		ObservedAt: collected.ObservedAt.UTC(), CollectedAt: now, Counters: append([]model.UsageCounter(nil), collected.Counters...), Cost: collected.Cost, Coverage: collected.Coverage,
	}
	if collected.Attribution == model.UsageAttributionExecution {
		observation.Attribution.AgentID = target.Execution.AgentID
		observation.Attribution.ExecutionID = target.Execution.ID
	}
	stored, repeated, err := store.RecordUsage(ctx, UsageWrite{Observation: observation, SourceKey: collected.SourceKey, Cumulative: collected.Cumulative})
	return RefreshUsageResult{Observation: stored, Repeated: repeated, Capabilities: reader.Capabilities()}, err
}

func (s *Service) QueryUsage(ctx context.Context, req QueryUsageRequest) (UsageResult, error) {
	store, ok := s.store.(UsageStore)
	if !ok {
		return UsageResult{}, fail(ErrUnavailable, "usage persistence is unavailable")
	}
	if err := validateUsageTarget(req.Filter.Target); err != nil {
		return UsageResult{}, err
	}
	return store.QueryUsage(ctx, req.Filter, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadUsage}, s.now().UTC())
}

func validateUsageTarget(target UsageTarget) error {
	if target.ConversationID == "" && target.ExecutionID == "" {
		return fail(ErrInvalid, "select a conversation or execution")
	}
	return nil
}

func validateCollectedUsage(value ports.CollectedUsage) error {
	if strings.TrimSpace(value.SourceKey) == "" || strings.TrimSpace(value.Source) == "" || strings.TrimSpace(value.SourceRevision) == "" || value.ObservedAt.IsZero() {
		return fail(ErrInvalid, "provider returned incomplete usage source")
	}
	switch value.Attribution {
	case model.UsageAttributionConversation, model.UsageAttributionExecution:
	default:
		return fail(ErrInvalid, "provider returned invalid usage attribution %q", value.Attribution)
	}
	seen := make(map[model.UsageUnit]struct{}, len(value.Counters))
	for _, counter := range value.Counters {
		if counter.Unit == "" || counter.Value < 0 {
			return fail(ErrInvalid, "provider returned invalid usage counter")
		}
		if _, duplicate := seen[counter.Unit]; duplicate {
			return fail(ErrInvalid, "provider returned duplicate usage unit %s", counter.Unit)
		}
		seen[counter.Unit] = struct{}{}
	}
	if value.Cost != nil {
		amount, ok := new(big.Rat).SetString(strings.TrimSpace(value.Cost.Amount))
		if !ok || amount.Sign() < 0 || strings.TrimSpace(value.Cost.Currency) == "" || value.Cost.Kind != model.UsageCostNativeReported {
			return fail(ErrInvalid, "provider returned invalid native usage cost")
		}
	}
	for _, state := range []model.UsageCoverageState{value.Coverage.Counters, value.Coverage.Cost} {
		switch state {
		case model.UsageCoverageComplete, model.UsageCoveragePartial, model.UsageCoverageUnknown, model.UsageCoverageUnsupported:
		default:
			return fail(ErrInvalid, "%s", fmt.Sprintf("provider returned invalid usage coverage %q", state))
		}
	}
	return nil
}
