package agentd

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	defaultUsageHistoryHours = 7 * 24
	maxUsageHistoryHours     = 90 * 24
	usageResetDropPercent    = 2.0
	usageForecastMinSamples  = 3
	usageForecastMinElapsed  = 30 * time.Minute
	usageForecastStaleAfter  = 2 * time.Hour
	maxUsageChartPoints      = 1200
	maxUsageResetMarkers     = 500
	maxUsageSpanOverrides    = 100
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
	From            string               `json:"from"`
	Points          []usageHistoryPoint  `json:"points"`
	Resets          []usageHistoryReset  `json:"resets"`
	ResetCount      int                  `json:"reset_count"`
	Forecast        usageHistoryForecast `json:"forecast"`
}

type usageHistoryResponse struct {
	From        string               `json:"from"`
	GeneratedAt string               `json:"generated_at"`
	Series      []usageHistorySeries `json:"series"`
}

type usageSeriesKey struct{ provider, window string }

// collectUsageHistory builds one series per provider × quota window. Each
// series is clipped to its own view start: the per-series override when the
// request carried one, the shared default otherwise. Series with retained
// rows but none inside their view are kept with empty points so a card whose
// span the operator narrowed past its data never loses its span controls.
func collectUsageHistory(since time.Time, seriesSince map[usageSeriesKey]time.Time, now time.Time) (usageHistoryResponse, error) {
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
		seriesFrom := since
		if override, ok := seriesSince[key]; ok {
			seriesFrom = override
		}
		series := usageHistorySeries{
			Provider: key.provider, WindowName: key.window,
			From:   seriesFrom.UTC().Format(time.RFC3339Nano),
			Points: make([]usageHistoryPoint, 0), Resets: make([]usageHistoryReset, 0),
		}
		visibleRows := make([]db.SubscriptionUsageHistoryRow, 0, len(rows))
		for _, row := range rows {
			if row.Duration > 0 {
				series.DurationSeconds = int64(row.Duration / time.Second)
			}
			if row.ObservedAt.Before(seriesFrom) {
				continue
			}
			visibleRows = append(visibleRows, row)
		}
		series.Forecast, series.Resets = forecastUsage(rows, now)
		series.Resets = resetMarkersSince(series.Resets, seriesFrom)
		series.ResetCount = len(series.Resets)
		series.Resets = downsampleUsageResets(series.Resets, maxUsageResetMarkers)
		visibleRows = downsampleUsageRows(visibleRows, series.Resets, maxUsageChartPoints)
		series.Points = make([]usageHistoryPoint, 0, len(visibleRows))
		for _, row := range visibleRows {
			point := usageHistoryPoint{At: row.ObservedAt.UTC().Format(time.RFC3339Nano), Pct: row.UsedPercent, Source: row.Source}
			if !row.ResetsAt.IsZero() {
				point.ResetsAt = row.ResetsAt.UTC().Format(time.RFC3339Nano)
			}
			series.Points = append(series.Points, point)
		}
		out.Series = append(out.Series, series)
	}
	return out, nil
}

func downsampleUsageResets(resets []usageHistoryReset, max int) []usageHistoryReset {
	if max < 2 || len(resets) <= max {
		return resets
	}
	stride := int(math.Ceil(float64(len(resets)-1) / float64(max-1)))
	out := make([]usageHistoryReset, 0, max)
	for i := 0; i < len(resets)-1; i += stride {
		out = append(out, resets[i])
	}
	return append(out, resets[len(resets)-1])
}

// downsampleUsageRows bounds the chart wire shape while retaining the first
// and latest observation plus both sides of every displayed reset. Forecasts
// are computed from the full rows before this display-only reduction.
func downsampleUsageRows(rows []db.SubscriptionUsageHistoryRow, resets []usageHistoryReset, max int) []db.SubscriptionUsageHistoryRow {
	if max < 2 || len(rows) <= max {
		return rows
	}
	required := map[int]bool{0: true, len(rows) - 1: true}
	resetAt := make(map[string]struct{}, len(resets))
	for _, reset := range resets {
		resetAt[reset.At] = struct{}{}
	}
	for i, row := range rows {
		at := row.ObservedAt.UTC().Format(time.RFC3339Nano)
		if _, ok := resetAt[at]; ok {
			required[i] = true
			if i > 0 {
				required[i-1] = true
			}
		}
	}
	remaining := max - len(required)
	if remaining > 0 {
		stride := int(math.Ceil(float64(len(rows)) / float64(remaining)))
		for i := 0; i < len(rows) && len(required) < max; i += stride {
			required[i] = true
		}
	}
	indices := make([]int, 0, len(required))
	for index := range required {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]db.SubscriptionUsageHistoryRow, 0, len(indices))
	for _, index := range indices {
		out = append(out, rows[index])
	}
	return out
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
// A forecast is paused once its declared reset passes or its newest sample is
// older than usageForecastStaleAfter; retained history must not read as live.
func forecastUsage(points []db.SubscriptionUsageHistoryRow, now time.Time) (usageHistoryForecast, []usageHistoryReset) {
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
	knownResetPassed := !last.ResetsAt.IsZero() && !now.Before(last.ResetsAt)
	observationStale := now.Sub(last.ObservedAt) > usageForecastStaleAfter
	if knownResetPassed || observationStale {
		forecast.Status = "stale"
		return forecast, resets
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

// parseUsageHistorySpans parses the `spans` query parameter: a comma-separated
// list of provider:window:hours per-series view overrides, e.g.
// "anthropic:seven_day:24,openai:five_hour:720". Entries for series that do
// not exist are harmless; they simply match nothing.
func parseUsageHistorySpans(raw string, now time.Time) (map[usageSeriesKey]time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ",")
	if len(entries) > maxUsageSpanOverrides {
		return nil, fmt.Errorf("too many span overrides, max %d", maxUsageSpanOverrides)
	}
	out := make(map[usageSeriesKey]time.Time, len(entries))
	for _, entry := range entries {
		parts := strings.Split(entry, ":")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("bad span %q, want provider:window:hours", entry)
		}
		hours, err := strconv.Atoi(parts[2])
		if err != nil || hours < 1 || hours > maxUsageHistoryHours {
			return nil, fmt.Errorf("bad span hours in %q, want 1..%d", entry, maxUsageHistoryHours)
		}
		out[usageSeriesKey{provider: parts[0], window: parts[1]}] = now.Add(-time.Duration(hours) * time.Hour)
	}
	return out, nil
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
	seriesSince, err := parseUsageHistorySpans(r.URL.Query().Get("spans"), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := collectUsageHistory(now.Add(-time.Duration(hours)*time.Hour), seriesSince, now)
	if err != nil {
		http.Error(w, "collect usage history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
