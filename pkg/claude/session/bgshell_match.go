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
	return ReconcileBackground(ledger, nil, cmdlines).Shells
}

// BackgroundLiveness is the result of ONE reconcile pass over both
// process-backed ledgers a session can hold.
type BackgroundLiveness struct {
	Shells   BgShellLiveness
	Monitors BgShellLiveness
}

// ReconcileBackground decides, for every entry of a session's
// background-shell and monitor ledgers at once, whether the command it
// recorded is still running among cmdlines.
//
// The two ledgers MUST be reconciled together rather than in two passes,
// because each live process may be claimed by at most one entry and the
// two kinds are indistinguishable in the process table: a monitor's watch
// script is launched by the harness exactly the way a background shell is
// (the harness reports a command monitor's task type as `local_bash`). Two
// independent passes would let a background shell and a monitor with
// similar commands both claim the same process, so neither would ever be
// retired.
//
// Entries are walked shells-first, each group sorted by id, purely so the
// outcome is deterministic when interchangeable duplicates compete for the
// same process — which one of N identical commands gets retired is
// arbitrary by construction.
//
// WEBSOCKET monitors are never offered a process to match. A `ws` watch
// runs inside the harness process and has no descendant of its own, so
// asking this reconcile about it would retire it instantly and always;
// it is reported Undecided and left to its deadline and the TTL.
func ReconcileBackground(
	shells map[string]db.BgShellSeen,
	monitors map[string]db.MonitorSeen,
	cmdlines []string,
) BackgroundLiveness {
	var out BackgroundLiveness
	if len(shells) == 0 && len(monitors) == 0 {
		return out
	}

	claimed := make([]bool, len(cmdlines))
	// claim finds an unclaimed process whose argv contains needle, marking
	// it taken. An empty needle means "too generic to look for".
	claim := func(needle string) (matched, decided bool) {
		if needle == "" {
			return false, false
		}
		for i, cmd := range cmdlines {
			if claimed[i] || !strings.Contains(cmd, needle) {
				continue
			}
			claimed[i] = true
			return true, true
		}
		return false, true
	}

	for _, id := range sortedKeys(shells) {
		matched, decided := claim(bgShellNeedle(shells[id].Command))
		out.Shells.record(id, matched, decided)
	}
	for _, id := range sortedKeys(monitors) {
		e := monitors[id]
		if e.WS {
			out.Monitors.Undecided = append(out.Monitors.Undecided, id)
			continue
		}
		matched, decided := claim(bgShellNeedle(e.Command))
		out.Monitors.record(id, matched, decided)
	}
	return out
}

// record files one entry's verdict into the right bucket.
func (v *BgShellLiveness) record(id string, matched, decided bool) {
	switch {
	case !decided:
		v.Undecided = append(v.Undecided, id)
	case matched:
		v.Alive = append(v.Alive, id)
	default:
		v.Dead = append(v.Dead, id)
	}
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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
