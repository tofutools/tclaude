package agentd

import (
	"net/http"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
// sessions of the same conv (a carry-forward resume) telescope into one
// running total rather than each re-counting the conversation's
// cumulative, while a resume-after-exit's fresh per-session counter is
// still counted (see db.CostDeltas). The agentd-local twin of
// db.CostDelta, kept lowercase so collectCosts and the sort/first-day
// helpers read the same field names they always have.
type costDelta struct {
	day       string
	convID    string
	sessionID string
	usd       float64
	updatedAt string // RFC3339Nano of the day's last spend; "" if unknown
	model     string // model display name denormalised onto the row; "" if unknown
	harness   string // harness denormalised onto the row; "" if unknown
	kind      string // real | what_if
}

func mixedCostDeltasFromRows(rows []db.CostDailyRow) []costDelta {
	deltas := db.MixedCostDeltas(rows)
	out := make([]costDelta, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, costDelta{
			day: d.Day, convID: d.ConvID, sessionID: d.SessionID, usd: d.USD,
			updatedAt: d.UpdatedAt, model: d.Model, harness: d.Harness, kind: d.Kind,
		})
	}
	return out
}

// costDeltasFromRows recovers per-day spend deltas from cumulative
// (conv, day) snapshots. It is a thin adapter over db.CostDeltas — the
// canonical walk shared with the top bar's SumCostSinceDay so the two cost
// surfaces can never drift — mapping each delta onto the agentd-local
// costDelta the rest of this file consumes. See db.CostDeltas for the
// per-conversation high-water baseline and its session-boundary reset (the
// resume-after-exit case that was hiding a conversation from every span
// after its first). whatif selects the cumulative column: false → cost_usd,
// true → virtual_cost_usd (the WHAT-IF estimate).
func costDeltasFromRows(rows []db.CostDailyRow, whatif bool) []costDelta {
	deltas := db.CostDeltas(rows, whatif)
	out := make([]costDelta, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, costDelta{day: d.Day, convID: d.ConvID, sessionID: d.SessionID, usd: d.USD, updatedAt: d.UpdatedAt, model: d.Model, harness: d.Harness})
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
	Day           string  `json:"day"`
	CostUSD       float64 `json:"cost_usd"`
	RealCostUSD   float64 `json:"real_cost_usd,omitempty"`
	WhatIfCostUSD float64 `json:"what_if_cost_usd,omitempty"`
	CostKind      string  `json:"cost_kind,omitempty"`
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
// is the display name reported by the day's most recent costed session,
// denormalised onto the cost row at write time so it survives the
// session being deleted; empty only for pre-v71 history of an
// already-gone session, or a session that never reported a model. Day is the slice's
// local calendar day; LastActivity is the wall-clock time (RFC3339Nano)
// of the slice's most recent spend — the finer-grained timestamp the
// breakdown shows and sorts on; "" when unknown (pre-v53 history whose
// session was already gone), in which case the surface falls back to
// LastDay. LastDay equals Day (the slice's only day) and is kept for
// the wire's existing last-activity fallback.
type costAgentRow struct {
	// AgentID is the spending actor's stable key — a display companion to
	// ConvID so a row can name WHO it belongs to. The per-conv keying and
	// the cumulative-cost delta walk are unchanged: cost stays a per-conv
	// series (a generation's cost resets per /clear), and this is only an
	// added attribution field, never a rekey. "" when the conv is not a
	// known agent (e.g. a plain conversation's spend).
	AgentID       string  `json:"agent_id,omitempty"`
	ConvID        string  `json:"conv_id"`
	Title         string  `json:"title"`
	Day           string  `json:"day"`
	CostUSD       float64 `json:"cost_usd"`
	RealCostUSD   float64 `json:"real_cost_usd,omitempty"`
	WhatIfCostUSD float64 `json:"what_if_cost_usd,omitempty"`
	CostKind      string  `json:"cost_kind"`
	Continued     bool    `json:"continued,omitempty"`
	LastDay       string  `json:"last_day"`
	LastActivity  string  `json:"last_activity,omitempty"`
	Model         string  `json:"model"`
	Harness       string  `json:"harness"`
}

// costsResponse is the /api/costs wire shape. Days is zero-filled —
// one point for every calendar day in [from, to] — so the chart can
// render gaps as empty bars without client-side date math. FirstDay is
// the earliest day carrying any recorded spend across all history (not
// just this span); the Costs tab's month projection uses it to decide
// where the per-weekday average starts (see firstCostDay).
type costsResponse struct {
	From           string         `json:"from"`
	To             string         `json:"to"`
	FirstDay       string         `json:"first_day,omitempty"`
	Days           []costDayPoint `json:"days"`
	Agents         []costAgentRow `json:"agents"`
	TotalUSD       float64        `json:"total_usd"`
	RealTotalUSD   float64        `json:"real_total_usd,omitempty"`
	WhatIfTotalUSD float64        `json:"what_if_total_usd,omitempty"`
	CostKind       string         `json:"cost_kind,omitempty"`
}

// maxCostSpanDays caps the requested span. The daily table is small,
// but a garbage from date must not zero-fill years of empty points
// into the response.
const maxCostSpanDays = 366

// collectCosts aggregates the daily cost table over [from, to].
// Pure assembly over costDeltasFromRows; the handler owns HTTP
// concerns, this owns the shape.
//
// to bounds the span's upper edge — today for the trailing/current-month
// spans, or a completed month's last day for the "browse an earlier
// month" spans. The maxCostSpanDays cap is measured back from to, so a
// far-past from can't zero-fill years of empty points.
//
// factor is the display multiplier from config (config.ResolvedCostFactor):
// every dollar figure in the response — the per-day bars, the per-agent
// breakdown, and the total — is scaled by it as the last step, so a
// compensation factor nudges the whole tab in lockstep while the
// underlying session_cost_daily rows stay raw. factor 1 (the default)
// is a no-op.
//
// Each daily session slice contributes real pay-per-token spend when present;
// otherwise it contributes the subscription WHAT-IF estimate. The response
// carries both subtotals and per-row kind metadata so the client can identify
// mixed spans without maintaining a second aggregation path.
func collectCosts(from, to time.Time, factor float64) (costsResponse, error) {
	if min := to.AddDate(0, 0, -(maxCostSpanDays - 1)); from.Before(min) {
		from = min
	}
	fromKey := from.Format(costDayKey)
	toKey := to.Format(costDayKey)

	rows, err := db.AllCostDailyRows()
	if err != nil {
		return costsResponse{}, err
	}
	deltas := mixedCostDeltasFromRows(rows)
	models, err := db.SessionModels()
	if err != nil {
		return costsResponse{}, err
	}
	harnesses, err := db.SessionHarnesses()
	if err != nil {
		return costsResponse{}, err
	}

	type kindTotals struct{ real, whatif float64 }
	byDay := map[string]kindTotals{}
	type sliceAgg struct {
		usd    float64
		real   float64
		whatif float64
		// lastActivity is the RFC3339Nano time of the slice's last spend —
		// the finest-grained "last activity" the breakdown can show; a
		// later same-day stamp raises it. "" when no contributing row
		// carried a timestamp.
		lastActivity string
		// model of the slice's latest-stamped delta with a known model;
		// modelAt tracks that stamp so a model-less delta (no statusline
		// tick yet) never blanks a value recorded earlier the same day.
		model   string
		modelAt string
		// harness of the slice's latest-stamped delta with a known harness.
		harness   string
		harnessAt string
	}
	// One aggregate per (conv, day): a conversation that spent across
	// several days breaks into one row per day, so a resume shows its true
	// per-day split rather than one lump.
	type sliceKey struct{ conv, day string }
	bySlice := map[sliceKey]*sliceAgg{}
	// Latest in-span day each conv spent on — drives the Continued flag:
	// every slice below a conv's latest day is an earlier continuation.
	convMaxDay := map[string]string{}
	total, realTotal, whatIfTotal := 0.0, 0.0, 0.0
	for _, d := range deltas {
		if d.day < fromKey || d.day > toKey {
			continue
		}
		dayTotals := byDay[d.day]
		if d.kind == "what_if" {
			dayTotals.whatif += d.usd
		} else {
			dayTotals.real += d.usd
		}
		byDay[d.day] = dayTotals
		k := sliceKey{d.convID, d.day}
		a := bySlice[k]
		if a == nil {
			a = &sliceAgg{}
			bySlice[k] = a
		}
		a.usd += d.usd
		if d.kind == "what_if" {
			a.whatif += d.usd
			whatIfTotal += d.usd
		} else {
			a.real += d.usd
			realTotal += d.usd
		}
		if d.updatedAt > a.lastActivity {
			a.lastActivity = d.updatedAt
		}
		// Prefer the model denormalised onto the cost row — it survives the
		// sessions row being deleted, so a retired agent still names its
		// model. Fall back to the live sessions lookup for pre-v71 history
		// of a still-alive session whose row predates the denormalisation.
		m := d.model
		if m == "" {
			m = models[d.sessionID]
		}
		if m != "" && d.updatedAt >= a.modelAt {
			a.model, a.modelAt = m, d.updatedAt
		}
		h := d.harness
		if h == "" {
			h = harnesses[d.sessionID]
		}
		if h != "" && d.updatedAt >= a.harnessAt {
			a.harness, a.harnessAt = h, d.updatedAt
		}
		if d.day > convMaxDay[d.convID] {
			convMaxDay[d.convID] = d.day
		}
		total += d.usd
	}

	out := costsResponse{From: fromKey, To: toKey, FirstDay: firstCostDay(deltas), TotalUSD: total,
		RealTotalUSD: realTotal, WhatIfTotalUSD: whatIfTotal, CostKind: costKind(realTotal, whatIfTotal),
		Days: []costDayPoint{}, Agents: []costAgentRow{}}
	for day := from; ; day = day.AddDate(0, 0, 1) {
		key := day.Format(costDayKey)
		if key > toKey {
			break
		}
		totals := byDay[key]
		out.Days = append(out.Days, costDayPoint{
			Day: key, CostUSD: totals.real + totals.whatif,
			RealCostUSD: totals.real, WhatIfCostUSD: totals.whatif,
			CostKind: costKind(totals.real, totals.whatif),
		})
	}
	for k, a := range bySlice {
		out.Agents = append(out.Agents, costAgentRow{
			AgentID:       peerAgentID(k.conv),
			ConvID:        k.conv,
			Title:         agent.CachedTitle(k.conv),
			Day:           k.day,
			CostUSD:       a.usd,
			RealCostUSD:   a.real,
			WhatIfCostUSD: a.whatif,
			CostKind:      costKind(a.real, a.whatif),
			Continued:     k.day < convMaxDay[k.conv],
			LastDay:       k.day,
			LastActivity:  a.lastActivity,
			Model:         a.model,
			Harness:       a.harness,
		})
	}
	sortCostAgentRows(out.Agents)
	// Display-only compensation, applied last so it never feeds back into
	// the per-conv baseline walk above. Scaling is monotonic for a
	// positive factor, so the sort order is unchanged. factor 1 is the
	// common path and a no-op.
	if factor != 1 {
		out.TotalUSD *= factor
		out.RealTotalUSD *= factor
		out.WhatIfTotalUSD *= factor
		for i := range out.Days {
			out.Days[i].CostUSD *= factor
			out.Days[i].RealCostUSD *= factor
			out.Days[i].WhatIfCostUSD *= factor
		}
		for i := range out.Agents {
			out.Agents[i].CostUSD *= factor
			out.Agents[i].RealCostUSD *= factor
			out.Agents[i].WhatIfCostUSD *= factor
		}
	}
	return out, nil
}

func costKind(real, whatif float64) string {
	switch {
	case real > 0 && whatif > 0:
		return "mixed"
	case whatif > 0:
		return "what_if"
	case real > 0:
		return "real"
	default:
		return ""
	}
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

// handleDashboardCosts serves GET /api/costs?from=YYYY-MM-DD[&to=YYYY-MM-DD] —
// the Costs tab's data source. from defaults to the first of the current
// month (the tab's default span); to defaults to today. The "browse an
// earlier month" spans pass an explicit to (a completed month's last day)
// so a bounded past window can be shown; the trailing/current-month spans
// omit it and get today. Each row prefers real spend and falls back to the
// WHAT-IF subscription estimate; the payload identifies the contributing
// kind at row, day, and total levels. Fetched on tab activation and span
// change, not on the 2s snapshot tick — history doesn't move that fast.
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
	to := now
	if q := r.URL.Query().Get("to"); q != "" {
		t, err := time.ParseInLocation(costDayKey, q, now.Location())
		if err != nil {
			http.Error(w, "bad to date, want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		to = t
	}
	cfg, _ := config.Load()
	out, err := collectCosts(from, to, cfg.ResolvedCostFactor())
	if err != nil {
		http.Error(w, "collect costs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
