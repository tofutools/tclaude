package db

import (
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDBTimeText_RepresentationErrorsCarrySentinel covers the text compatibility
// boundary's failure arm directly. Callers that treat the database as a
// rebuildable cache branch on IsTimestampRepresentationError to decide whether a
// failed write is a loud lossy-timestamp bug or a quiet degrade, so the sentinel
// wrap is load-bearing API rather than message decoration.
//
// The unparseable case in particular is only reachable through
// textUnixNanoValue.Value: parseLegacyDBTime returns a bare time.Parse error,
// and nothing below that boundary attaches the sentinel. Without this test,
// dropping the wrap silently reclassifies every malformed legacy timestamp as
// an ordinary persistence failure.
func TestDBTimeText_RepresentationErrorsCarrySentinel(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "unparseable text", value: "not a timestamp"},
		{name: "empty-string legacy sentinel", value: ""},
		{name: "above the Unix-nanosecond range", value: "3000-01-01T00:00:00Z"},
		{name: "below the Unix-nanosecond range", value: "1600-01-01T00:00:00Z"},
		{name: "reserved zero", value: "1970-01-01T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dbTimeText(tc.value).Value()
			require.Error(t, err)
			assert.True(t, errors.Is(err, errTimestampRepresentation),
				"a text timestamp that cannot be represented must be distinguishable from an unrelated write failure")
			assert.True(t, IsTimestampRepresentationError(err),
				"the exported predicate is the form external packages branch on")
		})
	}
}

// TestDBTimeText_ValidTextBindsAsUnixNanoseconds is the non-inverting control:
// the sentinel must not be attached to values that convert cleanly, or the
// predicate would classify every write as a representation failure and the
// quiet degrade path above would go loud for everything.
func TestDBTimeText_ValidTextBindsAsUnixNanoseconds(t *testing.T) {
	instant := time.Date(2026, 8, 1, 12, 34, 56, 789_000_000, time.UTC)

	for _, text := range []string{
		instant.Format(time.RFC3339Nano),
		"2026-08-01T14:34:56.789+02:00", // same instant, non-UTC caller offset
	} {
		value, err := dbTimeText(text).Value()
		require.NoError(t, err)
		assert.Equal(t, instant.UnixNano(), value, "text %q", text)
		assert.False(t, IsTimestampRepresentationError(err))
	}
}

// TestNullableDBTimeText_EmptyIsAbsenceAndInvalidStillFails pins the split
// between the two ways a text timestamp can be non-representable: the empty
// string is the legacy absence marker and maps to SQL NULL, while any other
// unconvertible value must still surface as a representation error rather than
// being quietly nulled out.
func TestNullableDBTimeText_EmptyIsAbsenceAndInvalidStillFails(t *testing.T) {
	assert.Nil(t, nullableDBTimeText(""), "the legacy empty timestamp is absence, not a failure")

	valuer, ok := nullableDBTimeText("not a timestamp").(driver.Valuer)
	require.True(t, ok, "a non-empty text timestamp still binds through the guarded valuer")
	_, err := valuer.Value()
	require.Error(t, err)
	assert.True(t, IsTimestampRepresentationError(err),
		"only the empty string is absence; other unconvertible text must stay an error")

	valuer, ok = nullableDBTimeText("2026-08-01T12:34:56Z").(driver.Valuer)
	require.True(t, ok)
	value, err := valuer.Value()
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 8, 1, 12, 34, 56, 0, time.UTC).UnixNano(), value)
}
