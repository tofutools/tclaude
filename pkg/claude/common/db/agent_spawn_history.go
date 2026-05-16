package db

import (
	"errors"
	"time"
)

// ErrSpawnRateLimited is the sentinel returned by ClaimSpawnSlot when
// the caller-agent has already used its full allowance of spawns
// within the rolling window. Callers translate this into a 429 at the
// HTTP layer.
var ErrSpawnRateLimited = errors.New("spawn rate limited")

// ClaimSpawnSlot atomically reserves a spawn slot for spawnerConvID at
// the given instant. Returns ErrSpawnRateLimited when the caller has
// already recorded maxPerWindow spawns inside the trailing window; nil
// on success (the caller may proceed with the spawn).
//
// maxPerWindow <= 0 means "unlimited" — the slot is granted without
// touching the table at all, so a daemon with the spawn rate limit
// disabled pays nothing here.
//
// Implementation mirrors db.ClaimCloneSlot: a single
// INSERT ... SELECT ... WHERE (count subquery) < max statement does
// the check + insert atomically under SQLite's WAL write lock, so two
// concurrent claims can't both pass when only one slot remains. The
// difference from ClaimCloneSlot is per-spawner counting (N rows per
// window) rather than per-source deduplication (1 row per cooldown):
// every `tclaude agent spawn` creates a brand-new conv, so there is no
// "same source" to gate against — what we cap is the caller's rate.
//
// Recording rule (same as ClaimCloneSlot): the attempt is recorded
// up-front, as soon as the check passes — not on successful spawn
// completion. A spawn that later fails (conv-id never materialises,
// group-add error, …) still consumes a slot, so a tight retry loop
// can't drain the limit by burning failed attempts.
//
// now is supplied by the caller so tests can advance a controlled
// clock; production passes time.Now().UTC().
func ClaimSpawnSlot(spawnerConvID string, maxPerWindow int, window time.Duration, now time.Time) error {
	if spawnerConvID == "" {
		return errors.New("ClaimSpawnSlot: spawnerConvID required")
	}
	if maxPerWindow <= 0 {
		return nil // rate limit disabled — unlimited spawns
	}
	if window < 0 {
		window = 0
	}
	d, err := Open()
	if err != nil {
		return err
	}
	// Normalise to UTC so the stored timestamps and the WHERE-clause
	// threshold are in one zone — RFC3339Nano strings only compare
	// correctly when the offset is identical, and a caller may hand us
	// a local-zoned time.
	now = now.UTC()
	threshold := now.Add(-window).Format(time.RFC3339Nano)
	nowStr := now.Format(time.RFC3339Nano)

	// INSERT only if the caller's spawn count inside the window is
	// still below the cap. SQLite executes INSERT ... SELECT ... WHERE
	// as a single statement under the database write lock (WAL mode),
	// so the read + write are atomic with respect to other writers.
	res, err := d.Exec(`
		INSERT INTO agent_spawn_history (spawner_conv_id, spawned_at)
		SELECT ?, ?
		WHERE (
			SELECT COUNT(*) FROM agent_spawn_history
			WHERE spawner_conv_id = ? AND spawned_at > ?
		) < ?`,
		spawnerConvID, nowStr, spawnerConvID, threshold, maxPerWindow)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSpawnRateLimited
	}
	return nil
}

// CountSpawnsSince returns how many spawns spawnerConvID has recorded
// since the given instant. Used by tests to assert that a rate-limited
// attempt did NOT consume a slot (the INSERT-WHERE leaves the table
// untouched on a limit hit); production callers go through
// ClaimSpawnSlot.
func CountSpawnsSince(spawnerConvID string, since time.Time) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	// UTC-normalise the threshold for the same reason ClaimSpawnSlot
	// does — spawned_at is stored UTC, so the comparison must be too.
	var n int
	err = d.QueryRow(`
		SELECT COUNT(*) FROM agent_spawn_history
		WHERE spawner_conv_id = ? AND spawned_at > ?`,
		spawnerConvID, since.UTC().Format(time.RFC3339Nano)).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}
