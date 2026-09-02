package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errUsagePoll stands in for whatever stopped a usage read — the console
// treats every failure the same way.
var errUsagePoll = errors.New("connection refused")

// loadedUsageModel is a console that has already read one usage payload, as it
// would two seconds after startup. Width is the console's usual test width, so
// nothing is trimmed unless the test asks for a narrow terminal.
func loadedUsageModel(u tuiUsage) tuiModel {
	m := newTUIModel(nil)
	m.operator = true
	m.width = 140
	m.height = 30
	m.usage = u
	m.usageLoaded = true
	return m
}

func claudeUsage() tuiUsage {
	return tuiUsage{tuiSubscriptionUsage: tuiSubscriptionUsage{
		Available: true,
		FiveHour:  &tuiUsageWindow{Pct: 42, Remaining: "3h41m"},
		SevenDay:  &tuiUsageWindow{Pct: 18, Remaining: "2d9h"},
	}}
}

func TestTUIUsageLineShowsBothRollingWindows(t *testing.T) {
	line := loadedUsageModel(claudeUsage()).usageLine()

	assert.Contains(t, line, "5h")
	assert.Contains(t, line, "42%")
	assert.Contains(t, line, "(3h41m)")
	assert.Contains(t, line, "7d")
	assert.Contains(t, line, "18%")
	assert.Contains(t, line, "(2d9h)")
	assert.Contains(t, line, "█", "the percentage is drawn as a bar, as the status line does")
	assert.Contains(t, line, "░")
}

// A window the daemon reported without a reset time (the weekly bucket often
// arrives that way) still shows its percentage — it just says nothing about
// when it resets.
func TestTUIUsageLineOmitsAMissingResetTimer(t *testing.T) {
	line := loadedUsageModel(tuiUsage{tuiSubscriptionUsage: tuiSubscriptionUsage{
		Available: true,
		SevenDay:  &tuiUsageWindow{Pct: 18},
	}}).usageLine()

	assert.Contains(t, line, "7d")
	assert.Contains(t, line, "18%")
	assert.NotContains(t, line, "(")
	assert.NotContains(t, line, "5h", "a window the daemon did not report is left out")
}

func TestTUIUsageLineNamesBothAccountsWhenCodexIsPresent(t *testing.T) {
	u := claudeUsage()
	u.Codex = &tuiSubscriptionUsage{
		Available: true,
		FiveHour:  &tuiUsageWindow{Pct: 7, Remaining: "1h2m"},
	}
	line := loadedUsageModel(u).usageLine()

	assert.Contains(t, line, "claude 5h")
	assert.Contains(t, line, "codex 5h")
	assert.Contains(t, line, "7%")
}

// Without a second account there is nothing to disambiguate, so the bars stay
// unlabelled — the shape Claude Code's own status line shows them in.
func TestTUIUsageLineLeavesASingleAccountUnlabelled(t *testing.T) {
	line := loadedUsageModel(claudeUsage()).usageLine()
	assert.NotContains(t, line, "claude 5h")
	assert.Contains(t, line, "5h")
}

func TestTUIUsageLineShowsAPISpend(t *testing.T) {
	u := claudeUsage()
	u.TotalCostUSD = 12.34
	u.TodayCostUSD = 0.42
	assert.Contains(t, loadedUsageModel(u).usageLine(), "api $12.34 mtd ($0.42 today)")

	// Nothing spent today: the month-to-date figure stands alone.
	u.TodayCostUSD = 0
	assert.Contains(t, loadedUsageModel(u).usageLine(), "api $12.34 mtd")
	assert.NotContains(t, loadedUsageModel(u).usageLine(), "today")

	// A subscription account records no per-token cost at all.
	assert.NotContains(t, loadedUsageModel(claudeUsage()).usageLine(), "api ")
}

// Real spend that would round to $0.00 must not read as free.
func TestTUIUsageLineDoesNotRoundSubCentSpendToZero(t *testing.T) {
	u := claudeUsage()
	u.TotalCostUSD = 0.001
	assert.Contains(t, loadedUsageModel(u).usageLine(), "api <1¢ mtd")
}

// An account the daemon has no rolling limits for — an API-billing account, or
// a cache that has gone stale — gets the dashboard's own wording rather than a
// blank where the figures belong.
func TestTUIUsageLineSaysNAWhenTheDaemonHasNoFigures(t *testing.T) {
	assert.Equal(t, "usage n/a", loadedUsageModel(tuiUsage{}).usageLine())
}

// The figures speak for themselves, so the line spends no width on a label —
// that width belongs to the second account's windows.
func TestTUIUsageLineCarriesNoLabel(t *testing.T) {
	line := loadedUsageModel(claudeUsage()).usageLine()
	assert.NotContains(t, line, "usage")
	assert.True(t, strings.HasPrefix(line, "5h "), line)
}

// The point of dropping the label: both accounts' 5h and 7d windows are four
// fields, and all four must survive on an ordinary terminal.
func TestTUIUsageLineFitsFourWindowsOnOneRow(t *testing.T) {
	u := bothAccountsUsage()
	u.TotalCostUSD = 12.34

	for _, width := range []int{200, 140, 120, 100, 80, 60} {
		m := loadedUsageModel(u)
		m.width = width
		line := m.usageLine()
		for _, want := range []string{"claude 5h", "claude 7d", "codex 5h", "codex 7d"} {
			assert.Contains(t, line, want, "width %d", width)
		}
		assert.Contains(t, line, "42%", "width %d", width)
		assert.Contains(t, line, "63%", "width %d", width)
		assert.LessOrEqual(t, lipgloss.Width(line)+2, width)
	}
}

// bothAccountsUsage is a readout with all four windows in it — the reading the
// line is laid out around.
func bothAccountsUsage() tuiUsage {
	u := claudeUsage()
	u.Codex = &tuiSubscriptionUsage{
		Available: true,
		FiveHour:  &tuiUsageWindow{Pct: 7, Remaining: "1h2m"},
		SevenDay:  &tuiUsageWindow{Pct: 63, Remaining: "5d1h"},
	}
	return u
}

// usageBarCells counts one field's bar in cells, filled and empty together, so
// a test can tell a full bar from the half-width one without decoding the
// color escapes lipgloss wraps them in.
func usageBarCells(field string) int {
	return strings.Count(field, "█") + strings.Count(field, "░")
}

// usageFirstField is the leftmost field of a rendered line.
func usageFirstField(line string) string {
	return strings.Split(line, " • ")[0]
}

// Ornament is what a crowded line gives up first: the reset timers, then half
// the bar, then the bar, then the spend field, and only last a rolling window.
// The order is the contract here, not the widths it happens at.
func TestTUIUsageLineTrimsOrnamentBeforeFields(t *testing.T) {
	u := bothAccountsUsage()
	u.TotalCostUSD = 12.34

	// The widest terminal at which the line has already given something up.
	// Each degradation is monotone in width, so walking down finds it.
	givenUpAt := func(gone func(line string) bool) int {
		for width := 200; width >= 20; width-- {
			m := loadedUsageModel(u)
			m.width = width
			if line := m.usageLine(); line != "" && gone(line) {
				return width
			}
		}
		return 0
	}
	timers := givenUpAt(func(l string) bool { return !strings.Contains(l, "(3h41m)") })
	halfBar := givenUpAt(func(l string) bool {
		return usageBarCells(usageFirstField(l)) == tuiUsageNarrowBarWidth
	})
	noBar := givenUpAt(func(l string) bool { return usageBarCells(usageFirstField(l)) == 0 })
	noSpend := givenUpAt(func(l string) bool { return !strings.Contains(l, "api ") })
	noWindow := givenUpAt(func(l string) bool { return !strings.Contains(l, "codex 7d") })

	assert.Greater(t, timers, halfBar, "the reset timers go before the bar is halved")
	assert.Greater(t, halfBar, noBar, "the bar is halved before it is dropped")
	assert.Greater(t, noBar, noSpend, "every window is drawn plainly before the spend goes")
	assert.Greater(t, noSpend, noWindow, "and the spend goes before any rolling window")

	// The widest terminal draws the whole reading.
	m := loadedUsageModel(u)
	m.width = 200
	line := m.usageLine()
	assert.Contains(t, line, "(3h41m)")
	assert.Equal(t, tuiUsageBarWidth, usageBarCells(usageFirstField(line)))
}

// API spend is the rightmost field and the first one dropped, so a terminal
// that cannot hold everything keeps the rolling windows rather than the money.
func TestTUIUsageLineDropsAPISpendBeforeAWindow(t *testing.T) {
	u := bothAccountsUsage()
	u.TotalCostUSD = 12.34

	m := loadedUsageModel(u)
	m.width = 200
	assert.Contains(t, m.usageLine(), "api $12.34 mtd", "there is room for all of it")

	m.width = 70
	line := m.usageLine()
	assert.NotContains(t, line, "api ", "the spend goes first")
	for _, want := range []string{"claude 5h", "claude 7d", "codex 5h", "codex 7d"} {
		assert.Contains(t, line, want)
	}
}

func TestTUIUsageLineIsAbsentUntilTheFirstReadLands(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.width = 140
	assert.Empty(t, m.usageLine(), "nothing to say before the first poll answers")
	assert.NotContains(t, m.renderList(), "usage")
}

// Every poll having failed is worth one honest word; the operator otherwise
// cannot tell an account with no limits from a readout that never arrived.
func TestTUIUsageLineReportsAReadoutItCouldNotGet(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.width = 140

	updated, _ := m.Update(tuiUsageMsg{err: errUsagePoll})
	failed := updated.(tuiModel)
	assert.Equal(t, "usage unavailable", failed.usageLine())

	// A later success replaces it outright.
	updated, _ = failed.Update(tuiUsageMsg{usage: claudeUsage()})
	ok := updated.(tuiModel)
	assert.Contains(t, ok.usageLine(), "42%")

	// And a failure after that keeps the figures the console already has: they
	// are cached readings to begin with, so one lost poll changes nothing.
	updated, _ = ok.Update(tuiUsageMsg{err: errUsagePoll})
	stale := updated.(tuiModel)
	assert.Contains(t, stale.usageLine(), "42%")
}

// A standalone console can outlive the daemon it points at and end up talking
// to a tclaude old enough to have no usage endpoint. That is not a failure to
// report at the operator — it is a readout that does not exist there — so the
// line goes away rather than latching "unavailable" forever, and the poll backs
// off to a slow re-check that brings it back if the far end is upgraded.
func TestTUIUsageGoesQuietAgainstADaemonWithoutTheEndpoint(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.width = 140

	// A daemon that HAS the endpoint and merely failed still gets reported.
	updated, _ := m.Update(tuiUsageMsg{err: errUsagePoll})
	assert.Equal(t, "usage unavailable", updated.(tuiModel).usageLine())

	unsupported := fmt.Errorf("GET /v1/usage: %w", &tuiUnsupportedEndpointError{msg: "Not Found"})
	updated, _ = updated.(tuiModel).Update(tuiUsageMsg{err: unsupported})
	quiet := updated.(tuiModel)
	assert.Empty(t, quiet.usageLine(), "no line at all for a daemon that has no readout")

	quiet.lastUsageAttempt = time.Now().Add(-tuiUsageInterval)
	assert.False(t, quiet.usageDue(), "and no 30s polling of a route that is not there")
	quiet.lastUsageAttempt = time.Now().Add(-tuiUsageUnsupportedInterval)
	assert.True(t, quiet.usageDue(), "but it checks again in case the daemon was upgraded")

	// And an upgraded daemon brings the readout straight back.
	updated, _ = quiet.Update(tuiUsageMsg{usage: claudeUsage()})
	assert.Contains(t, updated.(tuiModel).usageLine(), "42%")
}

// Figures an earlier daemon served have no source once the console is talking
// to one without the endpoint, so they are dropped rather than left frozen.
func TestTUIUsageDropsFiguresWhenTheEndpointDisappears(t *testing.T) {
	m := loadedUsageModel(claudeUsage())
	unsupported := fmt.Errorf("GET /v1/usage: %w", &tuiUnsupportedEndpointError{msg: "Not Found"})
	updated, _ := m.Update(tuiUsageMsg{err: unsupported})
	assert.Empty(t, updated.(tuiModel).usageLine())
}

// A 404 from the in-process client is typed the same way, so the console's two
// transports agree on what "this daemon has no such operation" looks like.
func TestTUIAPIMarksAMissingEndpointAsUnsupported(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	withOperatorToken(t, "tclo_test-token")

	err := api.get("/v1/usage", &tuiUsage{})
	require.Error(t, err)
	assert.True(t, tuiEndpointUnsupported(err), err)
	assert.Contains(t, err.Error(), "Not Found", "the message the operator reads is unchanged")

	// Every other refusal stays an ordinary failure.
	other := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "auth", "only the human operator may read subscription usage")
	})
	err = other.get("/v1/usage", &tuiUsage{})
	require.Error(t, err)
	assert.False(t, tuiEndpointUnsupported(err), err)
}

// The console rounds a percentage the way the dashboard's Math.round does, not
// the way Go's %.0f (half-to-even) would: the same payload must not read 62%
// here and 63% in the browser.
func TestTUIUsageRoundsHalvesLikeTheDashboard(t *testing.T) {
	line := loadedUsageModel(tuiUsage{tuiSubscriptionUsage: tuiSubscriptionUsage{
		Available: true,
		FiveHour:  &tuiUsageWindow{Pct: 62.5},
		SevenDay:  &tuiUsageWindow{Pct: 3.5},
	}}).usageLine()

	assert.Contains(t, line, "63%")
	assert.Contains(t, line, "4%")
}

// The readout is the operator's own account. A console the daemon does not
// treat as the human is refused it, so it neither polls nor leaves a line
// saying so — the identity warning above the listing already explains why.
func TestTUIUsageIsNotPolledByANonOperatorConsole(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 140
	m.usage = claudeUsage()
	m.usageLoaded = true

	assert.False(t, m.usageDue())
	assert.Empty(t, m.usageLine())
}

// The usage poll is far slower than the 2s listing poll and must not stack
// requests when one is slow.
func TestTUIUsagePollIsPaced(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	assert.True(t, m.usageDue(), "the first tick reads it")

	m.usageFetching = true
	assert.False(t, m.usageDue(), "never while one is in flight")

	m.usageFetching = false
	m.lastUsageAttempt = time.Now()
	assert.False(t, m.usageDue())

	m.lastUsageAttempt = time.Now().Add(-tuiUsageInterval)
	assert.True(t, m.usageDue())
}

// The tick drives both polls, and neither may block the other: a listing
// refresh already in flight must not hold up the usage read.
func TestTUITickReadsUsageAlongsideTheListing(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.refreshing = true

	updated, cmd := m.Update(tuiTickMsg{})
	got := updated.(tuiModel)
	assert.True(t, got.usageFetching, "the tick started a usage read")
	assert.False(t, got.lastUsageAttempt.IsZero())
	assert.NotNil(t, cmd)
}

func TestTUIUsageCmdReadsTheDaemonsUsageEndpoint(t *testing.T) {
	var path string
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(w, http.StatusOK, dashboardUsage{
			Available:    true,
			FiveHour:     &usageWindow{Pct: 42, Remaining: "3h41m"},
			SevenDay:     &usageWindow{Pct: 18, Remaining: "2d9h"},
			TotalCostUSD: 12.34,
		})
	})
	withOperatorToken(t, "tclo_test-token")

	msg, ok := newTUIModel(api).usageCmd()().(tuiUsageMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "/v1/usage", path)
	assert.True(t, msg.usage.Available)
	require.NotNil(t, msg.usage.FiveHour)
	assert.Equal(t, 42.0, msg.usage.FiveHour.Pct)
	assert.Equal(t, "3h41m", msg.usage.FiveHour.Remaining)
	assert.Equal(t, 12.34, msg.usage.TotalCostUSD)
}

// The line is budgeted as exactly one row of chrome, so a terminal too narrow
// for everything drops whole segments rather than wrapping onto a second row
// the viewport has not paid for.
func TestTUIUsageLineStaysOnOneRow(t *testing.T) {
	u := claudeUsage()
	u.TotalCostUSD = 12.34
	for width := 10; width <= 140; width += 3 {
		m := loadedUsageModel(u)
		m.width = width
		line := m.usageLine()
		if line == "" {
			continue
		}
		assert.LessOrEqual(t, lipgloss.Width(line)+2, width, "width %d: %q", width, line)
		assert.NotContains(t, line, "\n")
	}
}

// Same contract as every other optional line in the list view: whatever it
// adds must be paid for out of the agent viewport, or the terminal scrolls.
func TestTUIListWithTheUsageLineStillFitsTheTerminal(t *testing.T) {
	u := claudeUsage()
	u.TotalCostUSD = 12.34
	m := loadedUsageModel(u)
	m.height = 24
	m.notice = "Spawned agent scout in dev."
	m.refreshErr = "Refresh failed: connection refused"
	m.identityWarning = "Note: agentd was started from inside a harness session."
	m.mode = tuiModeConfirmQuit
	for i := range 30 {
		m.agents = append(m.agents, tuiAgentRow{ConvID: string(rune('a' + i)), Title: "agent", Online: true})
	}
	// usageLine is operator-gated, and identityWarning above is only a render.
	m.operator = true

	require.Contains(t, m.renderList(), "5h")
	assert.LessOrEqual(t, strings.Count(m.renderList(), "\n"), m.height)
}
