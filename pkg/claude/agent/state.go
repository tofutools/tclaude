package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// parseStateFilter validates the value of `--state` flags on `agent ls`
// and `groups ls`. Empty means "no filter".
//
//   - applies = true when filtering is on
//   - wantOnline = true when "online" was requested, false when "offline"
//   - err on any other value (single error message both call sites use)
func parseStateFilter(s string) (wantOnline, applies bool, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return false, false, nil
	case "online":
		return true, true, nil
	case "offline":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("invalid --state %q (want online | offline)", s)
	}
}

// completeStateFilterValues is the boa-style alternatives function for
// `--state` flags. Two values; we still hand them through completion so
// shells with descriptions get the hint.
func completeStateFilterValues(_ *cobra.Command, _ []string, toComplete string) []string {
	out := []string{}
	for _, v := range []struct{ slug, desc string }{
		{"online", "members with a live tmux session"},
		{"offline", "members with no live tmux session"},
	} {
		if strings.HasPrefix(v.slug, toComplete) {
			out = append(out, v.slug+"\t"+v.desc)
		}
	}
	return out
}

// parseDurationDays accepts everything time.ParseDuration accepts plus
// "<n>d" and "<n>w" for days and weeks. Mirrors session/prune.go's
// parseDuration; duplicated here rather than re-exporting because the
// session package depends on a different internal context.
func parseDurationDays(s string) (time.Duration, error) {
	if len(s) > 1 {
		suffix := s[len(s)-1]
		prefix := s[:len(s)-1]
		switch suffix {
		case 'w', 'W':
			var weeks int
			if _, err := fmt.Sscanf(prefix, "%d", &weeks); err == nil && weeks >= 0 {
				return time.Duration(weeks) * 7 * 24 * time.Hour, nil
			}
		case 'd', 'D':
			var days int
			if _, err := fmt.Sscanf(prefix, "%d", &days); err == nil && days >= 0 {
				return time.Duration(days) * 24 * time.Hour, nil
			}
		}
	}
	return time.ParseDuration(s)
}
