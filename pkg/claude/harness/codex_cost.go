package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CodexTokenCost is the cumulative pay-per-token-equivalent cost inferred
// from a Codex rollout's latest token_count event.
type CodexTokenCost struct {
	CostUSD  float64
	Model    string
	Observed time.Time
}

// OpenAIModelPrice is one OpenAI API pricing tier, expressed in USD per
// million tokens. CacheWritePerMTok is populated where OpenAI publishes a
// distinct cache-write rate even though Codex rollouts do not expose that
// token category.
type OpenAIModelPrice struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	CacheWritePerMTok  float64
	OutputPerMTok      float64
}

// OpenAIModelPricing contains the ordinary and optional long-context tiers
// shared by subscription-backed harnesses' WHAT-IF projections.
type OpenAIModelPricing struct {
	Short OpenAIModelPrice
	Long  *OpenAIModelPrice
}

// codexModelPrices lists models whose ordinary-input, cached-read, and output
// categories match Codex rollout token_count fields. Rates are Standard USD per
// 1M tokens from OpenAI API pricing. GPT-5.6 also bills explicit cache writes at
// 1.25x input, but the rollout exposes no cache-write token category; when such
// a write occurs, this what-if estimate is therefore a lower bound. A nil Long
// means the pricing page does not list long-context rates for that model. Pro
// rows list no cached-input discount, so cached input is charged at the regular
// input rate.
var codexModelPrices = map[string]OpenAIModelPricing{
	"gpt-5.6-sol": {
		Short: OpenAIModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 30.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 10.00, CachedInputPerMTok: 1.00, CacheWritePerMTok: 12.50, OutputPerMTok: 45.00},
	},
	"gpt-5.6-terra": {
		Short: OpenAIModelPrice{InputPerMTok: 2.00, CachedInputPerMTok: 0.20, CacheWritePerMTok: 2.50, OutputPerMTok: 12.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 4.00, CachedInputPerMTok: 0.40, CacheWritePerMTok: 5.00, OutputPerMTok: 18.00},
	},
	"gpt-5.6-luna": {
		Short: OpenAIModelPrice{InputPerMTok: 0.20, CachedInputPerMTok: 0.02, CacheWritePerMTok: 0.25, OutputPerMTok: 1.20},
		Long:  &OpenAIModelPrice{InputPerMTok: 0.40, CachedInputPerMTok: 0.04, CacheWritePerMTok: 0.50, OutputPerMTok: 1.80},
	},
	"gpt-5.5": {
		Short: OpenAIModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, OutputPerMTok: 30.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 10.00, CachedInputPerMTok: 1.00, OutputPerMTok: 45.00},
	},
	"gpt-5.5-pro": {
		Short: OpenAIModelPrice{InputPerMTok: 30.00, CachedInputPerMTok: 30.00, OutputPerMTok: 180.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 60.00, CachedInputPerMTok: 60.00, OutputPerMTok: 270.00},
	},
	"gpt-5.4": {
		Short: OpenAIModelPrice{InputPerMTok: 2.50, CachedInputPerMTok: 0.25, OutputPerMTok: 15.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, OutputPerMTok: 22.50},
	},
	"gpt-5.4-mini": {
		Short: OpenAIModelPrice{InputPerMTok: 0.75, CachedInputPerMTok: 0.075, OutputPerMTok: 4.50},
	},
	"gpt-5.4-nano": {
		Short: OpenAIModelPrice{InputPerMTok: 0.20, CachedInputPerMTok: 0.02, OutputPerMTok: 1.25},
	},
	"gpt-5.4-pro": {
		Short: OpenAIModelPrice{InputPerMTok: 30.00, CachedInputPerMTok: 30.00, OutputPerMTok: 180.00},
		Long:  &OpenAIModelPrice{InputPerMTok: 60.00, CachedInputPerMTok: 60.00, OutputPerMTok: 270.00},
	},
	// Current specialized Codex row. gpt-5-codex is kept as the common Codex
	// CLI/profile alias used across tclaude even when the public pricing row
	// carries a dated minor version.
	"gpt-5.3-codex": {
		Short: OpenAIModelPrice{InputPerMTok: 1.75, CachedInputPerMTok: 0.175, OutputPerMTok: 14.00},
	},
	"gpt-5-codex": {
		Short: OpenAIModelPrice{InputPerMTok: 1.75, CachedInputPerMTok: 0.175, OutputPerMTok: 14.00},
	},
	// gpt-5.3-codex-spark is intentionally absent: it is a research preview
	// whose rate is not final. Unknown prices remain unestimated rather than
	// borrowing another model's rate.
}

// LookupOpenAIModelPricing returns the same API-rate catalog used by Codex's
// WHAT-IF projection. Other subscription-backed OpenAI harnesses use this when
// their native provider adapter deliberately reports zero billing rates.
func LookupOpenAIModelPricing(model string) (OpenAIModelPricing, bool) {
	pricing, ok := codexModelPrices[strings.TrimSpace(model)]
	return pricing, ok
}

// CodexVirtualCostFromRollout reads rolloutPath and estimates cumulative
// pay-per-token cost by pricing each token_count.info.last_token_usage block.
// OpenAI selects long-context pricing per request, so applying one tier to the
// cumulative total_token_usage block would misprice a mixed short/long history.
// The model is taken from modelHint when supplied, else from each turn's latest
// turn_context model. ok is false when no priced, non-zero turn was found.
func CodexVirtualCostFromRollout(rolloutPath, modelHint string) (CodexTokenCost, bool, error) {
	rc, err := openCodexRollout(rolloutPath)
	if err != nil {
		return CodexTokenCost{}, false, err
	}
	defer func() { _ = rc.Close() }()

	var (
		latestModel = strings.TrimSpace(modelHint)
		costUSD     float64
		priced      bool
		observed    time.Time
	)
	err = scanCodexRolloutLines(rc, rolloutPath, func(line []byte) bool {
		if len(bytes.TrimSpace(line)) == 0 {
			return true
		}
		var env codexEnvelope
		if json.Unmarshal(line, &env) != nil {
			return true
		}
		switch env.Type {
		case "turn_context":
			var tc codexTurnContext
			if strings.TrimSpace(modelHint) == "" && json.Unmarshal(env.Payload, &tc) == nil && strings.TrimSpace(tc.Model) != "" {
				latestModel = strings.TrimSpace(tc.Model)
			}
		case "event_msg":
			var ev codexTokenCountEvent
			if json.Unmarshal(env.Payload, &ev) != nil || ev.Type != "token_count" {
				return true
			}
			turnUsage := ev.Info.LastTokenUsage
			if !ev.Info.LastTokenUsagePresent {
				if !codexUsageHasBillableTokens(ev.Info.TotalTokenUsage) {
					observed = parseCodexEventTime(env.Timestamp)
					return true
				}
				// Early rollout formats could omit last_token_usage. Preserve
				// their former latest-cumulative behavior as a compatibility
				// fallback; current rollouts take the per-turn path above.
				turnUsage = ev.Info.TotalTokenUsage
				costUSD = 0
				priced = false
			}
			turnCost, ok := codexVirtualCost(latestModel, turnUsage)
			if ok {
				costUSD += turnCost
				priced = true
			}
			observed = parseCodexEventTime(env.Timestamp)
		}
		return true
	})
	if err != nil {
		return CodexTokenCost{}, false, fmt.Errorf("scan codex rollout %s: %w", rolloutPath, err)
	}
	if !priced {
		return CodexTokenCost{}, false, nil
	}
	return CodexTokenCost{CostUSD: costUSD, Model: latestModel, Observed: observed}, true, nil
}

func codexVirtualCost(model string, usage codexTokenUsage) (float64, bool) {
	pricing, ok := codexModelPrices[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	if !codexUsageHasBillableTokens(usage) {
		return 0, false
	}
	price := pricing.Short
	if pricing.Long != nil && usage.InputTokens > OpenAIShortContextInputMax {
		price = *pricing.Long
	}
	cachedInput := usage.CachedInputTokens
	if cachedInput < 0 {
		cachedInput = 0
	}
	if cachedInput > usage.InputTokens {
		cachedInput = usage.InputTokens
	}
	uncachedInput := usage.InputTokens - cachedInput
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	return (float64(uncachedInput)*price.InputPerMTok +
		float64(cachedInput)*price.CachedInputPerMTok +
		float64(usage.OutputTokens)*price.OutputPerMTok) / 1_000_000, true
}

func codexUsageHasBillableTokens(usage codexTokenUsage) bool {
	return usage.InputTokens > 0 || usage.CachedInputTokens > 0 || usage.OutputTokens > 0
}

// OpenAIShortContextInputMax is the largest per-request input that receives
// OpenAI's short-context price. A request above this boundary is priced at the
// long-context rate for all of its input and output tokens.
const OpenAIShortContextInputMax = 272_000
