package transcript

import (
	"fmt"
	"strings"
)

// Category is a kind of carried content an operator can shorten on its own.
// Reasoning and harness-injected context are not categories: they are never
// carried (provider-bound, or re-injected by the destination harness).
type Category string

const (
	// CategoryChat is user and assistant text.
	CategoryChat Category = "chat"
	// CategoryToolCalls is tool call inputs.
	CategoryToolCalls Category = "tool_calls"
	// CategoryToolOutput is tool results.
	CategoryToolOutput Category = "tool_output"
)

// Categories lists every adjustable category, in display order.
var Categories = []Category{CategoryChat, CategoryToolCalls, CategoryToolOutput}

// ValidCategory reports whether c is an adjustable category.
func ValidCategory(c Category) bool {
	for _, k := range Categories {
		if k == c {
			return true
		}
	}
	return false
}

var categoryText = map[Category]string{
	CategoryChat:       "chat messages",
	CategoryToolCalls:  "tool calls",
	CategoryToolOutput: "tool outputs",
}

// CategoryUsage is what one category weighed before and after the policy.
// Token figures use the same coarse estimate as the budget.
type CategoryUsage struct {
	Category Category `json:"category"`
	// SourceTokens is the category at full fidelity; CarriedTokens is what
	// the plan carries of it, after shortening and any budget cut.
	SourceTokens  int64 `json:"source_tokens"`
	CarriedTokens int64 `json:"carried_tokens"`
	// Shortened counts records replaced by a one-line stub that points at
	// the full transcript; ShortenedTokens is roughly what that saved.
	Shortened       int   `json:"shortened,omitempty"`
	ShortenedTokens int64 `json:"shortened_tokens,omitempty"`
	// KeepPercent is the share kept at full fidelity, newest first, when a
	// percentage applied (set by the operator or found by CutFirst). Nil is
	// the automatic rule: tool records of the last three turns, all chat.
	KeepPercent *int `json:"keep_percent,omitempty"`
}

// cutoffs is, per category, the first entry index kept at full fidelity:
// that category's records in earlier entries are shortened to stubs.
type cutoffs map[Category]int

// categoryCosts estimates each entry's per-category cost at full fidelity.
func categoryCosts(t *Transcript) []map[Category]int64 {
	out := make([]map[Category]int64, len(t.Entries))
	for i, e := range t.Entries {
		m := map[Category]int64{}
		for _, b := range e.Blocks {
			switch b.Kind {
			case BlockText:
				m[CategoryChat] += textTokens(b.Text)
			case BlockToolCall:
				m[CategoryToolCalls] += textTokens(b.Name) + textTokens(b.Text) + textTokens(string(b.Input))
			case BlockToolResult:
				m[CategoryToolOutput] += textTokens(b.Text)
			}
		}
		out[i] = m
	}
	return out
}

func textTokens(s string) int64 { return (int64(len(s))*3 + 3) / 4 }

// keepCutoff returns the first entry index of the newest entries whose cost
// in c fits within pct percent of c's total.
func keepCutoff(costs []map[Category]int64, c Category, pct int) int {
	var total int64
	for _, m := range costs {
		total += m[c]
	}
	if pct >= 100 || total == 0 {
		return 0
	}
	limit := total * int64(max(pct, 0)) / 100
	var used int64
	for i := len(costs) - 1; i >= 0; i-- {
		if used+costs[i][c] > limit {
			return i + 1
		}
		used += costs[i][c]
	}
	return 0
}

// resolveCutoffs applies explicit keep percentages over the automatic rule.
func resolveCutoffs(t *Transcript, costs []map[Category]int64, keep map[Category]int) cutoffs {
	recent := recentToolStart(t.Entries)
	cut := cutoffs{CategoryChat: 0, CategoryToolCalls: recent, CategoryToolOutput: recent}
	for c, pct := range keep {
		if ValidCategory(c) {
			cut[c] = keepCutoff(costs, c, pct)
		}
	}
	return cut
}

// chatStub shortens an older chat message to a pointer at its full text.
func chatStub(role Role, text, path string, index int) string {
	who := "assistant"
	if role == RoleUser {
		who = "user"
	}
	return fmt.Sprintf("%searlier %s message shortened · %s; full: %s#entry-%d%s", markOpen, who, shortToolText(text, 160), path, index+1, markClose)
}

// cutNote tells the receiving agent what the conversion shortened, per
// category, with rough sizes. "" when nothing was shortened or omitted.
func cutNote(r *LossReport, operator bool) string {
	var parts []string
	for _, u := range r.Categories {
		if u.Shortened == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d older %s (~%s tokens) were shortened to one-line stubs", u.Shortened, categoryText[u.Category], roundTokens(u.ShortenedTokens)))
	}
	if n := r.Count(LossTruncatedBudget); n > 0 {
		parts = append(parts, fmt.Sprintf("the %d oldest entries were omitted to fit the budget", n))
	}
	if len(parts) == 0 {
		return ""
	}
	how := "automatically"
	if operator {
		how = "on the operator's carry-over settings"
	}
	return fmt.Sprintf(" Cut during conversion, %s: %s.", how, strings.Join(parts, "; "))
}

// roundTokens renders a rough token count ("19k", "850").
func roundTokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%d", n)
}
