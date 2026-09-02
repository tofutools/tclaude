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
	"github.com/tofutools/tclaude/pkg/claude/common/money"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
)

// tuiUsageInterval is how often the console re-reads the account's usage
// figures. Deliberately far slower than the 2s listing poll: these are
// rolling-window percentages fed by Claude Code's statusline callback and a
// cached Anthropic reading, so they move at minutes, and polling them on every
// tick would spend a DB read and a cost-history walk to redraw the same bar.
const tuiUsageInterval = 30 * time.Second

// tuiUsageUnsupportedInterval is how often a console keeps checking a daemon
// that answered "no such endpoint" — a standalone console pointed at an older
// tclaude. Giving up permanently would leave the readout dark for the rest of
// the console's life even after the far end is upgraded and restarted under it,
// which is precisely what a long-lived remote console does; a 404 every ten
// minutes costs nothing and brings the line back on its own.
const tuiUsageUnsupportedInterval = 10 * time.Minute

// tuiUsageBarWidth is how many cells one rolling-limit bar spends. It matches
// the web dashboard's USAGE_BAR_WIDTH so the same reading looks the same in
// both surfaces.
const tuiUsageBarWidth = 8

// tuiUsageNarrowBarWidth is the bar a crowded line falls back to. Four cells
// still read as a proportion, and halving them buys back the width two
// accounts' worth of windows need on an 80-column terminal.
const tuiUsageNarrowBarWidth = 4

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
// in flight, and no more often than tuiUsageInterval — or, against a daemon
// that has no usage endpoint, tuiUsageUnsupportedInterval.
func (m tuiModel) usageDue() bool {
	if !m.operator || m.usageFetching {
		return false
	}
	if m.lastUsageAttempt.IsZero() {
		return true
	}
	interval := tuiUsageInterval
	if m.usageUnsupported {
		interval = tuiUsageUnsupportedInterval
	}
	return time.Since(m.lastUsageAttempt) >= interval
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
func tuiUsageBar(pct float64, width int) string {
	pct = min(max(pct, 0), 100)
	styles := tuiUsageStyles()
	filled := int(math.Round(pct / 100 * float64(width)))
	fill := styles.low
	switch {
	case pct >= 80:
		fill = styles.high
	case pct >= 60:
		fill = styles.mid
	}
	return fill.Render(strings.Repeat("█", filled)) +
		styles.empty.Render(strings.Repeat("░", width-filled))
}

// tuiUsageDetail is how much width one pass spends on each window. The line
// tries these in order and keeps the first rendering that fits, so a crowded
// terminal gives up ornament — the reset timer, then half the bar, then the
// bar — before it gives up a whole field. Four windows (both accounts' 5h and
// 7d) is the reading this ordering is built to keep whole.
type tuiUsageDetail struct {
	// bar is the bar's width in cells; zero draws no bar at all.
	bar int
	// remaining is whether the window's reset timer is shown.
	remaining bool
}

var tuiUsageDetails = []tuiUsageDetail{
	{bar: tuiUsageBarWidth, remaining: true},
	{bar: tuiUsageBarWidth},
	{bar: tuiUsageNarrowBarWidth},
	{},
}

// tuiUsageSegment is one field of the line. A segment with a window renders at
// whatever detail the line can afford; one with only text (API spend, or a
// one-word status) is the same width at every detail.
type tuiUsageSegment struct {
	text   string
	label  string
	window *tuiUsageWindow
}

// render draws one field: "claude 5h ███░░░░░ 42% (3h41m)" at full detail,
// down to "claude 5h 42%" at the plainest. The reset timer is dropped when the
// daemon reported none.
func (s tuiUsageSegment) render(d tuiUsageDetail) string {
	if s.window == nil {
		return s.text
	}
	out := s.label
	if d.bar > 0 {
		out += " " + tuiUsageBar(s.window.Pct, d.bar)
	}
	// math.Round, not %.0f: Go rounds a half to even and the dashboard's
	// Math.round rounds it away from zero, so 62.5% would read 62% here and
	// 63% there off the very same payload.
	out += fmt.Sprintf(" %.0f%%", math.Round(s.window.Pct))
	if d.remaining && s.window.Remaining != "" {
		out += " (" + s.window.Remaining + ")"
	}
	return out
}

// tuiUsageWindowSegments turns an account's two windows into fields, prefixing
// each label with who it belongs to when the readout names more than one
// account. A window the source did not report is skipped rather than shown as
// a 0% bar it cannot stand behind.
func tuiUsageWindowSegments(u *tuiSubscriptionUsage, prefix string) []tuiUsageSegment {
	if u == nil || !u.Available {
		return nil
	}
	var out []tuiUsageSegment
	if u.FiveHour != nil {
		out = append(out, tuiUsageSegment{label: prefix + "5h", window: u.FiveHour})
	}
	if u.SevenDay != nil {
		out = append(out, tuiUsageSegment{label: prefix + "7d", window: u.SevenDay})
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

// tuiUsageMoney formats a dollar figure the way the dashboard does: thousands
// separated, to the cent below $100 and whole above it, and anything that
// would round to $0.00 as "<1¢" rather than reading as free.
func tuiUsageMoney(usd float64) string {
	return money.USD(usd)
}

// usageLine is the console's status line: the account's subscription limits
// and API spend, in the shape Claude Code's own status line shows them. It
// carries no "usage" label of its own — the bars and window names say what it
// is, and the width a label would take is width the second account's windows
// need.
//
// It says nothing rather than guessing. A console the daemon does not treat as
// the operator never polls the readout (the identity warning above the listing
// already explains why), and one whose polls have all failed shows that plainly
// instead of leaving a blank where figures belong. Those wordings keep the
// "usage" word, because "unavailable" on its own names nothing.
//
// The line is budgeted as exactly one row — renderList's chrome accounting
// depends on it — so it is drawn at the most detail that fits, and only then
// are whole fields dropped from the right; a terminal too narrow for even the
// first one gets no line at all.
func (m tuiModel) usageLine() string {
	if !m.operator || m.usageUnsupported {
		return ""
	}
	if !m.usageLoaded {
		if m.usageFailed {
			return m.fitUsageLine(tuiUsageStatus("usage unavailable"))
		}
		return ""
	}
	segments := tuiUsageWindowSegments(&m.usage.tuiSubscriptionUsage, "")
	if codex := tuiUsageWindowSegments(m.usage.Codex, "codex "); len(codex) > 0 {
		// Two accounts on one line: name both, so a bar cannot be read as the
		// wrong one's.
		for i := range segments {
			segments[i].label = "claude " + segments[i].label
		}
		segments = append(segments, codex...)
	}
	if cost := tuiUsageCost(m.usage); cost != "" {
		segments = append(segments, tuiUsageSegment{text: cost})
	}
	if len(segments) == 0 {
		// The dashboard's own wording for a readout with nothing in it.
		return m.fitUsageLine(tuiUsageStatus("usage n/a"))
	}
	return m.fitUsageLine(segments)
}

// tuiUsageStatus is the line reduced to one word about itself, for the states
// that have no figures to draw.
func tuiUsageStatus(text string) []tuiUsageSegment {
	return []tuiUsageSegment{{text: text}}
}

// fitUsageLine assembles the line and trims it to the terminal: first by
// spending less width per field (see tuiUsageDetails), and only when even the
// plainest rendering is too wide by dropping whole fields from the right,
// rather than cutting a bar in half.
func (m tuiModel) fitUsageLine(segments []tuiUsageSegment) string {
	fits := func(line string) bool {
		const indent = 2
		return m.width <= 0 || lipgloss.Width(line)+indent <= m.width
	}
	render := func(d tuiUsageDetail, n int) string {
		parts := make([]string, 0, n)
		for _, s := range segments[:n] {
			parts = append(parts, s.render(d))
		}
		return strings.Join(parts, " • ")
	}
	for _, d := range tuiUsageDetails {
		if line := render(d, len(segments)); fits(line) {
			return line
		}
	}
	plainest := tuiUsageDetails[len(tuiUsageDetails)-1]
	for n := len(segments) - 1; n > 0; n-- {
		if line := render(plainest, n); fits(line) {
			return line
		}
	}
	return ""
}
