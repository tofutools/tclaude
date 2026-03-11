package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// UsageCacheRow represents a cached usage API response.
type UsageCacheRow struct {
	Data          json.RawMessage // full CachedUsage JSON blob
	FetchedAt     time.Time
	LastAttemptAt time.Time
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
		fetchedAt.Format(time.RFC3339Nano),
		lastAttemptAt.Format(time.RFC3339Nano))
	return err
}

// LoadUsageCache returns the cached usage data, or nil if not found.
func LoadUsageCache() (*UsageCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var dataStr, fetchedStr, attemptStr string
	err = db.QueryRow(`SELECT data, fetched_at, last_attempt_at FROM usage_cache WHERE id = 1`).
		Scan(&dataStr, &fetchedStr, &attemptStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &UsageCacheRow{
		Data: json.RawMessage(dataStr),
	}
	row.FetchedAt, _ = time.Parse(time.RFC3339Nano, fetchedStr)
	row.LastAttemptAt, _ = time.Parse(time.RFC3339Nano, attemptStr)
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

// TryClaimUsageFetch atomically checks whether a fetch is needed (last_attempt_at
// older than ttl) and stamps the current time if so. Returns true if the caller
// should proceed with the fetch, false if another process already claimed it.
// This replaces the file-based mutex for usage API rate limiting.
// Crash-safe: if the caller crashes after claiming, the TTL expires naturally.
func TryClaimUsageFetch(ttl time.Duration) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}

	cutoff := time.Now().Add(-ttl).Format(time.RFC3339Nano)
	now := time.Now().Format(time.RFC3339Nano)

	// Try to claim: update only if stale or missing
	result, err := db.Exec(`UPDATE usage_cache SET last_attempt_at = ?
		WHERE id = 1 AND last_attempt_at < ?`, now, cutoff)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, nil // we claimed it
	}

	// No row existed — try to insert (first-ever fetch)
	result, err = db.Exec(`INSERT OR IGNORE INTO usage_cache (id, data, fetched_at, last_attempt_at)
		VALUES (1, '{}', ?, ?)`, now, now)
	if err != nil {
		return false, err
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// GitCacheRow represents cached git/PR data for a repository.
type GitCacheRow struct {
	Data      json.RawMessage // full cachedGitData JSON blob
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
		repoHash, string(data), fetchedAt.Format(time.RFC3339Nano))
	return err
}

// LoadGitCache returns cached git data for a repo, or nil if not found.
func LoadGitCache(repoHash string) (*GitCacheRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var dataStr, fetchedStr string
	err = db.QueryRow(`SELECT data, fetched_at FROM git_cache WHERE repo_hash = ?`, repoHash).
		Scan(&dataStr, &fetchedStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &GitCacheRow{
		Data: json.RawMessage(dataStr),
	}
	row.FetchedAt, _ = time.Parse(time.RFC3339Nano, fetchedStr)
	return row, nil
}
