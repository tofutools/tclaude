package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// SubscriptionUsageSampleInterval bounds history growth while retaining
	// enough resolution for 5-hour consumption forecasts.
	SubscriptionUsageSampleInterval = 15 * time.Minute
	// DefaultSubscriptionUsageRetention keeps several weekly cycles without
	// allowing the history tables to grow indefinitely.
	DefaultSubscriptionUsageRetention = 90 * 24 * time.Hour

	SubscriptionProviderAnthropic = "anthropic"
	SubscriptionProviderOpenAI    = "openai"
)

// SubscriptionUsageWindow is one rate-limit window in an account-wide
// subscription reading. Name is a stable provider-independent identifier
// such as "five_hour" or "seven_day". Duration may be zero when the source
// does not report a window length.
type SubscriptionUsageWindow struct {
	Name        string
	Duration    time.Duration
	UsedPercent float64
	ResetsAt    time.Time
}

// SubscriptionUsageSample is an account-wide reading captured from a
// provider. ObservedAt is the timestamp supplied by (or assigned to) the
// source; storage derives the 15-minute SampledAt bucket from it.
type SubscriptionUsageSample struct {
	Provider   string
	ObservedAt time.Time
	Source     string
	Windows    []SubscriptionUsageWindow
}

// SubscriptionUsageHistoryRow is one retained provider/window observation.
// ObservedAt, rather than the coalescing bucket's SampledAt, is the chart's
// time coordinate so replacements within a bucket keep their real timestamp.
type SubscriptionUsageHistoryRow struct {
	Provider    string
	WindowName  string
	Duration    time.Duration
	UsedPercent float64
	ResetsAt    time.Time
	ObservedAt  time.Time
	Source      string
	Excluded    bool
}

// ErrSubscriptionUsagePointNotFound means the observation changed or was
// pruned after the dashboard loaded it. Callers should refresh rather than
// applying the requested flag to a different point in the same sample bucket.
var ErrSubscriptionUsagePointNotFound = errors.New("subscription usage point not found")

// SubscriptionUsageHistorySince returns retained observations at or after
// since, ordered so callers can group and walk each provider/window series.
func SubscriptionUsageHistorySince(since time.Time) ([]SubscriptionUsageHistoryRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	bucketCutoff := since.UTC().Truncate(SubscriptionUsageSampleInterval).Format(time.RFC3339Nano)
	observedCutoff := since.UTC().Format(time.RFC3339Nano)
	rows, err := d.Query(`SELECT s.provider, w.window_name, w.duration_seconds,
		w.used_percent, w.resets_at, w.observed_at, w.source, w.excluded
		FROM subscription_usage_samples s
		JOIN subscription_usage_windows w ON w.sample_id = s.id
		WHERE s.sampled_at >= ? AND w.observed_at >= ?
		ORDER BY s.provider, w.window_name, w.observed_at`,
		bucketCutoff, observedCutoff)
	if err != nil {
		return nil, fmt.Errorf("read subscription usage history: %w", err)
	}
	defer rows.Close()

	out := make([]SubscriptionUsageHistoryRow, 0)
	for rows.Next() {
		var row SubscriptionUsageHistoryRow
		var durationSeconds int64
		var resetsAt, observedAt string
		if err := rows.Scan(&row.Provider, &row.WindowName, &durationSeconds,
			&row.UsedPercent, &resetsAt, &observedAt, &row.Source, &row.Excluded); err != nil {
			return nil, fmt.Errorf("read subscription usage history: scan: %w", err)
		}
		row.Duration = time.Duration(durationSeconds) * time.Second
		if resetsAt != "" {
			row.ResetsAt, err = time.Parse(time.RFC3339Nano, resetsAt)
			if err != nil {
				return nil, fmt.Errorf("read subscription usage history: resets_at: %w", err)
			}
		}
		row.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt)
		if err != nil {
			return nil, fmt.Errorf("read subscription usage history: observed_at: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read subscription usage history: rows: %w", err)
	}
	return out, nil
}

// SetSubscriptionUsagePointExcluded changes whether one exact retained
// observation participates in quota calculations. The observed timestamp is
// deliberately part of the predicate: a newer in-bucket replacement must not
// inherit a mutation sent by a stale dashboard.
func SetSubscriptionUsagePointExcluded(provider, windowName string, observedAt time.Time, excluded bool) error {
	provider = strings.TrimSpace(provider)
	windowName = strings.TrimSpace(windowName)
	if provider == "" || windowName == "" || observedAt.IsZero() {
		return fmt.Errorf("set subscription usage exclusion: provider, window, and observed_at are required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`UPDATE subscription_usage_windows SET excluded = ?
		WHERE window_name = ? AND observed_at = ? AND sample_id IN (
			SELECT id FROM subscription_usage_samples WHERE provider = ?
		)`, excluded, windowName, observedAt.UTC().Format(time.RFC3339Nano), provider)
	if err != nil {
		return fmt.Errorf("set subscription usage exclusion: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set subscription usage exclusion: affected rows: %w", err)
	}
	if changed != 1 {
		return ErrSubscriptionUsagePointNotFound
	}
	return nil
}

// SaveSubscriptionUsageSample stores the newest observation in a provider's
// 15-minute UTC bucket. It returns true when a new bucket was inserted or an
// existing bucket was replaced by a newer observation. Invalid/empty samples
// return an error and never disturb history.
//
// Retention is also enforced on every write attempt, including a duplicate,
// so hook/statusline activity naturally keeps the table bounded even when
// agentd is not running continuously.
func SaveSubscriptionUsageSample(sample SubscriptionUsageSample) (bool, error) {
	validated, err := validateSubscriptionUsageSample(sample)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	stored, err := saveSubscriptionUsageSampleTx(tx, validated, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return stored, nil
}

func validateSubscriptionUsageSample(sample SubscriptionUsageSample) (SubscriptionUsageSample, error) {
	sample.Provider = strings.TrimSpace(sample.Provider)
	if sample.Provider == "" {
		return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: provider is required")
	}
	if sample.ObservedAt.IsZero() {
		return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: observed_at is required")
	}
	if len(sample.Windows) == 0 {
		return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: at least one window is required")
	}
	sample.Windows = append([]SubscriptionUsageWindow(nil), sample.Windows...)
	seen := make(map[string]struct{}, len(sample.Windows))
	for i := range sample.Windows {
		w := &sample.Windows[i]
		w.Name = strings.TrimSpace(w.Name)
		name := w.Name
		if name == "" {
			return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: window name is required")
		}
		if _, exists := seen[name]; exists {
			return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: duplicate window %q", name)
		}
		seen[name] = struct{}{}
		if w.Duration < 0 {
			return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: window %q has negative duration", name)
		}
		if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) {
			return SubscriptionUsageSample{}, fmt.Errorf("save subscription usage sample: window %q has non-finite percent", name)
		}
	}
	return sample, nil
}

func saveSubscriptionUsageSampleTx(tx *sql.Tx, sample SubscriptionUsageSample, now time.Time) (bool, error) {
	cutoff := now.Add(-DefaultSubscriptionUsageRetention).Format(time.RFC3339Nano)
	if _, err := tx.Exec(`DELETE FROM subscription_usage_samples WHERE sampled_at < ?`, cutoff); err != nil {
		return false, fmt.Errorf("save subscription usage sample: prune: %w", err)
	}

	observedAt := sample.ObservedAt.UTC()
	sampledAt := observedAt.Truncate(SubscriptionUsageSampleInterval)
	if sampledAt.Before(now.Add(-DefaultSubscriptionUsageRetention)) {
		return false, nil // delayed stale observations must not regrow pruned history.
	}
	sampledStr := sampledAt.Format(time.RFC3339Nano)
	var id int64
	if _, err := tx.Exec(`INSERT OR IGNORE INTO subscription_usage_samples
		(provider, sampled_at) VALUES (?, ?)`, sample.Provider, sampledStr); err != nil {
		return false, fmt.Errorf("save subscription usage sample: insert bucket: %w", err)
	}
	if err := tx.QueryRow(`SELECT id FROM subscription_usage_samples
		WHERE provider = ? AND sampled_at = ?`, sample.Provider, sampledStr).Scan(&id); err != nil {
		return false, fmt.Errorf("save subscription usage sample: find bucket: %w", err)
	}

	stored := false
	for _, w := range sample.Windows {
		var existingObserved string
		err := tx.QueryRow(`SELECT observed_at FROM subscription_usage_windows
			WHERE sample_id = ? AND window_name = ?`, id, w.Name).Scan(&existingObserved)
		switch err {
		case nil:
			if existing, parseErr := time.Parse(time.RFC3339Nano, existingObserved); parseErr == nil && !observedAt.After(existing) {
				continue
			}
		case sql.ErrNoRows:
			// Insert below.
		default:
			return false, fmt.Errorf("save subscription usage sample: find window %q: %w", w.Name, err)
		}
		resetsAt := ""
		if !w.ResetsAt.IsZero() {
			resetsAt = w.ResetsAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.Exec(`INSERT INTO subscription_usage_windows
			(sample_id, window_name, duration_seconds, used_percent, resets_at, observed_at, source)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(sample_id, window_name) DO UPDATE SET
				duration_seconds = excluded.duration_seconds,
				used_percent = excluded.used_percent,
				resets_at = excluded.resets_at,
				observed_at = excluded.observed_at,
				source = excluded.source,
				excluded = 0`,
			id, w.Name, int64(w.Duration/time.Second), w.UsedPercent, resetsAt,
			observedAt.Format(time.RFC3339Nano), sample.Source); err != nil {
			return false, fmt.Errorf("save subscription usage sample: insert window %q: %w", w.Name, err)
		}
		stored = true
	}
	return stored, nil
}

// PruneSubscriptionUsageHistory deletes complete samples older than cutoff.
// Child windows are removed by the ON DELETE CASCADE foreign key.
func PruneSubscriptionUsageHistory(cutoff time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	result, err := d.Exec(`DELETE FROM subscription_usage_samples WHERE sampled_at < ?`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("prune subscription usage history: %w", err)
	}
	return result.RowsAffected()
}
