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
// cumulative snapshots, baselined per conversation — so multiple
// sessions of the same conv (a resume) telescope into one running
// total rather than each re-counting the conversation's cumulative.
type costDelta struct {
	day       string
	convID    string
	sessionID string
	usd       float64
	updatedAt string // RFC3339Nano of the day's last spend; "" if unknown
}

// costDeltasFromRows turns cumulative (conv, day) snapshots into per-day
// spend deltas. Rows must be ordered (conv-key, day, session_id) — the
// order AllCostDailyRows returns. The high-water baseline is carried per
// CONVERSATION, not per session: Claude Code's total_cost_usd is
// cumulative across the whole conversation and persists across resume, so
// when a conv is resumed under a new tclaude session (a new day, or just
// a fresh pane) that session's first snapshot already includes the prior
// spend. Baselining per conv recovers only the genuine rise — the
// session's first day no longer re-counts the conversation's whole
// cumulative, which was the multi-day (and same-day double-resume)
// double-count bug. The conv-key falls back to session_id for the rare
// row with no denormalised conv_id, so unrelated sessions never merge.
// A day's spend is its snapshot minus the conversation's high-water mark
// across all earlier rows; the conversation's first day carries its whole
// cumulative value (for rows born in the v51 backfill that means
// pre-existing history lands on the migration day). The high-water
// baseline clamps a dip-and-recover: only the rise above the previous
// maximum counts, never a negative day.
func costDeltasFromRows(rows []db.CostDailyRow) []costDelta {
	var out []costDelta
	prevKey := ""
	baseline := 0.0
	for _, r := range rows {
		key := r.ConvID
		if key == "" {
			key = r.SessionID
		}
		if key != prevKey {
			prevKey = key
			baseline = 0
		}
		if d := r.CostUSD - baseline; d > 0 {
			out = append(out, costDelta{day: r.Day, convID: r.ConvID, sessionID: r.SessionID, usd: d, updatedAt: r.UpdatedAt})
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

// firstCostDay returns the earliest day carrying recorded spend across
// all deltas — tclaude's first-ever costed day — or "" when nothing
// has ever been spent. The Costs tab's month projection uses it to
// anchor the per-weekday average: when the first-ever spend was this
// month, the empty days before it must not dilute the average (a fresh
// install would otherwise project far too low); when earlier-month
// history exists, those leading zeros are genuine idle weekdays and
// stay in the denominator. Deltas need not be sorted.
func firstCostDay(deltas []costDelta) string {
	first := ""
	for _, d := range deltas {
		if first == "" || d.day < first {
			first = d.day
		}
	}
	return first
}

// costDayPoint is one bar of the Costs tab chart: total spend across
// all agents on one local day.
type costDayPoint struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"cost_usd"`
}

// costAgentRow is one row of the Costs tab's per-agent breakdown: spend
// by one conversation (the dashboard's notion of an agent) on one local
// day within the requested span. A conversation that spent across
// several days yields one row per day, so a resume reads as the genuine
// per-day split (e.g. $16.44 the day it started, $3.64 the day it was
// continued) instead of one lump. Continued marks the earlier-day
// slices of such a multi-day conversation — every slice except its
// latest day in the span — so the surface can flag them (a ↩ icon) as
// continuations of the row shown above. Title resolves through the same
// cached lookup the snapshot uses; a conv deleted since the spend was
// recorded keeps its history under the "(unknown)" placeholder. Model
// is the display name reported by the day's most recent costed session;
// empty when no live sessions row still carries one. Day is the slice's
// local calendar day; LastActivity is the wall-clock time (RFC3339Nano)
// of the slice's most recent spend — the finer-grained timestamp the
// breakdown shows and sorts on; "" when unknown (pre-v53 history whose
// session was already gone), in which case the surface falls back to
// LastDay. LastDay equals Day (the slice's only day) and is kept for
// the wire's existing last-activity fallback.
type costAgentRow struct {
	ConvID       string  `json:"conv_id"`
	Title        string  `json:"title"`
	Day          string  `json:"day"`
	CostUSD      float64 `json:"cost_usd"`
	Continued    bool    `json:"continued,omitempty"`
	LastDay      string  `json:"last_day"`
	LastActivity string  `json:"last_activity,omitempty"`
	Model        string  `json:"model"`
}

// costsResponse is the /api/costs wire shape. Days is zero-filled —
// one point for every calendar day in [from, to] — so the chart can
// render gaps as empty bars without client-side date math. FirstDay is
// the earliest day carrying any recorded spend across all history (not
// just this span); the Costs tab's month projection uses it to decide
// where the per-weekday average starts (see firstCostDay).
type costsResponse struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	FirstDay string         `json:"first_day,omitempty"`
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
	type sliceAgg struct {
		usd float64
		// lastActivity is the RFC3339Nano time of the slice's last spend —
		// the finest-grained "last activity" the breakdown can show; a
		// later same-day stamp raises it. "" when no contributing row
		// carried a timestamp.
		lastActivity string
		// model of the slice's latest-stamped session with a known model;
		// modelAt tracks that stamp so a model-less session (its row
		// deleted, or no statusline tick yet) never blanks a value recorded
		// earlier the same day.
		model   string
		modelAt string
	}
	// One aggregate per (conv, day): a conversation that spent across
	// several days breaks into one row per day, so a resume shows its true
	// per-day split rather than one lump.
	type sliceKey struct{ conv, day string }
	bySlice := map[sliceKey]*sliceAgg{}
	// Latest in-span day each conv spent on — drives the Continued flag:
	// every slice below a conv's latest day is an earlier continuation.
	convMaxDay := map[string]string{}
	total := 0.0
	for _, d := range deltas {
		if d.day < fromKey || d.day > toKey {
			continue
		}
		byDay[d.day] += d.usd
		k := sliceKey{d.convID, d.day}
		a := bySlice[k]
		if a == nil {
			a = &sliceAgg{}
			bySlice[k] = a
		}
		a.usd += d.usd
		if d.updatedAt > a.lastActivity {
			a.lastActivity = d.updatedAt
		}
		if m := models[d.sessionID]; m != "" && d.updatedAt >= a.modelAt {
			a.model, a.modelAt = m, d.updatedAt
		}
		if d.day > convMaxDay[d.convID] {
			convMaxDay[d.convID] = d.day
		}
		total += d.usd
	}

	out := costsResponse{From: fromKey, To: toKey, FirstDay: firstCostDay(deltas), TotalUSD: total,
		Days: []costDayPoint{}, Agents: []costAgentRow{}}
	for day := from; ; day = day.AddDate(0, 0, 1) {
		key := day.Format(costDayKey)
		if key > toKey {
			break
		}
		out.Days = append(out.Days, costDayPoint{Day: key, CostUSD: byDay[key]})
	}
	for k, a := range bySlice {
		out.Agents = append(out.Agents, costAgentRow{
			ConvID:       k.conv,
			Title:        agent.CachedTitle(k.conv),
			Day:          k.day,
			CostUSD:      a.usd,
			Continued:    k.day < convMaxDay[k.conv],
			LastDay:      k.day,
			LastActivity: a.lastActivity,
			Model:        a.model,
		})
	}
	sortCostAgentRows(out.Agents)
	return out, nil
}

// sortCostAgentRows orders the breakdown most-recent-first: latest
// activity first, spend descending within a tie, conv id as the stable
// tail. Recency uses the precise last-activity timestamp when both
// rows carry one; otherwise it falls back to the calendar day, so an
// agent with a known time on a day sorts ahead of one with only that
// day's date (its activity is provably no earlier, and resolved finer).
func sortCostAgentRows(agents []costAgentRow) {
	sort.Slice(agents, func(i, j int) bool {
		if ki, kj := costRowRecencyKey(agents[i]), costRowRecencyKey(agents[j]); ki != kj {
			return ki > kj
		}
		if agents[i].CostUSD != agents[j].CostUSD {
			return agents[i].CostUSD > agents[j].CostUSD
		}
		return agents[i].ConvID < agents[j].ConvID
	})
}

// costRowRecencyKey is the string the breakdown sorts recency on. With
// a precise timestamp it's the RFC3339Nano value (lexically ordered —
// the local offset is constant across rows, so string order is time
// order); without one it's the calendar day floored to midnight, which
// sorts just below any same-day timestamp. Both forms share the
// "2006-01-02" prefix, so cross-form comparison still orders by day.
func costRowRecencyKey(a costAgentRow) string {
	if a.LastActivity != "" {
		return a.LastActivity
	}
	if a.LastDay != "" {
		return a.LastDay + "T00:00:00"
	}
	return ""
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
