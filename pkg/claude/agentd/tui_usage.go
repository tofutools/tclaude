package agentd

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
)

// tuiUsageInterval is how often the console re-reads the account's usage
// figures. Deliberately far slower than the 2s listing poll: these are
// rolling-window percentages fed by Claude Code's statusline callback and a
// cached Anthropic reading, so they move at minutes, and polling them on every
// tick would spend a DB read and a cost-history walk to redraw the same bar.
const tuiUsageInterval = 30 * time.Second

// tuiUsageBarWidth is how many cells one rolling-limit bar spends. It matches
// the web dashboard's USAGE_BAR_WIDTH so the same reading looks the same in
// both surfaces.
const tuiUsageBarWidth = 8

// ---- wire shapes -----------------------------------------------------------

// tuiUsage is the subset of /v1/usage the console renders — the same payload
// the web dashboard's top bar is built from, decoded down to the fields this
// one line shows.
type tuiUsage struct {
	tuiSubscriptionUsage
	// TotalCostUSD is month-to-date API spend, TodayCostUSD the part of it
	// spent today. Both stay 0 on a subscription account, which is what makes
	// them safe to render only when nonzero.
	TotalCostUSD float64 `json:"total_cost_usd"`
	TodayCostUSD float64 `json:"today_cost_usd"`
	// Codex is the Codex account's own rolling limits, absent when Codex is
	// not installed or has reported nothing recent.
	Codex *tuiSubscriptionUsage `json:"codex"`
}

// tuiSubscriptionUsage is one account's rolling rate-limit windows. Available
// false means the daemon had no usable reading — a cache that never filled, an
// API-billing account with no rolling limits, or figures gone stale.
type tuiSubscriptionUsage struct {
	Available bool            `json:"available"`
	FiveHour  *tuiUsageWindow `json:"five_hour"`
	SevenDay  *tuiUsageWindow `json:"seven_day"`
}

// tuiUsageWindow is one bucket: percent consumed, plus the daemon's
// pre-formatted time until it resets ("3h41m", "2d9h", "reset", or "").
type tuiUsageWindow struct {
	Pct       float64 `json:"pct"`
	Remaining string  `json:"remaining"`
}

// ---- polling ---------------------------------------------------------------

// usageDue reports whether this tick should start a usage poll: only for a
// console the daemon treats as the operator (the endpoint is human-only, so
// any other console would just collect refusals), never while one is already
// in flight, and no more often than tuiUsageInterval.
func (m tuiModel) usageDue() bool {
	if !m.operator || m.usageFetching {
		return false
	}
	return m.lastUsageAttempt.IsZero() || time.Since(m.lastUsageAttempt) >= tuiUsageInterval
}

// usageCmd reads the account's usage figures off the daemon. Like the listing
// poll it runs as a bubbletea command: the daemon answers it from SQLite (never
// the network), but the console must not block on that read.
func (m tuiModel) usageCmd() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		var u tuiUsage
		if err := api.get("/v1/usage", &u); err != nil {
			return tuiUsageMsg{err: err}
		}
		return tuiUsageMsg{usage: u}
	}
}

// ---- rendering -------------------------------------------------------------

// tuiUsageBarStyles are the colors one usage bar draws with, taken from the
// same palette as tclaude's other watch TUIs so a green bar here is the green
// used everywhere else.
type tuiUsageBarStyles struct {
	low, mid, high, empty lipgloss.Style
}

// tuiUsageStyles resolves those styles once per process. The color scheme
// (config tui.color_scheme) cannot change under a running console, and this
// line is redrawn every two seconds, so the config read happens on the first
// render and never again.
var tuiUsageStyles = sync.OnceValue(func() tuiUsageBarStyles {
	// Load failures are not worth a degraded readout: TUIColorScheme is
	// nil-safe and falls back to the default scheme.
	cfg, _ := config.Load()
	p := tuistyle.Resolve(cfg.TUIColorScheme())
	return tuiUsageBarStyles{
		low:   lipgloss.NewStyle().Foreground(lipgloss.Color(p.Working)),
		mid:   lipgloss.NewStyle().Foreground(lipgloss.Color(p.Idle)),
		high:  lipgloss.NewStyle().Foreground(lipgloss.Color(p.Danger)),
		empty: lipgloss.NewStyle().Foreground(lipgloss.Color(p.Help)),
	}
})

// tuiUsageBar renders one rolling limit as a filled/empty bar, colored on the
// statusbar's thresholds: green under 60%, amber from 60, red from 80.
func tuiUsageBar(pct float64) string {
	pct = min(max(pct, 0), 100)
	styles := tuiUsageStyles()
	filled := int(math.Round(pct / 100 * tuiUsageBarWidth))
	fill := styles.low
	switch {
	case pct >= 80:
		fill = styles.high
	case pct >= 60:
		fill = styles.mid
	}
	return fill.Render(strings.Repeat("█", filled)) +
		styles.empty.Render(strings.Repeat("░", tuiUsageBarWidth-filled))
}

// tuiUsageWindowText renders one labelled window: "5h ███░░░░░ 42% (3h41m)".
// The reset timer is dropped when the daemon reported none.
func tuiUsageWindowText(label string, w *tuiUsageWindow) string {
	if w == nil {
		return ""
	}
	out := fmt.Sprintf("%s %s %.0f%%", label, tuiUsageBar(w.Pct), w.Pct)
	if w.Remaining != "" {
		out += " (" + w.Remaining + ")"
	}
	return out
}

// tuiUsageWindowTexts renders an account's two windows, prefixing each with
// who it belongs to when the readout names more than one account. A window the
// source did not report is skipped rather than shown as a 0% bar it cannot
// stand behind.
func tuiUsageWindowTexts(u *tuiSubscriptionUsage, prefix string) []string {
	if u == nil || !u.Available {
		return nil
	}
	var out []string
	if s := tuiUsageWindowText(prefix+"5h", u.FiveHour); s != "" {
		out = append(out, s)
	}
	if s := tuiUsageWindowText(prefix+"7d", u.SevenDay); s != "" {
		out = append(out, s)
	}
	return out
}

// tuiUsageCost renders month-to-date API spend, with today's share beside it —
// "api $12.34 mtd ($0.42 today)". Empty for a subscription account, whose
// sessions record no per-token cost at all.
func tuiUsageCost(u tuiUsage) string {
	if u.TotalCostUSD <= 0 {
		return ""
	}
	out := "api " + tuiUsageMoney(u.TotalCostUSD) + " mtd"
	if u.TodayCostUSD > 0 {
		out += " (" + tuiUsageMoney(u.TodayCostUSD) + " today)"
	}
	return out
}

// tuiUsageMoney formats a dollar figure the way the dashboard does: real spend
// to the cent, and anything that would round to $0.00 as "<1¢" rather than
// reading as free.
func tuiUsageMoney(usd float64) string {
	if usd < 0.005 {
		return "<1¢"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// usageLine is the console's status line: the account's subscription limits
// and API spend, in the shape Claude Code's own status line shows them.
//
// It says nothing rather than guessing. A console the daemon does not treat as
// the operator never polls the readout (the identity warning above the listing
// already explains why), and one whose polls have all failed shows that plainly
// instead of leaving a blank where figures belong.
//
// The line is budgeted as exactly one row — renderList's chrome accounting
// depends on it — so segments are dropped from the right until it fits, and a
// terminal too narrow for even the first one gets no line at all.
func (m tuiModel) usageLine() string {
	if !m.operator {
		return ""
	}
	if !m.usageLoaded {
		if m.usageFailed {
			return m.fitUsageLine([]string{"unavailable"})
		}
		return ""
	}
	segments := tuiUsageWindowTexts(&m.usage.tuiSubscriptionUsage, "")
	if codex := tuiUsageWindowTexts(m.usage.Codex, "codex "); len(codex) > 0 {
		// Two accounts on one line: name both, so a bar cannot be read as the
		// wrong one's.
		for i := range segments {
			segments[i] = "claude " + segments[i]
		}
		segments = append(segments, codex...)
	}
	if cost := tuiUsageCost(m.usage); cost != "" {
		segments = append(segments, cost)
	}
	if len(segments) == 0 {
		// The dashboard's own wording for a readout with nothing in it.
		return m.fitUsageLine([]string{"n/a"})
	}
	return m.fitUsageLine(segments)
}

// fitUsageLine assembles the labelled line and trims it to the terminal,
// dropping whole segments from the right rather than cutting a bar in half.
func (m tuiModel) fitUsageLine(segments []string) string {
	const label = "usage  "
	const indent = 2
	for n := len(segments); n > 0; n-- {
		line := label + strings.Join(segments[:n], " • ")
		if m.width <= 0 || lipgloss.Width(line)+indent <= m.width {
			return line
		}
	}
	return ""
}
