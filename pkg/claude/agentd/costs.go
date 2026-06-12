package agentd

import (
	"net/http"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// costs.go — the /api/costs endpoint behind the dashboard's Costs tab,
// plus the pure aggregation that turns session_cost_daily's cumulative
// per-day snapshots into per-day / per-agent spend. The same
// aggregation feeds the top bar's month-to-date figure (usage.go), so
// the headline number and the tab's breakdown always agree.

// costDayKey formats a time as a session_cost_daily day key (local
// calendar date) — must match db's costDayFormat.
const costDayKey = "2006-01-02"

// costDelta is one recovered slice of actual spend: on this local day,
// the agent (conv) spent this many dollars. Derived from consecutive
// cumulative snapshots of a session; multiple sessions of the same
// conv simply contribute separate deltas.
type costDelta struct {
	day       string
	convID    string
	sessionID string
	usd       float64
}

// costDeltasFromRows turns cumulative (session, day) snapshots into
// per-day spend deltas. Rows must be ordered (session_id, day) — the
// order AllCostDailyRows returns. Within a session, a day's spend is
// its snapshot minus the highest snapshot of any earlier day; the
// session's first day carries its whole cumulative value (for rows
// born in the v51 backfill that means pre-existing history lands on
// the migration day). The high-water baseline clamps the /clear edge:
// a cumulative figure that dips and recovers only counts the rise
// above the previous maximum, never a negative day.
func costDeltasFromRows(rows []db.CostDailyRow) []costDelta {
	var out []costDelta
	prevSession := ""
	baseline := 0.0
	for _, r := range rows {
		if r.SessionID != prevSession {
			prevSession = r.SessionID
			baseline = 0
		}
		if d := r.CostUSD - baseline; d > 0 {
			out = append(out, costDelta{day: r.Day, convID: r.ConvID, sessionID: r.SessionID, usd: d})
			baseline = r.CostUSD
		}
	}
	return out
}

// sumCostDeltas totals the deltas with day keys in [from, to]; either
// bound may be "" for unbounded. Day keys are zero-padded ISO dates,
// so plain string comparison is date comparison.
func sumCostDeltas(deltas []costDelta, from, to string) float64 {
	total := 0.0
	for _, d := range deltas {
		if (from == "" || d.day >= from) && (to == "" || d.day <= to) {
			total += d.usd
		}
	}
	return total
}

// costDayPoint is one bar of the Costs tab chart: total spend across
// all agents on one local day.
type costDayPoint struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"cost_usd"`
}

// costAgentRow is one row of the Costs tab's per-agent breakdown:
// spend within the requested span, grouped by conversation (the
// dashboard's notion of an agent). Title resolves through the same
// cached lookup the snapshot uses; a conv deleted since the spend was
// recorded keeps its history under the "(unknown)" placeholder.
// Model is the display name reported by the agent's most recent
// costed session in the span — "latest model wins" when sessions ran
// different models; empty when no live sessions row still carries one.
type costAgentRow struct {
	ConvID  string  `json:"conv_id"`
	Title   string  `json:"title"`
	CostUSD float64 `json:"cost_usd"`
	LastDay string  `json:"last_day"`
	Model   string  `json:"model"`
}

// costsResponse is the /api/costs wire shape. Days is zero-filled —
// one point for every calendar day in [from, to] — so the chart can
// render gaps as empty bars without client-side date math.
type costsResponse struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Days     []costDayPoint `json:"days"`
	Agents   []costAgentRow `json:"agents"`
	TotalUSD float64        `json:"total_usd"`
}

// maxCostSpanDays caps the requested span. The daily table is small,
// but a garbage from date must not zero-fill years of empty points
// into the response.
const maxCostSpanDays = 366

// collectCosts aggregates the daily cost table over [from, today].
// Pure assembly over costDeltasFromRows; the handler owns HTTP
// concerns, this owns the shape.
func collectCosts(from time.Time) (costsResponse, error) {
	now := time.Now()
	if min := now.AddDate(0, 0, -(maxCostSpanDays - 1)); from.Before(min) {
		from = min
	}
	fromKey := from.Format(costDayKey)
	toKey := now.Format(costDayKey)

	rows, err := db.AllCostDailyRows()
	if err != nil {
		return costsResponse{}, err
	}
	deltas := costDeltasFromRows(rows)
	models, err := db.SessionModels()
	if err != nil {
		return costsResponse{}, err
	}

	byDay := map[string]float64{}
	type agentAgg struct {
		usd     float64
		lastDay string
		// model of the latest-day session with a known model; modelDay
		// tracks that day so a model-less session (its row deleted, or
		// no statusline tick yet) never blanks an older known value.
		model    string
		modelDay string
	}
	byConv := map[string]*agentAgg{}
	total := 0.0
	for _, d := range deltas {
		if d.day < fromKey || d.day > toKey {
			continue
		}
		byDay[d.day] += d.usd
		a := byConv[d.convID]
		if a == nil {
			a = &agentAgg{}
			byConv[d.convID] = a
		}
		a.usd += d.usd
		if d.day > a.lastDay {
			a.lastDay = d.day
		}
		if m := models[d.sessionID]; m != "" && d.day >= a.modelDay {
			a.model, a.modelDay = m, d.day
		}
		total += d.usd
	}

	out := costsResponse{From: fromKey, To: toKey, TotalUSD: total,
		Days: []costDayPoint{}, Agents: []costAgentRow{}}
	for day := from; ; day = day.AddDate(0, 0, 1) {
		key := day.Format(costDayKey)
		if key > toKey {
			break
		}
		out.Days = append(out.Days, costDayPoint{Day: key, CostUSD: byDay[key]})
	}
	for convID, a := range byConv {
		out.Agents = append(out.Agents, costAgentRow{
			ConvID:  convID,
			Title:   agent.CachedTitle(convID),
			CostUSD: a.usd,
			LastDay: a.lastDay,
			Model:   a.model,
		})
	}
	sortCostAgentRows(out.Agents)
	return out, nil
}

// sortCostAgentRows orders the breakdown most-recent-first: latest
// activity day first, spend descending within a day, conv id as the
// stable tail. Day keys are zero-padded ISO dates, so string order is
// date order.
func sortCostAgentRows(agents []costAgentRow) {
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].LastDay != agents[j].LastDay {
			return agents[i].LastDay > agents[j].LastDay
		}
		if agents[i].CostUSD != agents[j].CostUSD {
			return agents[i].CostUSD > agents[j].CostUSD
		}
		return agents[i].ConvID < agents[j].ConvID
	})
}

// handleDashboardCosts serves GET /api/costs?from=YYYY-MM-DD — the
// Costs tab's data source. from defaults to the first of the current
// month (the tab's default span); to is always today. Fetched on tab
// activation and span change, not on the 2s snapshot tick — history
// doesn't move that fast.
func handleDashboardCosts(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if q := r.URL.Query().Get("from"); q != "" {
		t, err := time.ParseInLocation(costDayKey, q, now.Location())
		if err != nil {
			http.Error(w, "bad from date, want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		from = t
	}
	out, err := collectCosts(from)
	if err != nil {
		http.Error(w, "collect costs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
