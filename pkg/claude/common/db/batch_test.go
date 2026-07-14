package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestCanonicalAgeTimestamp_FixedWidthLexicalOrder is the regression guard for
// the Age-column sort hazard: the member Age is sorted LEXICALLY (server
// sortMembersByAge, client sort.js), so the timestamp format must make lexical
// order equal chronological order. time.RFC3339Nano omits trailing-zero
// fractions, so a whole-second instant sorts as if OLDER than a same-second
// sub-second one ('.' < 'Z') — the inversion this canonical layout removes by
// padding every value to a constant width while preserving full precision.
func TestCanonicalAgeTimestamp_FixedWidthLexicalOrder(t *testing.T) {
	whole := CanonicalAgeTimestamp("2026-06-18T12:00:00Z")
	frac := CanonicalAgeTimestamp("2026-06-18T12:00:00.5Z")

	// Constant width regardless of sub-second precision.
	assert.Equal(t, len(whole), len(frac), "canonical Age values are fixed-width")

	// Same wall-clock second: the fractional instant is chronologically newer and
	// MUST sort greater lexically — the property the newest-first Age sort relies
	// on. With bare RFC3339Nano ("…00Z" vs "…00.5Z") this assertion would fail.
	assert.Greater(t, frac, whole, "sub-second sorts after whole-second in the same second")

	// Full sub-second precision is preserved, never truncated to seconds.
	assert.Equal(t, "2026-06-18T12:00:00.500000000Z", frac)
}

// TestCanonicalAgeTimestamp_ZoneAndEdgeCases pins the zone canonicalisation
// (agents.created_at is written in the daemon's LOCAL zone) plus the empty and
// unparseable inputs.
func TestCanonicalAgeTimestamp_ZoneAndEdgeCases(t *testing.T) {
	// A non-UTC offset is normalised to UTC so values from different sources sort
	// in one zone.
	assert.Equal(t, "2026-06-18T10:00:00.000000000Z",
		CanonicalAgeTimestamp("2026-06-18T12:00:00+02:00"))

	assert.Equal(t, "", CanonicalAgeTimestamp(""), "empty stays empty")
	assert.Equal(t, "not-a-time", CanonicalAgeTimestamp("not-a-time"),
		"unparseable is returned unchanged, not blanked")
}

// TestCanonicalAgeTimestampFromTime pins that the time.Time formatter (the CLI
// actor path) produces exactly what CanonicalAgeTimestamp produces for the same
// instant, so the dashboard and CLI Age values are byte-identical.
func TestCanonicalAgeTimestampFromTime(t *testing.T) {
	assert.Equal(t, "", CanonicalAgeTimestampFromTime(time.Time{}), "zero time yields empty Age")

	instant := time.Date(2026, 6, 18, 12, 0, 0, 500_000_000, time.UTC)
	assert.Equal(t,
		CanonicalAgeTimestamp(instant.Format(time.RFC3339Nano)),
		CanonicalAgeTimestampFromTime(instant),
		"string and time.Time canonicalisers agree byte-for-byte")
}
