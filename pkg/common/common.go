package common

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
)

func DefaultParamEnricher() boa.ParamEnricher {
	return boa.ParamEnricherCombine(
		boa.ParamEnricherBool,
		boa.ParamEnricherName,
		boa.ParamEnricherShort,
	)
}

// Size unit multipliers
const (
	KB int64 = 1024
	MB int64 = 1024 * KB
	GB int64 = 1024 * MB
	TB int64 = 1024 * GB
)

var sizePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([kmgt]?b?)?$`)

// ParseSize parses a human-readable size string into bytes.
// Supports formats like: "100", "10k", "10kb", "10K", "10KB", "1.5m", "1mb", "1g", "1gb", "1t", "1tb"
// Case-insensitive. The "b" suffix is optional.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	matches := sizePattern.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("invalid size format: %q", s)
	}

	numStr := matches[1]
	unit := strings.ToLower(matches[2])

	// Parse the numeric part
	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %q", numStr)
	}

	// Determine multiplier based on unit
	var multiplier int64 = 1
	switch {
	case unit == "" || unit == "b":
		multiplier = 1
	case strings.HasPrefix(unit, "k"):
		multiplier = KB
	case strings.HasPrefix(unit, "m"):
		multiplier = MB
	case strings.HasPrefix(unit, "g"):
		multiplier = GB
	case strings.HasPrefix(unit, "t"):
		multiplier = TB
	default:
		return 0, fmt.Errorf("unknown unit: %q", unit)
	}

	result := int64(num * float64(multiplier))
	if result < 0 {
		return 0, fmt.Errorf("size cannot be negative")
	}

	return result, nil
}
