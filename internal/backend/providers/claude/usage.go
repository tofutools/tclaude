package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type usageReader struct{}

func (*Provider) Usage() ports.UsageReader { return usageReader{} }

func (usageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true, CoverageNote: "Claude Code transcript assistant-message usage; native monetary cost is not reported"}
}

func (usageReader) Collect(ctx context.Context, request ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	if err := ctx.Err(); err != nil {
		return ports.CollectedUsage{}, err
	}
	if request.History == nil {
		return unsupportedClaudeUsage(request), nil
	}
	evidence, err := decodeClaudeHistoryEvidence(request.History.Evidence)
	if err != nil || evidence.NativeID != request.Native.Reference || evidence.Fingerprint != request.History.SourceFingerprint {
		return ports.CollectedUsage{}, fmt.Errorf("claude usage history binding is invalid")
	}
	raw, err := os.ReadFile(evidence.Path)
	if err != nil {
		return ports.CollectedUsage{}, err
	}
	digest := sha256.Sum256(raw)
	revision := hex.EncodeToString(digest[:])
	counters, observed, partial := collectClaudeUsage(raw)
	coverage := model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported, Reason: "Claude Code transcript does not report monetary cost"}
	if partial {
		coverage.Counters = model.UsageCoveragePartial
	}
	if observed.IsZero() {
		observed = request.Native.ObservedAt
	}
	return ports.CollectedUsage{SourceKey: "claude:" + evidence.Fingerprint, Source: "claude.transcript.message.usage", SourceRevision: revision, ObservedAt: observed,
		Counters: counters, Coverage: coverage, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func collectClaudeUsage(raw []byte) ([]model.UsageCounter, time.Time, bool) {
	totals := map[model.UsageUnit]int64{}
	var observed time.Time
	partial := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var row struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role  string `json:"role"`
				Usage *struct {
					Input      int64 `json:"input_tokens"`
					Output     int64 `json:"output_tokens"`
					CacheRead  int64 `json:"cache_read_input_tokens"`
					CacheWrite int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			partial = true
			continue
		}
		if row.Type != "assistant" || row.Message.Role != "assistant" || row.Message.Usage == nil {
			continue
		}
		values := []struct {
			unit  model.UsageUnit
			value int64
		}{{model.UsageInputTokens, row.Message.Usage.Input}, {model.UsageOutputTokens, row.Message.Usage.Output}, {model.UsageCacheReadTokens, row.Message.Usage.CacheRead}, {model.UsageCacheWriteTokens, row.Message.Usage.CacheWrite}}
		for _, value := range values {
			if value.value < 0 {
				partial = true
				continue
			}
			totals[value.unit] += value.value
		}
		if parsed, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil && parsed.After(observed) {
			observed = parsed.UTC()
		}
	}
	partial = partial || scanner.Err() != nil
	return orderedUsageCounters(totals), observed, partial
}

func unsupportedClaudeUsage(request ports.UsageCollectionRequest) ports.CollectedUsage {
	reference := request.Native.Namespace + ":" + request.Native.Reference
	if strings.Trim(reference, ":") == "" {
		reference = string(request.Execution.ConversationID)
	}
	return ports.CollectedUsage{SourceKey: "claude:" + reference, Source: "claude.transcript.message.usage", SourceRevision: "unresolved-v1", ObservedAt: request.Execution.UpdatedAt,
		Coverage: model.UsageCoverage{Counters: model.UsageCoverageUnknown, Cost: model.UsageCoverageUnsupported, Reason: "no composition-selected Claude transcript source"}, Cumulative: true, Attribution: model.UsageAttributionConversation}
}

func orderedUsageCounters(values map[model.UsageUnit]int64) []model.UsageCounter {
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
