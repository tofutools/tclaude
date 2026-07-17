package agentd

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	defaultUsageHistoryHours = 7 * 24
	maxUsageHistoryHours     = 90 * 24
	usageResetDropPercent    = 2.0
	usageForecastMinSamples  = 3
	usageForecastMinElapsed  = 30 * time.Minute
)

type usageHistoryPoint struct {
	At       string  `json:"at"`
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resets_at,omitempty"`
	Source   string  `json:"source,omitempty"`
}

type usageHistoryReset struct {
	At  string  `json:"at"`
	Pct float64 `json:"pct"`
}

type usageHistoryForecast struct {
	Status           string  `json:"status"`
	SegmentStartedAt string  `json:"segment_started_at"`
	BaselinePct      float64 `json:"baseline_pct"`
	SampleCount      int     `json:"sample_count"`
	RatePctPerHour   float64 `json:"rate_pct_per_hour,omitempty"`
	HitsLimitAt      string  `json:"hits_limit_at,omitempty"`
	ResetAt          string  `json:"reset_at,omitempty"`
}

type usageHistorySeries struct {
	Provider        string               `json:"provider"`
	WindowName      string               `json:"window_name"`
	DurationSeconds int64                `json:"duration_seconds,omitempty"`
	Points          []usageHistoryPoint  `json:"points"`
	Resets          []usageHistoryReset  `json:"resets"`
	Forecast        usageHistoryForecast `json:"forecast"`
}

type usageHistoryResponse struct {
	From        string               `json:"from"`
	GeneratedAt string               `json:"generated_at"`
	Series      []usageHistorySeries `json:"series"`
}

type usageSeriesKey struct{ provider, window string }

func collectUsageHistory(since, now time.Time) (usageHistoryResponse, error) {
	// Forecast from the whole retained history so a 24h chart can still anchor
	// a weekly series at a reset that happened several days earlier. Only the
	// plotted points/reset markers are clipped to the requested view below.
	rows, err := db.SubscriptionUsageHistorySince(now.Add(-db.DefaultSubscriptionUsageRetention))
	if err != nil {
		return usageHistoryResponse{}, err
	}
	bySeries := make(map[usageSeriesKey][]db.SubscriptionUsageHistoryRow)
	for _, row := range rows {
		key := usageSeriesKey{provider: row.Provider, window: row.WindowName}
		bySeries[key] = append(bySeries[key], row)
	}
	keys := make([]usageSeriesKey, 0, len(bySeries))
	for key := range bySeries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].window < keys[j].window
	})
	out := usageHistoryResponse{
		From: since.UTC().Format(time.RFC3339Nano), GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Series: make([]usageHistorySeries, 0, len(keys)),
	}
	for _, key := range keys {
		rows := bySeries[key]
		series := usageHistorySeries{
			Provider: key.provider, WindowName: key.window,
			Points: make([]usageHistoryPoint, 0, len(rows)), Resets: make([]usageHistoryReset, 0),
		}
		for _, row := range rows {
			if row.ObservedAt.Before(since) {
				continue
			}
			if row.Duration > 0 {
				series.DurationSeconds = int64(row.Duration / time.Second)
			}
			point := usageHistoryPoint{At: row.ObservedAt.UTC().Format(time.RFC3339Nano), Pct: row.UsedPercent, Source: row.Source}
			if !row.ResetsAt.IsZero() {
				point.ResetsAt = row.ResetsAt.UTC().Format(time.RFC3339Nano)
			}
			series.Points = append(series.Points, point)
		}
		if len(series.Points) == 0 {
			continue
		}
		series.Forecast, series.Resets = forecastUsage(rows)
		series.Resets = resetMarkersSince(series.Resets, since)
		out.Series = append(out.Series, series)
	}
	return out, nil
}

func resetMarkersSince(resets []usageHistoryReset, since time.Time) []usageHistoryReset {
	out := make([]usageHistoryReset, 0, len(resets))
	for _, reset := range resets {
		at, err := time.Parse(time.RFC3339Nano, reset.At)
		if err == nil && !at.Before(since) {
			out = append(out, reset)
		}
	}
	return out
}

// forecastUsage treats provider-declared reset boundaries and meaningful
// downward steps as change points. The latter catches out-of-cycle resets; the
// new segment starts at the observed post-reset minimum rather than inventing
// a 0% sample. A least-squares slope anchored at that baseline then estimates
// the current segment's consumption rate.
func forecastUsage(points []db.SubscriptionUsageHistoryRow) (usageHistoryForecast, []usageHistoryReset) {
	if len(points) == 0 {
		return usageHistoryForecast{Status: "insufficient"}, []usageHistoryReset{}
	}
	segmentStart := 0
	resets := make([]usageHistoryReset, 0)
	for i := 1; i < len(points); i++ {
		prev, next := points[i-1], points[i]
		crossedKnownReset := !prev.ResetsAt.IsZero() && prev.ObservedAt.Before(prev.ResetsAt) && !next.ObservedAt.Before(prev.ResetsAt)
		unexpectedDrop := prev.UsedPercent-next.UsedPercent >= usageResetDropPercent
		if crossedKnownReset || unexpectedDrop {
			segmentStart = i
			resets = append(resets, usageHistoryReset{
				At: next.ObservedAt.UTC().Format(time.RFC3339Nano), Pct: next.UsedPercent,
			})
		}
	}
	segment := points[segmentStart:]
	first, last := segment[0], segment[len(segment)-1]
	forecast := usageHistoryForecast{
		Status: "insufficient", SegmentStartedAt: first.ObservedAt.UTC().Format(time.RFC3339Nano),
		BaselinePct: first.UsedPercent, SampleCount: len(segment),
	}
	if !last.ResetsAt.IsZero() {
		forecast.ResetAt = last.ResetsAt.UTC().Format(time.RFC3339Nano)
	}
	if last.UsedPercent >= 100 {
		forecast.Status = "limit"
		forecast.HitsLimitAt = last.ObservedAt.UTC().Format(time.RFC3339Nano)
		return forecast, resets
	}
	if len(segment) < usageForecastMinSamples || last.ObservedAt.Sub(first.ObservedAt) < usageForecastMinElapsed {
		return forecast, resets
	}
	baseline := first.UsedPercent
	maxPct := baseline
	var numerator, denominator float64
	for _, point := range segment[1:] {
		hours := point.ObservedAt.Sub(first.ObservedAt).Hours()
		if hours <= 0 {
			continue
		}
		maxPct = math.Max(maxPct, point.UsedPercent)
		numerator += hours * math.Max(0, maxPct-baseline)
		denominator += hours * hours
	}
	if denominator == 0 {
		return forecast, resets
	}
	rate := numerator / denominator
	if rate < 0.01 {
		forecast.Status = "flat"
		return forecast, resets
	}
	forecast.RatePctPerHour = rate
	hitAt := last.ObservedAt.Add(time.Duration((100 - last.UsedPercent) / rate * float64(time.Hour)))
	forecast.HitsLimitAt = hitAt.UTC().Format(time.RFC3339Nano)
	if last.ResetsAt.IsZero() {
		forecast.Status = "projected"
	} else if hitAt.Before(last.ResetsAt) {
		forecast.Status = "before_reset"
	} else {
		forecast.Status = "after_reset"
	}
	return forecast, resets
}

func handleDashboardUsageHistory(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hours := defaultUsageHistoryHours
	if raw := r.URL.Query().Get("hours"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxUsageHistoryHours {
			http.Error(w, "bad hours, want 1..2160", http.StatusBadRequest)
			return
		}
		hours = parsed
	}
	now := time.Now()
	out, err := collectUsageHistory(now.Add(-time.Duration(hours)*time.Hour), now)
	if err != nil {
		http.Error(w, "collect usage history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
