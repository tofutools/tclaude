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

type codexModelPrice struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	OutputPerMTok      float64
}

type codexModelPricing struct {
	Short codexModelPrice
	Long  *codexModelPrice
}

// codexModelPrices lists models whose ordinary-input, cached-read, and output
// categories match Codex rollout token_count fields. Rates are Standard USD per
// 1M tokens from OpenAI API pricing. GPT-5.6 also bills explicit cache writes at
// 1.25x input, but the rollout exposes no cache-write token category; when such
// a write occurs, this what-if estimate is therefore a lower bound. A nil Long
// means the pricing page does not list long-context rates for that model. Pro
// rows list no cached-input discount, so cached input is charged at the regular
// input rate.
var codexModelPrices = map[string]codexModelPricing{
	"gpt-5.6-sol": {
		Short: codexModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, OutputPerMTok: 30.00},
	},
	"gpt-5.6-terra": {
		Short: codexModelPrice{InputPerMTok: 2.50, CachedInputPerMTok: 0.25, OutputPerMTok: 15.00},
	},
	"gpt-5.6-luna": {
		Short: codexModelPrice{InputPerMTok: 1.00, CachedInputPerMTok: 0.10, OutputPerMTok: 6.00},
	},
	"gpt-5.5": {
		Short: codexModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, OutputPerMTok: 30.00},
		Long:  &codexModelPrice{InputPerMTok: 10.00, CachedInputPerMTok: 1.00, OutputPerMTok: 45.00},
	},
	"gpt-5.5-pro": {
		Short: codexModelPrice{InputPerMTok: 30.00, CachedInputPerMTok: 30.00, OutputPerMTok: 180.00},
		Long:  &codexModelPrice{InputPerMTok: 60.00, CachedInputPerMTok: 60.00, OutputPerMTok: 270.00},
	},
	"gpt-5.4": {
		Short: codexModelPrice{InputPerMTok: 2.50, CachedInputPerMTok: 0.25, OutputPerMTok: 15.00},
		Long:  &codexModelPrice{InputPerMTok: 5.00, CachedInputPerMTok: 0.50, OutputPerMTok: 22.50},
	},
	"gpt-5.4-mini": {
		Short: codexModelPrice{InputPerMTok: 0.75, CachedInputPerMTok: 0.075, OutputPerMTok: 4.50},
	},
	"gpt-5.4-nano": {
		Short: codexModelPrice{InputPerMTok: 0.20, CachedInputPerMTok: 0.02, OutputPerMTok: 1.25},
	},
	"gpt-5.4-pro": {
		Short: codexModelPrice{InputPerMTok: 30.00, CachedInputPerMTok: 30.00, OutputPerMTok: 180.00},
		Long:  &codexModelPrice{InputPerMTok: 60.00, CachedInputPerMTok: 60.00, OutputPerMTok: 270.00},
	},
	// Current specialized Codex row. gpt-5-codex is kept as the common Codex
	// CLI/profile alias used across tclaude even when the public pricing row
	// carries a dated minor version.
	"gpt-5.3-codex": {
		Short: codexModelPrice{InputPerMTok: 1.75, CachedInputPerMTok: 0.175, OutputPerMTok: 14.00},
	},
	"gpt-5-codex": {
		Short: codexModelPrice{InputPerMTok: 1.75, CachedInputPerMTok: 0.175, OutputPerMTok: 14.00},
	},
	// gpt-5.3-codex-spark is intentionally absent: it is a research preview
	// whose rate is not final. Unknown prices remain unestimated rather than
	// borrowing another model's rate.
}

// CodexVirtualCostFromRollout reads rolloutPath and estimates the latest
// cumulative pay-per-token cost from token_count.info.total_token_usage. The
// model is taken from modelHint when supplied, else from the rollout's latest
// turn_context model. ok is false for "not enough data" cases: no token_count,
// no known model price, or an all-zero cumulative usage block.
func CodexVirtualCostFromRollout(rolloutPath, modelHint string) (CodexTokenCost, bool, error) {
	rc, err := openCodexRollout(rolloutPath)
	if err != nil {
		return CodexTokenCost{}, false, err
	}
	defer func() { _ = rc.Close() }()

	var (
		latestModel string
		latestInfo  *codexTokenCountInfo
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
			if json.Unmarshal(env.Payload, &tc) == nil && strings.TrimSpace(tc.Model) != "" {
				latestModel = strings.TrimSpace(tc.Model)
			}
		case "event_msg":
			var ev codexTokenCountEvent
			if json.Unmarshal(env.Payload, &ev) != nil || ev.Type != "token_count" {
				return true
			}
			info := ev.Info
			latestInfo = &info
			observed = parseCodexEventTime(env.Timestamp)
		}
		return true
	})
	if err != nil {
		return CodexTokenCost{}, false, fmt.Errorf("scan codex rollout %s: %w", rolloutPath, err)
	}
	if latestInfo == nil {
		return CodexTokenCost{}, false, nil
	}
	for _, model := range []string{modelHint, latestModel} {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		cost, ok := codexVirtualCost(model, latestInfo.TotalTokenUsage, latestInfo.ModelContextWindow)
		if ok {
			return CodexTokenCost{CostUSD: cost, Model: model, Observed: observed}, true, nil
		}
	}
	return CodexTokenCost{}, false, nil
}

func codexVirtualCost(model string, usage codexTokenUsage, windowSize int64) (float64, bool) {
	pricing, ok := codexModelPrices[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	if usage.InputTokens <= 0 && usage.CachedInputTokens <= 0 && usage.OutputTokens <= 0 {
		return 0, false
	}
	price := pricing.Short
	if pricing.Long != nil && windowSize > codexShortContextWindowMax {
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

// codexShortContextWindowMax is the largest model_context_window treated as
// OpenAI's "Short context" pricing tier. Codex reports the full model context
// window in token_count.info.model_context_window; this is not an input-only
// value.
const codexShortContextWindowMax = 272_000
