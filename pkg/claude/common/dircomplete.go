package common

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExpandHomePrefix expands a leading "~" or "~/" in p to the user's home
// directory, for filesystem lookups during directory tab-completion. The
// returned path is only used to stat/list the filesystem; the text the user
// sees and edits keeps whatever they typed (e.g. "~/Doc" stays "~/Doc").
func ExpandHomePrefix(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// CompleteDirPath performs bash-like Tab completion of a directory path: it
// matches the final path segment against sibling directory names and
// extends input to their longest common prefix. When exactly one directory
// matches, the result is completed all the way through a trailing "/" (so
// pressing Tab repeatedly walks down the tree); when several match, input is
// extended as far as unambiguous and the candidate names are returned for
// display, mirroring bash's "partial-complete, then list" behavior.
//
// It is shared by the TUIs that ask an operator for a directory — the
// `session watch` new-session prompt and the agentd console's spawn form —
// so both complete paths the same way.
func CompleteDirPath(input string) (completed string, candidates []string) {
	if input == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home + "/", nil
		}
		return input, nil
	}

	dir, prefix := filepath.Split(ExpandHomePrefix(input))
	if dir == "" {
		dir = "."
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return input, nil
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return input, nil
	}
	sort.Strings(names)

	common := names[0]
	for _, n := range names[1:] {
		common = commonStringPrefix(common, n)
	}

	// input's final path segment is always `prefix` verbatim (home
	// expansion only ever rewrites the directory portion before it), so
	// trimming that many characters off the end of input recovers the
	// unexpanded lead-in (e.g. "~/" stays "~/" rather than becoming the
	// resolved home directory).
	head := input[:len(input)-len(prefix)]
	completed = head + common
	if len(names) == 1 {
		return completed + "/", nil
	}
	if completed != input {
		return completed, names
	}
	return input, names
}

// commonStringPrefix returns the longest common prefix of a and b.
func commonStringPrefix(a, b string) string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
