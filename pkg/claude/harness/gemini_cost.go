package harness

import (
	"strings"
	"time"
)

// Gemini CLI WHAT-IF cost: what a conversation's model calls would cost at
// Gemini API pay-per-token rates. Gemini writes no price of its own, so this
// prices each call's recorded TokensSummary (see gemini_usage.go) against the
// table below. Like Codex's projection it is hypothetical — a Google sign-in
// or a Code Assist licence is not billed per token — and the dashboard shows
// it only behind the cost.show_on_subscription opt-in.

// GeminiModelPrice is one Gemini API pricing tier in USD per 1M tokens.
type GeminiModelPrice struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	OutputPerMTok      float64
}

// GeminiModelPricing holds the ordinary tier and, for models Google prices by
// prompt size, the tier for prompts over geminiLongContextThreshold.
type GeminiModelPricing struct {
	Short GeminiModelPrice
	Long  *GeminiModelPrice
}

// geminiLongContextThreshold is the prompt size above which Google bills a
// whole request at the Long tier.
const geminiLongContextThreshold = 200_000

// geminiModelPrices is the Gemini API's Standard paid-tier text pricing
// (ai.google.dev/gemini-api/docs/pricing, as updated 2026-10-01). Output
// includes thinking tokens. The 3.6–3.8 Flash rates are the ones in force now;
// Google has announced they double in January 2027. Models the page does not
// list (older previews such as gemini-3-pro-preview and gemini-3-flash-preview)
// are deliberately absent: an unknown price stays unestimated rather than
// borrowing another model's rate.
var geminiModelPrices = map[string]GeminiModelPricing{
	"gemini-3.8-flash":      {Short: GeminiModelPrice{InputPerMTok: 0.75, CachedInputPerMTok: 0.075, OutputPerMTok: 3.75}},
	"gemini-3.7-flash":      {Short: GeminiModelPrice{InputPerMTok: 0.75, CachedInputPerMTok: 0.075, OutputPerMTok: 3.75}},
	"gemini-3.6-flash":      {Short: GeminiModelPrice{InputPerMTok: 0.75, CachedInputPerMTok: 0.075, OutputPerMTok: 3.75}},
	"gemini-3.5-flash":      {Short: GeminiModelPrice{InputPerMTok: 1.50, CachedInputPerMTok: 0.15, OutputPerMTok: 9.00}},
	"gemini-3.5-flash-lite": {Short: GeminiModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.03, OutputPerMTok: 2.50}},
	"gemini-3.1-flash-lite": {Short: GeminiModelPrice{InputPerMTok: 0.25, CachedInputPerMTok: 0.025, OutputPerMTok: 1.50}},
	"gemini-3.1-pro-preview": {
		Short: GeminiModelPrice{InputPerMTok: 2.00, CachedInputPerMTok: 0.20, OutputPerMTok: 12.00},
		Long:  &GeminiModelPrice{InputPerMTok: 4.00, CachedInputPerMTok: 0.40, OutputPerMTok: 18.00},
	},
	"gemini-2.5-pro": {
		Short: GeminiModelPrice{InputPerMTok: 1.25, CachedInputPerMTok: 0.125, OutputPerMTok: 10.00},
		Long:  &GeminiModelPrice{InputPerMTok: 2.50, CachedInputPerMTok: 0.25, OutputPerMTok: 15.00},
	},
	"gemini-2.5-flash":      {Short: GeminiModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.03, OutputPerMTok: 2.50}},
	"gemini-2.5-flash-lite": {Short: GeminiModelPrice{InputPerMTok: 0.10, CachedInputPerMTok: 0.01, OutputPerMTok: 0.40}},
}

// LookupGeminiModelPricing returns the rate card for a concrete Gemini model.
func LookupGeminiModelPricing(model string) (GeminiModelPricing, bool) {
	pricing, ok := geminiModelPrices[strings.TrimSpace(model)]
	return pricing, ok
}

// geminiCallCostUSD prices one model call. Gemini's `input` is the request's
// promptTokenCount, which INCLUDES the cached tokens, and `tool` is the
// separately counted tool-use prompt, billed as input. Thinking is billed as
// output. ok is false for an unpriced model.
func geminiCallCostUSD(model string, tokens geminiTokens) (float64, bool) {
	pricing, ok := LookupGeminiModelPricing(model)
	if !ok {
		return 0, false
	}
	input := max(tokens.Input, 0)
	cached := min(max(tokens.Cached, 0), input)
	tool := max(tokens.Tool, 0)
	output := max(tokens.Output, 0) + max(tokens.Thoughts, 0)
	price := pricing.Short
	if pricing.Long != nil && input+tool > geminiLongContextThreshold {
		price = *pricing.Long
	}
	usd := float64(input-cached+tool)*price.InputPerMTok +
		float64(cached)*price.CachedInputPerMTok +
		float64(output)*price.OutputPerMTok
	return usd / 1_000_000, true
}

// GeminiCostDay is the cumulative WHAT-IF cost through the end of one local
// calendar day, in the shape db.VirtualCostDailySnapshot persists.
type GeminiCostDay struct {
	Day      string
	CostUSD  float64
	Observed time.Time
	Model    string
}

// geminiBilledCall is one model call as recorded, kept for pricing even after
// a rewind or a compression drops it from the conversation: the call was made
// whether or not its message survives.
type geminiBilledCall struct {
	model     string
	tokens    geminiTokens
	timestamp time.Time
}

// geminiCostDayFormat matches db's session_cost_daily day key.
const geminiCostDayFormat = "2006-01-02"

// geminiCostHistory prices calls in record order and returns the conversation
// total plus one cumulative row per local day that had a priced call. A call
// without a timestamp is attributed to the latest timestamp seen before it, or
// to now when none was. Unpriced calls add nothing.
func geminiCostHistory(calls []geminiBilledCall, now time.Time) (float64, []GeminiCostDay) {
	var (
		total   float64
		history []GeminiCostDay
		last    time.Time
	)
	for _, call := range calls {
		at := call.timestamp
		if at.IsZero() {
			at = last
		}
		if at.IsZero() {
			at = now
		}
		last = at
		usd, ok := geminiCallCostUSD(call.model, call.tokens)
		if !ok || usd <= 0 {
			continue
		}
		total += usd
		day := at.Local().Format(geminiCostDayFormat)
		if n := len(history); n > 0 && day < history[n-1].Day {
			// A clock step backwards: keep the history cumulative and ordered.
			day = history[n-1].Day
		}
		if n := len(history); n > 0 && history[n-1].Day == day {
			history[n-1].CostUSD = total
			if at.After(history[n-1].Observed) {
				history[n-1].Observed = at
			}
			history[n-1].Model = call.model
			continue
		}
		history = append(history, GeminiCostDay{Day: day, CostUSD: total, Observed: at, Model: call.model})
	}
	return total, history
}
