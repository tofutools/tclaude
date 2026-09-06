package copilot

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	legacyharness "github.com/tofutools/tclaude/pkg/claude/harness"
)

type usageReader struct{ provider *Provider }

func (p *Provider) Usage() ports.UsageReader { return usageReader{provider: p} }
func (usageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true, CoverageNote: "Copilot assistant_usage_events counters and native nano-AIU charge units; no monetary currency is reported"}
}

func (r usageReader) Collect(ctx context.Context, request ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	key := "copilot:" + request.Native.Reference
	store, err := legacyharness.OpenCopilotUsageStore(r.provider.nativeHome)
	if errors.Is(err, legacyharness.ErrCopilotUsageStoreAbsent) {
		return ports.CollectedUsage{SourceKey: key, Source: "copilot.assistant_usage_events", SourceRevision: "unresolved-v1", ObservedAt: request.Execution.UpdatedAt,
			Coverage: model.UsageCoverage{Counters: model.UsageCoverageUnknown, Cost: model.UsageCoverageUnsupported, Reason: "Copilot usage store is unavailable; nano-AIU has no monetary currency"}, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
	}
	if err != nil {
		return ports.CollectedUsage{}, err
	}
	defer func() { _ = store.Close() }()
	totals := map[model.UsageUnit]int64{}
	var after int64
	var observed time.Time
	for {
		calls, err := store.Calls(ctx, []legacyharness.CopilotUsageCursor{{SessionID: request.Native.Reference, AfterEventID: after}}, 500)
		if err != nil {
			return ports.CollectedUsage{}, err
		}
		for _, call := range calls {
			totals[model.UsageInputTokens] += call.InputTokens
			totals[model.UsageOutputTokens] += call.OutputTokens
			totals[model.UsageCacheReadTokens] += call.CacheReadTokens
			totals[model.UsageCacheWriteTokens] += call.CacheWriteTokens
			totals[model.UsageReasoningTokens] += call.ReasoningTokens
			totals[model.UsageRequests]++
			if call.HasNanoAIU {
				totals[model.UsageNanoAIU] += call.TotalNanoAIU
			}
			if parsed, parseErr := time.Parse(time.RFC3339Nano, call.CreatedAt); parseErr == nil && parsed.After(observed) {
				observed = parsed.UTC()
			}
			after = call.EventID
		}
		if len(calls) < 500 {
			break
		}
	}
	if observed.IsZero() {
		observed = request.Execution.UpdatedAt
	}
	counters := orderedCopilotCounters(totals)
	coverage := model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported, Reason: "native nano-AIU is a non-monetary unit; no currency is reported"}
	return ports.CollectedUsage{SourceKey: key, Source: "copilot.assistant_usage_events", SourceRevision: "event-id:" + strconv.FormatInt(after, 10), ObservedAt: observed,
		Counters: counters, Coverage: coverage, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func orderedCopilotCounters(values map[model.UsageUnit]int64) []model.UsageCounter {
	order := []model.UsageUnit{model.UsageInputTokens, model.UsageOutputTokens, model.UsageCacheReadTokens, model.UsageCacheWriteTokens, model.UsageReasoningTokens, model.UsageRequests, model.UsageNanoAIU}
	out := make([]model.UsageCounter, 0, len(values))
	for _, unit := range order {
		if value, ok := values[unit]; ok {
			out = append(out, model.UsageCounter{Unit: unit, Value: value})
		}
	}
	return out
}

var _ ports.UsageProvider = (*Provider)(nil)
