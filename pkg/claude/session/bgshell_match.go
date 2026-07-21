package session

import (
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// bgShellNeedleMin is the shortest command fragment that may be used to
// claim a live process. A one- or two-character fragment would match
// almost any argv, so an entry that cannot offer more than this is
// reported UNDECIDED rather than matched against noise — the ledger's TTL
// then owns it. Long enough to be specific, short enough that ordinary
// commands ("go test", "make", "pytest") clear it.
const bgShellNeedleMin = 6

// BgShellLiveness is the verdict of one reconcile pass over a session's
// background-shell ledger.
type BgShellLiveness struct {
	// Dead lists ledger ids whose command matched no live process. The
	// caller removes these — this is the signal that replaces the exit
	// hook Claude Code never fires.
	Dead []string
	// Alive lists ledger ids positively matched to a live process. The
	// caller re-stamps these so a genuinely long-running background shell
	// never ages out through the TTL.
	Alive []string
	// Undecided lists ids whose command was too short or too heavily
	// quoted to match on. They are neither confirmed nor retired; the TTL
	// is what bounds them.
	Undecided []string
}

// ReconcileBgShells decides, for each entry of a background-shell ledger,
// whether the command it recorded is still running among cmdlines — the
// full argv of every process below the agent's harness, as returned by
// DescendantCommandLines.
//
// Matching is by command-fragment containment, because Claude Code exposes
// no PID for a background task: the wrapper shell it launches carries the
// command inside its own argv (verified empirically, see proctree.go), so
// "some descendant's argv contains this command" is the available proxy
// for "this task is still running".
//
// Each live process may be claimed by at most one ledger entry. That is
// what makes N concurrent copies of the SAME command resolve correctly: if
// an agent launched `npm run dev` three times and two are still up, two
// entries claim the two survivors and the third is retired, instead of all
// three matching the same process (which would retire nothing) or none
// matching (which would retire all). Which specific id gets retired among
// interchangeable duplicates is arbitrary — they are indistinguishable by
// construction — so entries are walked in a stable sorted order to keep
// the outcome deterministic rather than map-iteration random.
func ReconcileBgShells(ledger map[string]db.BgShellSeen, cmdlines []string) BgShellLiveness {
	var out BgShellLiveness
	if len(ledger) == 0 {
		return out
	}
	ids := make([]string, 0, len(ledger))
	for id := range ledger {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	claimed := make([]bool, len(cmdlines))
	for _, id := range ids {
		needle := bgShellNeedle(ledger[id].Command)
		if needle == "" {
			out.Undecided = append(out.Undecided, id)
			continue
		}
		matched := false
		for i, cmd := range cmdlines {
			if claimed[i] || !strings.Contains(cmd, needle) {
				continue
			}
			claimed[i] = true
			matched = true
			break
		}
		if matched {
			out.Alive = append(out.Alive, id)
		} else {
			out.Dead = append(out.Dead, id)
		}
	}
	return out
}

// bgShellNeedle reduces a recorded shell command to the longest fragment
// that can be looked for verbatim inside a process's argv, or "" when no
// usable fragment exists.
//
// Two things stop the whole command from being usable as-is:
//
//   - SINGLE QUOTES. The harness wraps the command in a single-quoted
//     `eval '<command>'`, so any single quote inside it is rewritten as
//     '"'"' and the original text no longer appears verbatim.
//   - NEWLINES. A multi-line command survives intact in a Linux
//     /proc/<pid>/cmdline, but macOS's `ps` output is line-oriented and a
//     multi-line argv cannot be read back whole. Splitting on newlines
//     keeps one matcher correct on both platforms.
//
// Splitting on both and taking the longest surviving run yields the most
// specific fragment that is safe everywhere. A fragment shorter than
// bgShellNeedleMin is rejected as too generic to distinguish processes.
func bgShellNeedle(command string) string {
	best := ""
	for _, seg := range strings.FieldsFunc(command, func(r rune) bool {
		return r == '\'' || r == '\n' || r == '\r'
	}) {
		if seg = strings.TrimSpace(seg); len(seg) > len(best) {
			best = seg
		}
	}
	if len(best) < bgShellNeedleMin {
		return ""
	}
	return best
}
