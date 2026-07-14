package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var strictRFC3339Pattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`,
)

// ParseRFC3339 trims surrounding whitespace, enforces the exact RFC3339
// lexical shape used by process templates, and then validates the calendar
// value with time.Parse. The lexical gate closes permissive time.Parse cases
// such as one-digit hours, comma fractions, and out-of-range zone offsets.
func ParseRFC3339(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if !strictRFC3339Pattern.MatchString(trimmed) {
		return time.Time{}, fmt.Errorf("timestamp %q must use strict RFC3339 syntax", value)
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}
