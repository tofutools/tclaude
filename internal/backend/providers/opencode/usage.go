package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type usageReader struct{ provider *Provider }

func (p *Provider) Usage() ports.UsageReader { return usageReader{provider: p} }
func (usageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true, CollectCost: true, CostKind: model.UsageCostNativeReported, CoverageNote: "OpenCode export message tokens and native reported USD cost"}
}

func (r usageReader) Collect(ctx context.Context, request ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	var raw []byte
	var revision, key string
	var err error
	if request.History != nil {
		var evidence historyEvidence
		var readErr error
		_, raw, revision, evidence, readErr = historyReader{provider: r.provider}.readSelection(ctx, *request.History)
		if readErr != nil {
			return ports.CollectedUsage{}, readErr
		}
		key = "opencode:" + evidence.Fingerprint
	} else {
		var stateRoot string
		entries, _ := os.ReadDir(r.provider.privateRoot)
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "execution-") {
				continue
			}
			candidate := filepath.Join(r.provider.privateRoot, entry.Name())
			manifest, manifestErr := readHistoryManifest(candidate)
			if manifestErr == nil && manifest.NativeID == request.Native.Reference {
				stateRoot = candidate
				break
			}
		}
		if stateRoot == "" {
			return ports.CollectedUsage{SourceKey: "opencode:" + request.Native.Reference, Source: "opencode.session.export", SourceRevision: "unresolved-v1", ObservedAt: request.Execution.UpdatedAt,
				Coverage: model.UsageCoverage{Counters: model.UsageCoverageUnknown, Cost: model.UsageCoverageUnknown, Reason: "attributed OpenCode session is unavailable"}, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
		}
		manifest, _ := readHistoryManifest(stateRoot)
		_, raw, revision, err = historyReader{provider: r.provider}.export(ctx, stateRoot, manifest.NativeID, manifest.CWD)
		if err != nil {
			return ports.CollectedUsage{}, err
		}
		key = "opencode:" + historySourceFingerprint(stateRoot, manifest.NativeID)
	}
	counters, cost, observed, partial, err := collectOpenCodeUsage(raw)
	if err != nil {
		return ports.CollectedUsage{}, err
	}
	coverage := model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageComplete}
	if cost == nil {
		coverage.Cost, coverage.Reason = model.UsageCoverageUnknown, "exported messages did not report native cost"
	}
	if partial {
		coverage.Counters, coverage.Cost, coverage.Reason = model.UsageCoveragePartial, model.UsageCoveragePartial, "some exported usage fields were invalid"
	}
	if observed.IsZero() {
		observed = request.Execution.UpdatedAt
	}
	return ports.CollectedUsage{SourceKey: key, Source: "opencode.session.export", SourceRevision: revision, ObservedAt: observed, Counters: counters, Cost: cost,
		Coverage: coverage, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func collectOpenCodeUsage(raw []byte) ([]model.UsageCounter, *model.UsageCost, time.Time, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value struct {
		Info struct {
			Time struct {
				Updated int64 `json:"updated"`
			} `json:"time"`
		} `json:"info"`
		Messages []struct {
			Info struct {
				Role   string      `json:"role"`
				Cost   json.Number `json:"cost"`
				Tokens struct {
					Input     int64 `json:"input"`
					Output    int64 `json:"output"`
					Reasoning int64 `json:"reasoning"`
					Cache     struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"info"`
		} `json:"messages"`
	}
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, time.Time{}, false, err
	}
	totals := map[model.UsageUnit]int64{}
	cost := new(big.Rat)
	haveCost, partial := false, false
	for _, message := range value.Messages {
		if message.Info.Role != "assistant" {
			continue
		}
		values := []struct {
			unit  model.UsageUnit
			value int64
		}{{model.UsageInputTokens, message.Info.Tokens.Input}, {model.UsageOutputTokens, message.Info.Tokens.Output}, {model.UsageReasoningTokens, message.Info.Tokens.Reasoning}, {model.UsageCacheReadTokens, message.Info.Tokens.Cache.Read}, {model.UsageCacheWriteTokens, message.Info.Tokens.Cache.Write}}
		for _, value := range values {
			if value.value < 0 {
				partial = true
				continue
			}
			totals[value.unit] += value.value
		}
		if spelling := string(message.Info.Cost); spelling != "" {
			part, ok := new(big.Rat).SetString(spelling)
			if !ok || part.Sign() < 0 {
				partial = true
			} else {
				cost.Add(cost, part)
				haveCost = true
			}
		}
	}
	var nativeCost *model.UsageCost
	if haveCost {
		amount, err := finiteDecimal(cost)
		if err != nil {
			return nil, nil, time.Time{}, partial, err
		}
		nativeCost = &model.UsageCost{Amount: amount, Currency: "USD", Kind: model.UsageCostNativeReported}
	}
	return orderedOpenCodeCounters(totals), nativeCost, milliseconds(value.Info.Time.Updated), partial, nil
}

func finiteDecimal(value *big.Rat) (string, error) {
	if value.Sign() == 0 {
		return "0", nil
	}
	denominator := new(big.Int).Set(value.Denom())
	twos, fives := 0, 0
	for new(big.Int).Mod(denominator, big.NewInt(2)).Sign() == 0 {
		denominator.Div(denominator, big.NewInt(2))
		twos++
	}
	for new(big.Int).Mod(denominator, big.NewInt(5)).Sign() == 0 {
		denominator.Div(denominator, big.NewInt(5))
		fives++
	}
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return "", fmt.Errorf("native cost is not a finite decimal")
	}
	return strings.TrimRight(strings.TrimRight(value.FloatString(max(twos, fives)), "0"), "."), nil
}

func orderedOpenCodeCounters(values map[model.UsageUnit]int64) []model.UsageCounter {
	units := make([]model.UsageUnit, 0, len(values))
	for unit := range values {
		units = append(units, unit)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
	out := make([]model.UsageCounter, 0, len(units))
	for _, unit := range units {
		out = append(out, model.UsageCounter{Unit: unit, Value: values[unit]})
	}
	return out
}

var _ ports.UsageProvider = (*Provider)(nil)
