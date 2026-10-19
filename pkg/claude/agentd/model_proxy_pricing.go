package agentd

import (
	"math"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// modelRate is a price tier in USD per million tokens, which is the same
// number as micro-USD per token.
type modelRate struct{ input, output, cacheRead, cacheWrite, cacheWrite1h float64 }

// modelPricing is a model's ordinary tier and, for a model priced by prompt
// length, the tier above longAbove prompt tokens.
type modelPricing struct {
	short     modelRate
	long      *modelRate
	longAbove int64
}

// lookupModelPricing finds a gateway model's price: the gateway's own
// prices first (exact ID, then the longest trailing-* prefix), then the
// built-in API rates for its dialect.
func lookupModelPricing(p *config.ModelProxyPolicy, model string) (modelPricing, bool) {
	if p != nil {
		if r, ok := p.Prices[model]; ok {
			return configuredPricing(r)
		}
		best := ""
		for pattern := range p.Prices {
			prefix, ok := strings.CutSuffix(pattern, "*")
			if ok && strings.HasPrefix(model, prefix) && len(prefix) >= len(best) {
				best = pattern
			}
		}
		if best != "" {
			return configuredPricing(p.Prices[best])
		}
	}
	if p != nil && p.Dialect == "openai" {
		o, ok := harness.LookupOpenAIModelPricing(model)
		if !ok {
			return modelPricing{}, false
		}
		out := modelPricing{short: openAIRate(o.Short)}
		if o.Long != nil {
			long := openAIRate(*o.Long)
			out.long, out.longAbove = &long, harness.OpenAIShortContextInputMax
		}
		return out, true
	}
	a, ok := harness.LookupAnthropicModelPricing(model)
	if !ok {
		return modelPricing{}, false
	}
	out := modelPricing{short: anthropicRate(a.Short)}
	if a.Long != nil {
		long := anthropicRate(*a.Long)
		out.long, out.longAbove = &long, a.LongAbove
	}
	return out, true
}

func configuredPricing(r config.ModelProxyPrice) (modelPricing, bool) {
	if !(r.Input > 0 && r.Output > 0) {
		return modelPricing{}, false
	}
	or := func(v, fallback float64) float64 {
		if v > 0 {
			return v
		}
		return fallback
	}
	write := or(r.CacheWrite, r.Input)
	return modelPricing{short: modelRate{input: r.Input, output: r.Output, cacheRead: or(r.CacheRead, r.Input), cacheWrite: write, cacheWrite1h: or(r.CacheWrite1h, write)}}, true
}

func anthropicRate(p harness.AnthropicModelPrice) modelRate {
	return modelRate{input: p.InputPerMTok, output: p.OutputPerMTok, cacheRead: p.CacheReadPerMTok, cacheWrite: p.CacheWrite5mPerMTok, cacheWrite1h: p.CacheWrite1hPerMTok}
}

func openAIRate(p harness.OpenAIModelPrice) modelRate {
	write := p.CacheWritePerMTok
	if write == 0 {
		write = p.InputPerMTok
	}
	return modelRate{input: p.InputPerMTok, output: p.OutputPerMTok, cacheRead: p.CachedInputPerMTok, cacheWrite: write, cacheWrite1h: write}
}

// tier is the rate for a request with this many prompt tokens.
func (m modelPricing) tier(prompt int64) modelRate {
	if m.long != nil && prompt > m.longAbove {
		return *m.long
	}
	return m.short
}

// costMicroUSD prices observed usage, rounding up to whole micro-dollars.
func (m modelPricing) costMicroUSD(u db.ModelProxyUsage) int64 {
	r := m.tier(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens)
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	cost := float64(u.InputTokens)*r.input + float64(u.OutputTokens)*r.output + float64(u.CacheReadTokens)*r.cacheRead +
		float64(u.CacheWriteTokens-write1h)*r.cacheWrite + float64(write1h)*r.cacheWrite1h
	return int64(math.Ceil(cost))
}

// worstCaseMicroUSD bounds a request before it runs: every allowed prompt
// token at the dearest input rate of the dearest tier, plus every allowed
// output token.
func (m modelPricing) worstCaseMicroUSD(maxInput, maxOutput int64) int64 {
	r := m.tier(maxInput)
	input := max(r.input, r.cacheWrite, r.cacheWrite1h)
	return int64(math.Ceil(float64(maxInput)*input + float64(maxOutput)*r.output))
}
