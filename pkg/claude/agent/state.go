package agent

import (
	"fmt"
	"strings"

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
