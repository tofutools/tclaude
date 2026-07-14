package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestCanonicalAgeTimestamp_PreservesPrecision pins the wire representation,
// which is deliberately ordinary UTC RFC3339Nano. Age consumers compare parsed
// instants rather than relying on the strings to have a sortable fixed width.
func TestCanonicalAgeTimestamp_PreservesPrecision(t *testing.T) {
	whole := CanonicalAgeTimestamp("2026-06-18T12:00:00Z")
	frac := CanonicalAgeTimestamp("2026-06-18T12:00:00.5Z")

	assert.Equal(t, "2026-06-18T12:00:00Z", whole)
	assert.Equal(t, "2026-06-18T12:00:00.5Z", frac,
		"full sub-second precision is preserved, never truncated to seconds")
}

// TestCanonicalAgeTimestamp_ZoneAndEdgeCases pins the zone canonicalisation
// (agents.created_at is written in the daemon's LOCAL zone) plus the empty and
// unparseable inputs.
func TestCanonicalAgeTimestamp_ZoneAndEdgeCases(t *testing.T) {
	// A non-UTC offset is normalised to UTC so values from different sources sort
	// in one zone.
	assert.Equal(t, "2026-06-18T10:00:00Z",
		CanonicalAgeTimestamp("2026-06-18T12:00:00+02:00"))

	assert.Equal(t, "", CanonicalAgeTimestamp(""), "empty stays empty")
	assert.Equal(t, "not-a-time", CanonicalAgeTimestamp("not-a-time"),
		"unparseable is returned unchanged, not blanked")
}

// TestCanonicalAgeTimestampFromTime pins that the time.Time formatter (the CLI
// actor path) produces exactly what CanonicalAgeTimestamp produces for the
// same instant, so the dashboard and CLI Age values are byte-identical.
func TestCanonicalAgeTimestampFromTime(t *testing.T) {
	assert.Equal(t, "", CanonicalAgeTimestampFromTime(time.Time{}), "zero time yields empty Age")

	instant := time.Date(2026, 6, 18, 12, 0, 0, 500_000_000, time.UTC)
	assert.Equal(t,
		CanonicalAgeTimestamp(instant.Format(time.RFC3339Nano)),
		CanonicalAgeTimestampFromTime(instant),
		"string and time.Time canonicalisers agree byte-for-byte")
}

func TestEarliestAgeTimestamp(t *testing.T) {
	assert.Equal(t, "2020-01-02T10:00:00.25Z", EarliestAgeTimestamp(
		"2026-07-14T12:00:00Z", // actor row stamped by a later backfill
		"2020-01-02T12:00:00.25+02:00",
	), "older conversation creation repairs a late actor enrollment time")

	assert.Equal(t, "2020-01-02T10:00:00Z", EarliestAgeTimestamp(
		"2020-01-02T10:00:00Z", // stable actor birth
		"2026-07-14T12:00:00Z", // later reincarnated conversation
	), "actor birth remains the Age across later conversation generations")

	assert.Equal(t, "2020-01-02T10:00:00Z",
		EarliestAgeTimestamp("bad-actor-time", "2020-01-02T10:00:00Z"),
		"a valid source wins over an unparseable one")
	assert.Equal(t, "bad-actor-time", EarliestAgeTimestamp("bad-actor-time", ""),
		"the first non-empty invalid value is preserved for diagnostics")
	assert.Empty(t, EarliestAgeTimestamp("", ""))
}
