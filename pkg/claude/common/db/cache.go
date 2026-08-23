package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UsageCacheRow represents a cached usage API response.
type UsageCacheRow struct {
	Data          json.RawMessage // full CachedUsage JSON blob
	FetchedAt     time.Time
	LastAttemptAt time.Time
}

// CodexUsageCacheRow represents the latest Codex rate-limit snapshot lifted
// from a rollout token_count event.
type CodexUsageCacheRow struct {
	Data       json.RawMessage // full harness.CodexUsage JSON blob
	ObservedAt time.Time
	UpdatedAt  time.Time
	Source     string
}

// SaveUsageCache upserts the usage cache row (single-row table, key=1).
func SaveUsageCache(data json.RawMessage, fetchedAt, lastAttemptAt time.Time) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO usage_cache (id, data, fetched_at, last_attempt_at)
		VALUES (1, ?, ?, ?)`,
		string(data),
		nullableDBTime(fetchedAt),
		nullableDBTime(lastAttemptAt))
	return err
}

// LoadUsageCache returns the cached usage data, or nil if not found.
func LoadUsageCache() (*UsageCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var dataStr string
	var fetchedAt, lastAttemptAt dbTimestamp
	err = db.QueryRow(`SELECT data, fetched_at, last_attempt_at FROM usage_cache WHERE id = 1`).
		Scan(&dataStr, &fetchedAt, &lastAttemptAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &UsageCacheRow{
		Data: json.RawMessage(dataStr),
	}
	row.FetchedAt = fetchedAt.Time()
	row.LastAttemptAt = lastAttemptAt.Time()
	return row, nil
}

// DeleteUsageCache removes the usage cache entry.
func DeleteUsageCache() error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM usage_cache WHERE id = 1`)
	return err
}

const codexAccountUsageLimitID = "codex"

// SaveCodexUsageCacheIfNewer stores a Codex usage snapshot when it belongs to
// the selected account-wide quota or its rollout observation timestamp is
// newer than the current cache row. Identity outranks time: an identified
// account snapshot repairs a legacy/unidentified or different-quota row even
// when that row has a newer observation, while an unidentified/different
// snapshot cannot downgrade an identified account row. This lets the parser's
// quota selection survive cache arbitration without comparing percentages or
// inferring quota resets.
//
// When identities are equivalent, equal or older observations cannot regress
// the current readout, but their genuinely observed windows still pass through
// the per-window history freshness gate: an out-of-order weekly-only event may
// fill a history child that a newer five-hour-only cache snapshot did not
// contain.
func SaveCodexUsageCacheIfNewer(data json.RawMessage, observedAt time.Time, source string, historyWindows ...SubscriptionUsageWindow) (bool, error) {
	if observedAt.IsZero() {
		return false, nil
	}
	db, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingData string
	var existingObserved dbTimestamp
	err = tx.QueryRow(`SELECT data, observed_at FROM codex_usage_cache WHERE id = 1`).
		Scan(&existingData, &existingObserved)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	cacheNewer := true
	identityRepair := false
	if err == nil {
		incomingAccount := codexUsageLimitID(data) == codexAccountUsageLimitID
		existingAccount := codexUsageLimitID(json.RawMessage(existingData)) == codexAccountUsageLimitID
		if existingAccount && !incomingAccount {
			// A pre-upgrade hook or other unidentified writer must not
			// re-poison the selected account cache or its history.
			return false, nil
		}
		identityRepair = incomingAccount && !existingAccount
		if !identityRepair && !observedAt.After(existingObserved.Time()) {
			cacheNewer = false
		}
	}
	if !cacheNewer && len(historyWindows) == 0 {
		return false, nil
	}

	now := time.Now()
	if identityRepair {
		// Pre-fix OpenAI history discarded quota identity just like the
		// current-value cache did. Once that cache is proven ambiguous, no
		// value/reset/source heuristic can safely recover individual account
		// points. Drop only the derived OpenAI history and immediately seed it
		// below from the verified account snapshot; Anthropic is independent.
		if _, err = tx.Exec(`DELETE FROM subscription_usage_samples WHERE provider = ?`,
			SubscriptionProviderOpenAI); err != nil {
			return false, fmt.Errorf("save codex usage cache: clear unidentified OpenAI history: %w", err)
		}
	}
	if cacheNewer {
		_, err = tx.Exec(`INSERT OR REPLACE INTO codex_usage_cache (id, data, observed_at, updated_at, source)
			VALUES (1, ?, ?, ?, ?)`,
			string(data),
			dbTime(observedAt.UTC()),
			dbTime(now.UTC()),
			source)
		if err != nil {
			return false, err
		}
	}
	if len(historyWindows) > 0 {
		sample, err := validateSubscriptionUsageSample(SubscriptionUsageSample{
			Provider: SubscriptionProviderOpenAI, ObservedAt: observedAt,
			Source: source, Windows: historyWindows,
		})
		if err != nil {
			return false, err
		}
		if _, err := saveSubscriptionUsageSampleTx(tx, sample, now.UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return cacheNewer, nil
}

func codexUsageLimitID(data json.RawMessage) string {
	var identity struct {
		LimitID      string
		SnakeLimitID string `json:"limit_id"`
	}
	if json.Unmarshal(data, &identity) != nil {
		return ""
	}
	if identity.LimitID != "" {
		return identity.LimitID
	}
	return identity.SnakeLimitID
}

// LoadCodexUsageCache returns the cached Codex usage snapshot, or nil if none
// has been observed yet.
func LoadCodexUsageCache() (*CodexUsageCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var dataStr, source string
	var observedAt, updatedAt dbTimestamp
	err = db.QueryRow(`SELECT data, observed_at, updated_at, source FROM codex_usage_cache WHERE id = 1`).
		Scan(&dataStr, &observedAt, &updatedAt, &source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &CodexUsageCacheRow{
		Data:   json.RawMessage(dataStr),
		Source: source,
	}
	row.ObservedAt = observedAt.Time()
	row.UpdatedAt = updatedAt.Time()
	return row, nil
}

// LoadDashboardUsageCaches reads the single-row Claude and Codex usage caches
// together, the latest GitHub Copilot monthly quota, and whether retained
// subscription history exists. The dashboard needs all four on every snapshot;
// one query avoids extra pool checkouts and SQLite round trips while preserving
// every source as independently optional.
func LoadDashboardUsageCaches() (*UsageCacheRow, *CodexUsageCacheRow, *SubscriptionUsageHistoryRow, bool, error) {
	d, err := Open()
	if err != nil {
		return nil, nil, nil, false, err
	}
	var usageData sql.NullString
	var fetchedAt, lastAttemptAt dbTimestamp
	var codexData, source sql.NullString
	var observedAt, updatedAt dbTimestamp
	var copilotProvider, copilotWindow, copilotSource sql.NullString
	var copilotDuration sql.NullInt64
	var copilotPercent sql.NullFloat64
	var copilotResetsAt, copilotObservedAt dbTimestamp
	var hasHistory bool
	historyCutoff := dbTime(time.Now().UTC().Add(-DefaultSubscriptionUsageRetention))
	err = d.QueryRow(`SELECT
			u.data, u.fetched_at, u.last_attempt_at,
			c.data, c.observed_at, c.updated_at, c.source,
			g.provider, g.window_name, g.duration_seconds, g.used_percent,
			g.resets_at, g.observed_at, g.source,
			EXISTS(SELECT 1 FROM subscription_usage_samples WHERE sampled_at >= ? LIMIT 1)
		FROM (SELECT 1) singleton
		LEFT JOIN usage_cache u ON u.id = 1
		LEFT JOIN codex_usage_cache c ON c.id = 1
		LEFT JOIN (
			SELECT s.provider, w.window_name, w.duration_seconds, w.used_percent,
				w.resets_at, w.observed_at, w.source
			FROM subscription_usage_samples s
			JOIN subscription_usage_windows w ON w.sample_id = s.id
			WHERE s.provider = ? AND w.window_name = 'monthly'
			ORDER BY w.observed_at DESC LIMIT 1
		) g ON TRUE`, historyCutoff, SubscriptionProviderGitHub).Scan(
		&usageData, &fetchedAt, &lastAttemptAt,
		&codexData, &observedAt, &updatedAt, &source,
		&copilotProvider, &copilotWindow, &copilotDuration, &copilotPercent,
		&copilotResetsAt, &copilotObservedAt, &copilotSource,
		&hasHistory)
	if err != nil {
		return nil, nil, nil, false, err
	}
	var usage *UsageCacheRow
	if usageData.Valid {
		usage = &UsageCacheRow{Data: json.RawMessage(usageData.String)}
		usage.FetchedAt = fetchedAt.Time()
		usage.LastAttemptAt = lastAttemptAt.Time()
	}
	var codex *CodexUsageCacheRow
	if codexData.Valid {
		codex = &CodexUsageCacheRow{
			Data:   json.RawMessage(codexData.String),
			Source: source.String,
		}
		codex.ObservedAt = observedAt.Time()
		codex.UpdatedAt = updatedAt.Time()
	}
	var copilot *SubscriptionUsageHistoryRow
	if copilotProvider.Valid && copilotWindow.Valid && copilotPercent.Valid {
		copilot = &SubscriptionUsageHistoryRow{
			Provider: copilotProvider.String, WindowName: copilotWindow.String,
			Duration:    time.Duration(copilotDuration.Int64) * time.Second,
			UsedPercent: copilotPercent.Float64, ResetsAt: copilotResetsAt.Time(),
			ObservedAt: copilotObservedAt.Time(), Source: copilotSource.String,
		}
	}
	return usage, codex, copilot, hasHistory, nil
}

// GitCacheRow represents cached git/PR data for a repository.
type GitCacheRow struct {
	Data      json.RawMessage // full GitSnapshot JSON blob
	FetchedAt time.Time
}

// SaveGitCache upserts a git cache row keyed by repo hash.
func SaveGitCache(repoHash string, data json.RawMessage, fetchedAt time.Time) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO git_cache (repo_hash, data, fetched_at)
		VALUES (?, ?, ?)`,
		repoHash, string(data), dbTime(fetchedAt))
	return err
}

// ListGitCacheByPrefixSince returns git_cache rows whose key starts with
// prefix and were fetched at or after since, keyed by full repo hash. The
// daemon-wide merged-PR poller uses it to enumerate recently refreshed
// branch-link (`bl_`) resolutions without loading the whole table.
func ListGitCacheByPrefixSince(prefix string, since time.Time) (map[string]*GitCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
	rows, err := db.Query(`SELECT repo_hash, data, fetched_at FROM git_cache
		WHERE repo_hash LIKE ? ESCAPE '\' AND fetched_at >= ?`,
		pattern, dbTimeBoundary(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*GitCacheRow{}
	for rows.Next() {
		var key, dataStr string
		var fetchedAt dbTimestamp
		if err := rows.Scan(&key, &dataStr, &fetchedAt); err != nil {
			return nil, err
		}
		row := &GitCacheRow{Data: json.RawMessage(dataStr)}
		row.FetchedAt = fetchedAt.Time()
		out[key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadGitCache returns cached git data for a repo, or nil if not found.
func LoadGitCache(repoHash string) (*GitCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var dataStr string
	var fetchedAt dbTimestamp
	err = db.QueryRow(`SELECT data, fetched_at FROM git_cache WHERE repo_hash = ?`, repoHash).
		Scan(&dataStr, &fetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &GitCacheRow{
		Data: json.RawMessage(dataStr),
	}
	row.FetchedAt = fetchedAt.Time()
	return row, nil
}
