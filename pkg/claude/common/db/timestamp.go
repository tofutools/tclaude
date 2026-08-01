package db

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"time"
)

// dbTimestamp is the only production scan target for an INTEGER timestamp.
// In particular it intentionally rejects string/[]byte inputs: database/sql
// can silently stringify an INTEGER for a *string destination, which would let
// a missed reader degrade to a zero time after an ignored parse error.
type dbTimestamp struct {
	time  time.Time
	valid bool
}

// migrationBridgeTimestamp is limited to code that is called both by an old
// migration and by current production paths. It accepts the historical text
// representation while that migration runs, but current INTEGER rows still
// cross the boundary as int64 rather than through database/sql string coercion.
type migrationBridgeTimestamp struct {
	time time.Time
	text string
}

func (t *migrationBridgeTimestamp) Scan(src any) error {
	switch value := src.(type) {
	case int64:
		t.time = timeFromUnixNano(value)
		t.text = t.time.Format(time.RFC3339Nano)
		return nil
	case string:
		parsed, err := parseLegacyDBTime(value)
		if err != nil {
			return err
		}
		t.time = parsed
		t.text = value
		return nil
	case []byte:
		return t.Scan(string(value))
	default:
		return fmt.Errorf("migration bridge timestamp must be INTEGER or legacy TEXT, got %T", src)
	}
}

func (t migrationBridgeTimestamp) Text() string    { return t.text }
func (t migrationBridgeTimestamp) Time() time.Time { return t.time }

func (t *dbTimestamp) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		t.time = time.Time{}
		t.valid = false
		return nil
	case int64:
		t.time = timeFromUnixNano(value)
		t.valid = true
		return nil
	default:
		return fmt.Errorf("database timestamp must be INTEGER or NULL, got %T", src)
	}
}

func (t dbTimestamp) Time() time.Time {
	if !t.valid {
		return time.Time{}
	}
	return t.time
}

func (t dbTimestamp) NullTime() sql.NullTime {
	return sql.NullTime{Time: t.time, Valid: t.valid}
}

// RFC3339Nano is the outbound JSON/API/export boundary for a database
// timestamp. The database representation remains an INTEGER; absent values
// become the conventional empty optional string.
func (t dbTimestamp) RFC3339Nano() string {
	if !t.valid {
		return ""
	}
	return t.time.Format(time.RFC3339Nano)
}

// unixNanoValue defers timestamp validation until database/sql binds the
// argument. That keeps every production write on one range-checked path while
// still letting callers pass timestamps directly to Exec and Query.
type unixNanoValue struct {
	time       time.Time
	allowEpoch bool
}

type textUnixNanoValue struct {
	value string
}

func (v textUnixNanoValue) Value() (driver.Value, error) {
	parsed, err := parseLegacyDBTime(v.value)
	if err != nil {
		return nil, fmt.Errorf("database timestamp %q: %w", v.value, err)
	}
	return timeToUnixNano(parsed)
}

func (v unixNanoValue) Value() (driver.Value, error) {
	ns, err := guardedUnixNano(v.time, v.allowEpoch)
	if err != nil {
		return nil, err
	}
	return ns, nil
}

// dbTime returns a database/sql value containing Unix nanoseconds. UnixNano
// wraps silently outside its representable 1678–2262 window, so callers must
// never bind a bare UnixNano result for a persisted timestamp.
func dbTime(value time.Time) driver.Valuer {
	return unixNanoValue{time: value}
}

// dbTimeBoundary is for read-only range predicates whose inclusive boundary
// may legitimately be the Unix epoch. Persisted timestamps still use dbTime,
// where integer zero remains reserved for absence detection.
func dbTimeBoundary(value time.Time) driver.Valuer {
	return unixNanoValue{time: value, allowEpoch: true}
}

// nullableDBTime maps Go's zero time to SQL NULL and otherwise uses the same
// guarded Unix-nanosecond conversion as dbTime. NULL is the sole representation
// for an absent timestamp in the v181 schema; legacy empty strings migrate to
// NULL deterministically.
func nullableDBTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return dbTime(value)
}

// dbTimeText converts a timestamp crossing a compatibility boundary where the
// public/legacy shape must remain RFC3339 text (for example conv-index rows and
// group export JSON) before binding it to an INTEGER timestamp column.
func dbTimeText(value string) driver.Valuer {
	return textUnixNanoValue{value: value}
}

func nullableDBTimeText(value string) any {
	if value == "" {
		return nil
	}
	return dbTimeText(value)
}

func timeToUnixNano(value time.Time) (int64, error) {
	return guardedUnixNano(value, false)
}

func guardedUnixNano(value time.Time, allowEpoch bool) (int64, error) {
	if value.IsZero() {
		return 0, fmt.Errorf("database timestamp is zero; use nullableDBTime for an absent timestamp")
	}
	ns := value.UnixNano()
	if ns == 0 && !allowEpoch {
		return 0, fmt.Errorf("database timestamp %s maps to reserved zero", value.Format(time.RFC3339Nano))
	}
	if !time.Unix(0, ns).Equal(value) {
		return 0, fmt.Errorf("database timestamp %s is outside the Unix-nanosecond range", value.Format(time.RFC3339Nano))
	}
	return ns, nil
}

func timeFromUnixNano(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}

// parseLegacyDBTime is migration-only. Current rows must be INTEGER, but the
// v181 converter also sees the historical RFC3339Nano text and numeric text
// written through INTEGER values into a pre-v181 TEXT-affinity column during a
// fresh v1→head walk.
func parseLegacyDBTime(value string) (time.Time, error) {
	if ns, err := strconv.ParseInt(value, 10, 64); err == nil {
		parsed := timeFromUnixNano(ns)
		if _, err := timeToUnixNano(parsed); err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if _, err := timeToUnixNano(parsed); err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}
