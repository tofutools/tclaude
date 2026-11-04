package harness

import (
	"regexp"
	"strings"
)

// AnthropicModelPrice is one Claude API pricing tier in USD per million
// tokens. CacheRead covers cache hits and refreshes.
type AnthropicModelPrice struct {
	InputPerMTok        float64
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64
	CacheReadPerMTok    float64
	OutputPerMTok       float64
}

// AnthropicModelPricing is a model's ordinary tier plus, for a model priced
// by prompt length, the tier for prompts over LongAbove input tokens (all
// input: uncached, cache reads and cache writes).
type AnthropicModelPricing struct {
	Short     AnthropicModelPrice
	Long      *AnthropicModelPrice
	LongAbove int64
}

// Sonnet 4 and 4.5 charge long-context rates for prompts over 200k tokens
// (the 1M context window); Claude 4.6 and later price the whole window at
// standard rates.
//
// anthropicModelPrices are the Claude API's standard (global routing, not
// fast mode, not batch) rates from
// https://platform.claude.com/docs/en/about-claude/pricing, keyed by model
// ID without a date suffix. Unknown models stay unpriced rather than
// borrowing another model's rate.
//
// Fable 5.5 and Mythos 5.5 are entered ahead of release with the 5.1 rates,
// an assumption until Anthropic publishes their pricing.
var anthropicModelPrices = map[string]AnthropicModelPricing{
	"claude-fable-5-5":  {Short: AnthropicModelPrice{10, 12.50, 20, 0.25, 50}},
	"claude-mythos-5-5": {Short: AnthropicModelPrice{10, 12.50, 20, 0.25, 50}},
	"claude-fable-5-1":  {Short: AnthropicModelPrice{10, 12.50, 20, 0.25, 50}},
	"claude-mythos-5-1": {Short: AnthropicModelPrice{10, 12.50, 20, 0.25, 50}},
	"claude-fable-5":    {Short: AnthropicModelPrice{10, 12.50, 20, 1, 50}},
	"claude-mythos-5":   {Short: AnthropicModelPrice{10, 12.50, 20, 1, 50}},
	"claude-opus-5-5":   {Short: AnthropicModelPrice{4, 5, 8, 0.20, 20}},
	"claude-opus-5":     {Short: AnthropicModelPrice{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-8":   {Short: AnthropicModelPrice{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-7":   {Short: AnthropicModelPrice{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-6":   {Short: AnthropicModelPrice{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-5":   {Short: AnthropicModelPrice{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-1":   {Short: AnthropicModelPrice{15, 18.75, 30, 1.50, 75}},
	"claude-opus-4":     {Short: AnthropicModelPrice{15, 18.75, 30, 1.50, 75}},
	"claude-sonnet-5-5": {Short: AnthropicModelPrice{2, 2.50, 4, 0.10, 10}},
	"claude-sonnet-5":   {Short: AnthropicModelPrice{2, 2.50, 4, 0.20, 10}},
	"claude-sonnet-4-6": {Short: AnthropicModelPrice{3, 3.75, 6, 0.30, 15}},
	"claude-sonnet-4-5": {Short: AnthropicModelPrice{3, 3.75, 6, 0.30, 15}, Long: &AnthropicModelPrice{6, 7.50, 12, 0.60, 22.50}, LongAbove: 200000},
	"claude-sonnet-4":   {Short: AnthropicModelPrice{3, 3.75, 6, 0.30, 15}, Long: &AnthropicModelPrice{6, 7.50, 12, 0.60, 22.50}, LongAbove: 200000},
	"claude-haiku-5-5": {
		Short:     AnthropicModelPrice{0.10, 0.125, 0.20, 0.01, 0.50},
		Long:      &AnthropicModelPrice{0.50, 0.625, 1, 0.05, 2.50},
		LongAbove: 100000,
	},
	"claude-haiku-4-5":  {Short: AnthropicModelPrice{1, 1.25, 2, 0.10, 5}},
	"claude-3-5-haiku":  {Short: AnthropicModelPrice{0.80, 1, 1.60, 0.08, 4}},
	"claude-haiku-3-5":  {Short: AnthropicModelPrice{0.80, 1, 1.60, 0.08, 4}},
	"claude-3-7-sonnet": {Short: AnthropicModelPrice{3, 3.75, 6, 0.30, 15}},
	"claude-3-5-sonnet": {Short: AnthropicModelPrice{3, 3.75, 6, 0.30, 15}},
	"claude-3-opus":     {Short: AnthropicModelPrice{15, 18.75, 30, 1.50, 75}},
	"claude-3-haiku":    {Short: AnthropicModelPrice{0.25, 0.30, 0.50, 0.03, 1.25}},
}

var anthropicDateSuffix = regexp.MustCompile(`-\d{8}$`)

// LookupAnthropicModelPricing prices a Claude model ID, with or without its
// date suffix (claude-haiku-4-5-20251001) or a context tag ([1m]).
func LookupAnthropicModelPricing(model string) (AnthropicModelPricing, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(id, '['); i >= 0 {
		id = id[:i]
	}
	id = anthropicDateSuffix.ReplaceAllString(id, "")
	pricing, ok := anthropicModelPrices[id]
	return pricing, ok && pricing.Short.OutputPerMTok > 0
}
