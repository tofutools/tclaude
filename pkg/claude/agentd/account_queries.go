package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	PermUsageRead = "usage.read"
	PermCostsRead = "costs.read"
)

type accountUsageWindow struct {
	Provider   string                          `json:"provider"`
	WindowName string                          `json:"window_name"`
	Available  bool                            `json:"available"`
	Status     string                          `json:"status"` // current | stale | reset | unknown_age
	Pct        float64                         `json:"pct"`    // last observed, never an inferred zero after reset
	UsedUnits  float64                         `json:"used_units,omitempty"`
	LimitUnits float64                         `json:"limit_units,omitempty"`
	ObservedAt string                          `json:"observed_at,omitempty"`
	AgeSeconds *int64                          `json:"age_seconds,omitempty"`
	ResetsAt   string                          `json:"resets_at,omitempty"`
	Source     string                          `json:"source,omitempty"`
	Forecasts  map[string]usageHistoryForecast `json:"forecasts"`
}

type accountUsageResponse struct {
	GeneratedAt      string                 `json:"generated_at"`
	Scope            string                 `json:"scope"`
	Windows          []accountUsageWindow   `json:"windows"`
	CoverageWarnings []usageCoverageWarning `json:"coverage_warnings"`
}

// collectAccountUsage preserves observed values, including expired readings.
// Unlike dashboard bars, missing windows are never synthesized as zero usage.
// Forecasts use the same seven-day view and estimators as the Usage tab.
func collectAccountUsage(now time.Time, idleTimeout time.Duration) (accountUsageResponse, error) {
	history, err := collectUsageHistory(now.Add(-7*24*time.Hour), nil, now)
	if err != nil {
		return accountUsageResponse{}, err
	}
	out := accountUsageResponse{GeneratedAt: now.UTC().Format(time.RFC3339Nano), Scope: "account", Windows: []accountUsageWindow{}, CoverageWarnings: history.CoverageWarnings}
	windows := map[usageSeriesKey]accountUsageWindow{}
	forecasts := map[usageSeriesKey]map[string]usageHistoryForecast{}
	for _, s := range history.Series {
		forecasts[usageSeriesKey{s.Provider, s.WindowName}] = s.Forecasts
	}
	if idleTimeout <= 0 {
		idleTimeout = usageStaleAfter
	}
	add := func(provider, name string, pct, used, limit float64, observed, reset time.Time, source string) {
		if observed.IsZero() {
			return
		}
		key := usageSeriesKey{provider, name}
		if prev, ok := windows[key]; ok {
			at, _ := time.Parse(time.RFC3339Nano, prev.ObservedAt)
			if !observed.After(at) {
				return
			}
		}
		maxAge := idleTimeout
		if provider == db.SubscriptionProviderOpenAI {
			maxAge = codexUsageMaxAge
		}
		status := "current"
		if now.Sub(observed) > maxAge {
			status = "stale"
		}
		if !reset.IsZero() && !reset.After(now) {
			status = "reset"
		}
		age := max(int64(0), int64(now.Sub(observed)/time.Second))
		w := accountUsageWindow{Provider: provider, WindowName: name, Available: status == "current", Status: status, Pct: pct, UsedUnits: used, LimitUnits: limit, ObservedAt: observed.UTC().Format(time.RFC3339Nano), AgeSeconds: &age, Source: source, Forecasts: forecasts[key]}
		if w.Forecasts == nil {
			w.Forecasts = map[string]usageHistoryForecast{}
		}
		if !reset.IsZero() {
			w.ResetsAt = reset.UTC().Format(time.RFC3339Nano)
		}
		windows[key] = w
	}
	// History supplies windows absent from the compact provider caches (Sonnet,
	// for example), and retains explicitly stale readings for informed clients.
	rows, err := db.SubscriptionUsageHistorySince(now.Add(-db.DefaultSubscriptionUsageRetention))
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		reset := row.ResetsAt
		if row.Provider == db.SubscriptionProviderGitHub {
			reset = copilotMonthlyResetAt(row.ObservedAt)
		}
		add(row.Provider, row.WindowName, row.UsedPercent, row.UsedUnits, row.LimitUnits, row.ObservedAt, reset, row.Source)
	}
	claude, codex, _, _, err := db.LoadDashboardUsageCaches()
	if err != nil {
		return out, err
	}
	if claude != nil {
		var c usageapi.CachedUsage
		if json.Unmarshal(claude.Data, &c) == nil {
			for name, b := range map[string]*usageapi.CachedBucket{"five_hour": c.FiveHour, "seven_day": c.SevenDay, "seven_day_sonnet": c.SevenDaySonnet} {
				if b != nil {
					key := usageSeriesKey{db.SubscriptionProviderAnthropic, name}
					// Claude carries forward omitted buckets while advancing the
					// cache-wide FetchedAt. Only history proves a bucket's age.
					if _, exists := windows[key]; exists {
						continue
					}
					status := "unknown_age"
					if !b.ResetsAt.IsZero() && !b.ResetsAt.After(now) {
						status = "reset"
					}
					windows[key] = accountUsageWindow{
						Provider: db.SubscriptionProviderAnthropic, WindowName: name,
						Status: status, Pct: b.Pct, Source: "cache",
						ResetsAt:  formatResetsAt(b.ResetsAt),
						Forecasts: map[string]usageHistoryForecast{},
					}
				}
			}
		}
	}
	if codex != nil {
		var c harness.CodexUsage
		if json.Unmarshal(codex.Data, &c) == nil {
			for name, b := range map[string]*harness.CodexRateLimitWindow{"five_hour": c.FiveHour, "seven_day": c.Weekly} {
				if b != nil {
					add(db.SubscriptionProviderOpenAI, name, b.UsedPercent, 0, 0, c.Observed, b.ResetsAt, "cache")
				}
			}
		}
	}
	for _, w := range windows {
		out.Windows = append(out.Windows, w)
	}
	sort.Slice(out.Windows, func(i, j int) bool {
		a, b := out.Windows[i], out.Windows[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.WindowName < b.WindowName
	})
	return out, nil
}

func handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRead); !ok {
		return
	}
	cfg, _ := config.Load()
	out, err := collectAccountUsage(time.Now(), cfg.ResolvedUsageIdleTimeout())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "usage_read", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type accountCostsResponse struct {
	costsResponse
	GeneratedAt    string  `json:"generated_at"`
	Timezone       string  `json:"timezone"`
	Scope          string  `json:"scope"`
	AgentID        string  `json:"agent_id,omitempty"`
	WhatIfEnabled  bool    `json:"what_if_enabled"`
	TodayRealUSD   float64 `json:"today_real_usd"`
	TodayWhatIfUSD float64 `json:"today_what_if_usd"`
}

func accountCostRange(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for name, target := range map[string]*time.Time{"from": &from, "to": &to} {
		if q := r.URL.Query().Get(name); q != "" {
			t, err := time.ParseInLocation(costDayKey, q, now.Location())
			if err != nil {
				return from, to, fmt.Errorf("bad %s date, want YYYY-MM-DD", name)
			}
			*target = t
		}
	}
	if from.After(to) {
		return from, to, fmt.Errorf("from must be on or before to")
	}
	if from.Before(to.AddDate(0, 0, -(maxCostSpanDays - 1))) {
		return from, to, fmt.Errorf("date range must be at most %d days", maxCostSpanDays)
	}
	return from, to, nil
}

func handleAccountCosts(w http.ResponseWriter, r *http.Request) {
	conv, ok := requirePermission(w, r, PermCostsRead)
	if !ok {
		return
	}
	now := time.Now()
	from, to, err := accountCostRange(r, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_range", err.Error())
		return
	}
	self := r.URL.Query().Get("self")
	if self != "" && self != "true" {
		writeError(w, http.StatusBadRequest, "bad_self", "self must be true")
		return
	}
	agentID := ""
	if self == "true" {
		if conv == "" {
			writeError(w, http.StatusBadRequest, "agent_required", "--self requires an identified agent caller")
			return
		}
		agentID, err = db.AgentIDForConv(conv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "agent_lookup", "could not resolve caller's stable agent ID")
			return
		}
		if agentID == "" {
			writeError(w, http.StatusBadRequest, "agent_required", "--self requires a caller with a stable agent ID")
			return
		}
	}
	cfg, _ := config.Load()
	includeWhatIf := cfg != nil && cfg.Cost != nil && cfg.Cost.ShowOnSubscription
	// nil config keeps the raw costs: dashboard display multipliers do not apply.
	costs, err := collectCosts(from, to, nil, includeWhatIf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "costs_read", err.Error())
		return
	}
	if agentID != "" {
		costs = costsForAgent(costs, agentID)
	}
	out := accountCostsResponse{costsResponse: costs, GeneratedAt: now.UTC().Format(time.RFC3339Nano), Timezone: now.Location().String(), Scope: "account", AgentID: agentID, WhatIfEnabled: includeWhatIf}
	if agentID != "" {
		out.Scope = "self"
	}
	for _, day := range costs.Days {
		if day.Day == now.Format(costDayKey) {
			out.TodayRealUSD = day.RealCostUSD
			out.TodayWhatIfUSD = day.WhatIfCostUSD
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func costsForAgent(in costsResponse, agentID string) costsResponse {
	out := costsResponse{From: in.From, To: in.To, Days: []costDayPoint{}, Agents: []costAgentRow{}}
	byDay := map[string]costDayPoint{}
	for _, row := range in.Agents {
		if row.AgentID != agentID {
			continue
		}
		out.Agents = append(out.Agents, row)
		out.TotalUSD += row.CostUSD
		out.RealTotalUSD += row.RealCostUSD
		out.WhatIfTotalUSD += row.WhatIfCostUSD
		out.VirtualCostCredits += row.VirtualCostCredits
		day := byDay[row.Day]
		day.Day = row.Day
		day.CostUSD += row.CostUSD
		day.RealCostUSD += row.RealCostUSD
		day.WhatIfCostUSD += row.WhatIfCostUSD
		day.VirtualCostCredits += row.VirtualCostCredits
		day.CostKind = costKind(day.RealCostUSD, day.WhatIfCostUSD)
		byDay[row.Day] = day
	}
	for _, day := range in.Days {
		d := byDay[day.Day]
		d.Day = day.Day
		out.Days = append(out.Days, d)
	}
	out.CostKind = costKind(out.RealTotalUSD, out.WhatIfTotalUSD)
	return out
}
