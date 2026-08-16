package agentd

import (
	"encoding/json"
	"errors"
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
	usageForecastRecentCount = 5
	maxUsageChartPoints      = 1200
	maxUsageResetMarkers     = 500
	maxUsageSpanOverrides    = 100
)

// openCodeCoverageGrace is how long a sample may be overdue before the next
// one stops counting as in flight. One and a half sampling intervals keeps the
// warning quiet while sampling is merely trailing an active OpenCode session,
// so the warning reads as "sampling has fallen behind the work" rather than "a
// sample is due".
const openCodeCoverageGrace = 3 * db.SubscriptionUsageSampleInterval / 2

type usageHistoryPoint struct {
	At       string  `json:"at"`
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resets_at,omitempty"`
	Source   string  `json:"source,omitempty"`
	Excluded bool    `json:"excluded,omitempty"`
}

type usageHistoryReset struct {
	At  string  `json:"at"`
	Pct float64 `json:"pct"`
}

// Forecast algorithms. Each derives a pace from a different slice of the
// current post-reset segment; the operator picks one per graph and the wire
// carries all of them so switching needs no refetch.
const (
	// usageForecastAlgoSpan draws a straight line through the first and last
	// included sample inside the graph's own view. It is the default: it is the
	// pace the operator can read off the chart with a ruler, and narrowing the
	// history span is how they ask for a more recent pace.
	usageForecastAlgoSpan = "span"
	// usageForecastAlgoRecent uses only the newest few samples, so a burst that
	// started minutes ago dominates instead of being averaged against idle time.
	usageForecastAlgoRecent = "recent"
	// usageForecastAlgoFit is the original least-squares slope over the whole
	// post-reset segment, anchored at the post-reset baseline. It is the
	// smoothest and the slowest to react; long idle stretches hold it down.
	usageForecastAlgoFit = "fit"

	usageForecastDefaultAlgo = usageForecastAlgoSpan
)

var usageForecastAlgos = []string{usageForecastAlgoSpan, usageForecastAlgoRecent, usageForecastAlgoFit}

type usageHistoryForecast struct {
	Algorithm        string  `json:"algorithm,omitempty"`
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
	// Forecast is the default algorithm's prediction; Forecasts carries every
	// algorithm keyed by name so the dashboard can switch without a refetch.
	Forecast  usageHistoryForecast            `json:"forecast"`
	Forecasts map[string]usageHistoryForecast `json:"forecasts"`
}

type usageHistoryResponse struct {
	From             string                 `json:"from"`
	GeneratedAt      string                 `json:"generated_at"`
	Series           []usageHistorySeries   `json:"series"`
	CoverageWarnings []usageCoverageWarning `json:"coverage_warnings"`
}

type usageCoverageWarning struct {
	Provider     string   `json:"provider"`
	NativeSource string   `json:"native_source,omitempty"`
	Models       []string `json:"models"`
	From         string   `json:"from"`
	ActivityFrom string   `json:"activity_from"`
	ActivityTo   string   `json:"activity_to"`
	NativeLatest string   `json:"native_latest,omitempty"`
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
		Series: make([]usageHistorySeries, 0, len(keys)), CoverageWarnings: []usageCoverageWarning{},
	}
	for _, key := range keys {
		rows := bySeries[key]
		// Older Copilot samples may contain the CLI account snapshot's raw
		// timestamp_utc mislabeled as resetDate. The allowance boundary is
		// documented independently: the first day of the next calendar month at
		// 00:00 UTC. Derive it from each observation so old and new history agree.
		if key.provider == db.SubscriptionProviderGitHub {
			for i := range rows {
				rows[i].ResetsAt = copilotMonthlyResetAt(rows[i].ObservedAt)
			}
		}
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
		includedRows := make([]db.SubscriptionUsageHistoryRow, 0, len(rows))
		for _, row := range rows {
			if row.Duration > 0 {
				series.DurationSeconds = int64(row.Duration / time.Second)
			}
			if row.ObservedAt.Before(seriesFrom) {
				if !row.Excluded {
					includedRows = append(includedRows, row)
				}
				continue
			}
			visibleRows = append(visibleRows, row)
			if !row.Excluded {
				includedRows = append(includedRows, row)
			}
		}
		// Excluded observations remain display data, but are absent from every
		// derived value: reset detection, reset timing, pace, and prediction.
		series.Forecasts, series.Resets = forecastUsage(includedRows, now, seriesFrom)
		series.Forecast = series.Forecasts[usageForecastDefaultAlgo]
		series.Resets = resetMarkersSince(series.Resets, seriesFrom)
		series.ResetCount = len(series.Resets)
		series.Resets = downsampleUsageResets(series.Resets, maxUsageResetMarkers)
		visibleRows = downsampleUsageRows(visibleRows, series.Resets, maxUsageChartPoints)
		series.Points = make([]usageHistoryPoint, 0, len(visibleRows))
		for _, row := range visibleRows {
			point := usageHistoryPoint{At: row.ObservedAt.UTC().Format(time.RFC3339Nano), Pct: row.UsedPercent, Source: row.Source, Excluded: row.Excluded}
			if !row.ResetsAt.IsZero() {
				point.ResetsAt = row.ResetsAt.UTC().Format(time.RFC3339Nano)
			}
			series.Points = append(series.Points, point)
		}
		out.Series = append(out.Series, series)
	}
	coverageFrom := make(map[string]time.Time)
	for key := range bySeries {
		effective := since
		if override, ok := seriesSince[key]; ok {
			effective = override
		}
		if prior, ok := coverageFrom[key.provider]; !ok || effective.Before(prior) {
			// A provider-level warning qualifies every visible card for that
			// provider, so its range is the union of those cards' spans.
			coverageFrom[key.provider] = effective
		}
	}
	out.CoverageWarnings, err = collectOpenCodeUsageCoverageWarnings(since, coverageFrom, now, rows)
	if err != nil {
		return usageHistoryResponse{}, err
	}
	return out, nil
}

func openCodeNativeUsageSource(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case db.SubscriptionProviderOpenAI:
		return db.SubscriptionProviderOpenAI
	case db.SubscriptionProviderAnthropic:
		return db.SubscriptionProviderAnthropic
	default:
		return ""
	}
}

func collectOpenCodeUsageCoverageWarnings(
	defaultFrom time.Time,
	providerFrom map[string]time.Time,
	to time.Time,
	nativeRows []db.SubscriptionUsageHistoryRow,
) ([]usageCoverageWarning, error) {
	queryFrom := defaultFrom
	for _, from := range providerFrom {
		if from.Before(queryFrom) {
			queryFrom = from
		}
	}
	activity, err := db.OpenCodeUsageActivityBetween(queryFrom, to)
	if err != nil {
		return nil, err
	}
	type activityGroup struct {
		models map[string]struct{}
		first  time.Time
		last   time.Time
	}
	byProvider := map[string]*activityGroup{}
	for _, row := range activity {
		provider := strings.TrimSpace(row.ProviderID)
		if provider == "" {
			continue
		}
		from := defaultFrom
		if nativeSource := openCodeNativeUsageSource(provider); nativeSource != "" {
			if selected, ok := providerFrom[nativeSource]; ok {
				from = selected
			}
		}
		if row.ObservedAt.Before(from) {
			continue
		}
		group := byProvider[provider]
		if group == nil {
			group = &activityGroup{models: map[string]struct{}{}, first: row.ObservedAt, last: row.ObservedAt}
			byProvider[provider] = group
		}
		group.models[row.ModelID] = struct{}{}
		if row.ObservedAt.Before(group.first) {
			group.first = row.ObservedAt
		}
		if row.ObservedAt.After(group.last) {
			group.last = row.ObservedAt
		}
	}
	// Only the newest native sample per provider matters: samples are absolute
	// readings of the account quota rather than deltas, so anything plotted at
	// or after an OpenCode turn already contains that turn's spend.
	latestNative := map[string]time.Time{}
	for _, row := range nativeRows {
		if row.Excluded {
			continue
		}
		from := defaultFrom
		if selected, ok := providerFrom[row.Provider]; ok {
			from = selected
		}
		if row.ObservedAt.Before(from) || row.ObservedAt.After(to) {
			continue
		}
		if row.ObservedAt.After(latestNative[row.Provider]) {
			latestNative[row.Provider] = row.ObservedAt
		}
	}
	providers := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	out := make([]usageCoverageWarning, 0)
	for _, provider := range providers {
		nativeSource := openCodeNativeUsageSource(provider)
		group := byProvider[provider]
		var native time.Time
		if nativeSource != "" {
			native = latestNative[nativeSource]
			if openCodeActivityCoveredByNativeSamples(group.last, native, to) {
				continue
			}
		}
		models := make([]string, 0, len(group.models))
		for model := range group.models {
			models = append(models, model)
		}
		sort.Strings(models)
		from := defaultFrom
		if nativeSource != "" {
			if selected, ok := providerFrom[nativeSource]; ok {
				from = selected
			}
		}
		warning := usageCoverageWarning{
			Provider: provider, NativeSource: nativeSource, Models: models,
			From:         from.UTC().Format(time.RFC3339Nano),
			ActivityFrom: group.first.UTC().Format(time.RFC3339Nano),
			ActivityTo:   group.last.UTC().Format(time.RFC3339Nano),
		}
		if !native.IsZero() {
			warning.NativeLatest = native.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, warning)
	}
	return out, nil
}

// openCodeActivityCoveredByNativeSamples reports whether the newest native
// sample can be trusted to already include the newest OpenCode turn.
//
// Each native sample is an absolute reading of the provider account's quota,
// not a delta, so a sample taken after an OpenCode turn contains that turn's
// spend and every plotted point stays true regardless of how the sampler
// behaved earlier in the span. A gap in the middle only blurs where between
// two correct readings the consumption landed. What genuinely leaves the
// graphs incomplete is activity that outruns the newest sample.
//
// The grace is anchored on now rather than on the activity: forgiving an
// uncovered turn because a sample was due shortly after it would forgive it
// forever, including the case this warning exists for — the OpenAI series only
// advances when Codex itself runs, so once sampling stops, the sample that
// would have covered that turn is never coming.
func openCodeActivityCoveredByNativeSamples(latestActivity, latestNative, now time.Time) bool {
	if latestActivity.IsZero() || latestNative.IsZero() {
		return false
	}
	if !latestActivity.After(latestNative) {
		return true
	}
	return !now.After(latestNative.Add(openCodeCoverageGrace))
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

// downsampleUsageRows normally bounds the chart wire shape while retaining
// the first/latest observation, both sides of every displayed reset, and all
// explicitly excluded observations so they remain reversible. Forecasts are
// computed from the full included rows before this display-only reduction.
func downsampleUsageRows(rows []db.SubscriptionUsageHistoryRow, resets []usageHistoryReset, max int) []db.SubscriptionUsageHistoryRow {
	if max < 2 || len(rows) <= max {
		return rows
	}
	required := map[int]bool{0: true, len(rows) - 1: true}
	firstIncluded, lastIncluded := -1, -1
	for i, row := range rows {
		if row.Excluded {
			continue
		}
		if firstIncluded < 0 {
			firstIncluded = i
		}
		lastIncluded = i
	}
	if firstIncluded >= 0 {
		required[firstIncluded] = true
		required[lastIncluded] = true
	}
	resetAt := make(map[string]struct{}, len(resets))
	for _, reset := range resets {
		resetAt[reset.At] = struct{}{}
	}
	for i, row := range rows {
		if row.Excluded {
			// Explicitly excluded points must stay on the chart so the operator
			// can reverse the decision even when ordinary samples are reduced.
			required[i] = true
		}
		at := row.ObservedAt.UTC().Format(time.RFC3339Nano)
		if _, ok := resetAt[at]; ok {
			required[i] = true
			for previous := i - 1; previous >= 0; previous-- {
				if !rows[previous].Excluded {
					required[previous] = true
					break
				}
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
// a 0% sample. Every algorithm then estimates a pace from some slice of that
// current segment; see usageForecastAlgos for what each one looks at.
//
// viewFrom is the graph's own history span. Only the default `span` algorithm
// honours it — the operator narrowing a card to 24h is how they ask that card's
// prediction to ignore what happened before that.
//
// A forecast is paused once its declared reset passes or its newest sample is
// older than usageForecastStaleAfter; retained history must not read as live.
func forecastUsage(points []db.SubscriptionUsageHistoryRow, now, viewFrom time.Time) (map[string]usageHistoryForecast, []usageHistoryReset) {
	if len(points) == 0 {
		out := make(map[string]usageHistoryForecast, len(usageForecastAlgos))
		for _, algo := range usageForecastAlgos {
			out[algo] = usageHistoryForecast{Algorithm: algo, Status: "insufficient"}
		}
		return out, []usageHistoryReset{}
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
	out := make(map[string]usageHistoryForecast, len(usageForecastAlgos))
	for _, algo := range usageForecastAlgos {
		out[algo] = forecastUsageSegment(algo, segment, now, viewFrom)
	}
	return out, resets
}

// usageForecastSamples is the slice of the current segment an algorithm reads.
// It always ends on the newest sample: every algorithm anchors its line at the
// current value and differs only in how far back it looks to find a slope.
func usageForecastSamples(algo string, segment []db.SubscriptionUsageHistoryRow, viewFrom time.Time) []db.SubscriptionUsageHistoryRow {
	switch algo {
	case usageForecastAlgoSpan:
		for i, point := range segment {
			if !point.ObservedAt.Before(viewFrom) {
				return segment[i:]
			}
		}
		return nil
	case usageForecastAlgoRecent:
		start := max(0, len(segment)-usageForecastRecentCount)
		last := segment[len(segment)-1].ObservedAt
		// A burst can produce several samples inside a few minutes, which would
		// leave the newest few spanning less than the minimum elapsed time and
		// report nothing at all. Reach further back until there is enough of a
		// baseline to divide by.
		for start > 0 && last.Sub(segment[start].ObservedAt) < usageForecastMinElapsed {
			start--
		}
		return segment[start:]
	default:
		return segment
	}
}

// forecastUsageSegment reports one algorithm's view of the current segment.
// Status, the declared reset and the staleness/limit gates are shared: they
// describe the samples themselves rather than any particular pace.
func forecastUsageSegment(algo string, segment []db.SubscriptionUsageHistoryRow, now, viewFrom time.Time) usageHistoryForecast {
	samples := usageForecastSamples(algo, segment, viewFrom)
	last := segment[len(segment)-1]
	forecast := usageHistoryForecast{Algorithm: algo, Status: "insufficient", SampleCount: len(samples)}
	if len(samples) > 0 {
		forecast.SegmentStartedAt = samples[0].ObservedAt.UTC().Format(time.RFC3339Nano)
		forecast.BaselinePct = samples[0].UsedPercent
	}
	if !last.ResetsAt.IsZero() {
		forecast.ResetAt = last.ResetsAt.UTC().Format(time.RFC3339Nano)
	}
	knownResetPassed := !last.ResetsAt.IsZero() && !now.Before(last.ResetsAt)
	observationStale := now.Sub(last.ObservedAt) > usageForecastStaleAfter
	if knownResetPassed || observationStale {
		forecast.Status = "stale"
		return forecast
	}
	if last.UsedPercent >= 100 {
		forecast.Status = "limit"
		forecast.HitsLimitAt = last.ObservedAt.UTC().Format(time.RFC3339Nano)
		return forecast
	}
	if len(samples) < usageForecastMinSamples || last.ObservedAt.Sub(samples[0].ObservedAt) < usageForecastMinElapsed {
		return forecast
	}
	rate, ok := usageForecastRate(algo, samples)
	if !ok {
		return forecast
	}
	if rate < 0.01 {
		forecast.Status = "flat"
		return forecast
	}
	forecast.RatePctPerHour = rate
	hitAt := last.ObservedAt.Add(time.Duration((100 - last.UsedPercent) / rate * float64(time.Hour)))
	forecast.HitsLimitAt = hitAt.UTC().Format(time.RFC3339Nano)
	switch {
	case last.ResetsAt.IsZero():
		forecast.Status = "projected"
	case hitAt.Before(last.ResetsAt):
		forecast.Status = "before_reset"
	default:
		forecast.Status = "after_reset"
	}
	return forecast
}

// usageForecastRate turns the chosen samples into percentage points per hour.
// The straight-line algorithms report the average pace actually observed
// between their two endpoints — the slope a ruler laid on the chart would
// give. `fit` keeps the original least-squares slope through the baseline,
// which is smoother but lags a change in pace by roughly the segment's length.
func usageForecastRate(algo string, samples []db.SubscriptionUsageHistoryRow) (float64, bool) {
	first, last := samples[0], samples[len(samples)-1]
	if algo != usageForecastAlgoFit {
		hours := last.ObservedAt.Sub(first.ObservedAt).Hours()
		if hours <= 0 {
			return 0, false
		}
		return math.Max(0, last.UsedPercent-first.UsedPercent) / hours, true
	}
	baseline := first.UsedPercent
	maxPct := baseline
	var numerator, denominator float64
	for _, point := range samples[1:] {
		hours := point.ObservedAt.Sub(first.ObservedAt).Hours()
		if hours <= 0 {
			continue
		}
		maxPct = math.Max(maxPct, point.UsedPercent)
		numerator += hours * math.Max(0, maxPct-baseline)
		denominator += hours * hours
	}
	if denominator == 0 {
		return 0, false
	}
	return numerator / denominator, true
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

type usageHistoryExclusionBody struct {
	Provider   string `json:"provider"`
	WindowName string `json:"window_name"`
	At         string `json:"at"`
	Excluded   *bool  `json:"excluded"`
}

func handleDashboardUsageHistoryPoint(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body usageHistoryExclusionBody
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "bad exclusion body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Excluded == nil {
		http.Error(w, "excluded is required", http.StatusBadRequest)
		return
	}
	at, err := time.Parse(time.RFC3339Nano, body.At)
	if err != nil {
		http.Error(w, "at must be an RFC3339 timestamp", http.StatusBadRequest)
		return
	}
	if err := db.SetSubscriptionUsagePointExcluded(body.Provider, body.WindowName, at, *body.Excluded); err != nil {
		if errors.Is(err, db.ErrSubscriptionUsagePointNotFound) {
			http.Error(w, "usage point changed or no longer exists; refresh and try again", http.StatusNotFound)
			return
		}
		http.Error(w, "set usage exclusion: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"excluded": *body.Excluded})
}
