package pathv1

import (
	"fmt"
	"time"
)

func CanonicalTimestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func ParseCanonicalTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid path-v1 timestamp: %w", err)
	}
	if canonical := CanonicalTimestamp(parsed); value != canonical {
		return time.Time{}, fmt.Errorf("noncanonical path-v1 timestamp %q, want %q", value, canonical)
	}
	return parsed, nil
}
