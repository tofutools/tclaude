package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BgShellSet is the per-session ledger of Claude Code BACKGROUND SHELL
// commands (`Bash` with `run_in_background: true`) believed to be running
// right now, keyed by the harness's backgroundTaskId. It is persisted as
// JSON in sessions.bg_shells_json and is the source of truth behind the
// dashboard's "⚙+N" badge.
//
// It is the sibling of SubagentSet (subagents.go) and self-heals for the
// same reason — the hook stream is lossy — but the loss is far more
// structural here: Claude Code fires a PostToolUse hook when a background
// shell LAUNCHES and no hook at all when it EXITS. A ledger fed only by
// hooks could therefore only ever grow, and would show long-finished
// "ghost" shells exactly during the idle window when the badge is read.
//
// Three things keep it honest, in order of authority:
//
//   - PROCESS LIVENESS (the primary signal, and the only one that is
//     ground truth): the daemon re-matches each entry against the live
//     descendant processes of the agent's harness at dashboard read time
//     — see agentd's background-shell reconcile. Claude Code exposes no
//     PID anywhere (not in the hook, the tool result, or the transcript),
//     so the recorded Command is what the match is made on; that is why
//     an entry carries the command string and not just an id.
//   - EXPLICIT REMOVAL: a PostToolUse for `TaskStop` names the task_id it
//     killed, and process (re)starts and session exit are known-zero
//     boundaries — background shells are children of the harness process,
//     so none can outlive it.
//   - BgShellTTL: the backstop for a host where descendant enumeration is
//     unavailable or the command cannot be matched. It bounds how long a
//     ghost can be displayed; it is deliberately the only mechanism that
//     needs no cooperation from the OS or the harness.
type BgShellSet map[string]BgShellSeen

// BgShellSeen is one ledger entry: the shell command that was launched
// (what the liveness reconcile matches a live process against, and what
// the badge tooltip shows) and when the entry was last proved alive —
// stamped at launch, then refreshed by any later evidence.
type BgShellSeen struct {
	Command string    `json:"cmd,omitempty"`
	Seen    time.Time `json:"seen"`
}

// BgShellTTL is how long an entry survives with no evidence before
// LiveCount/Sweep treat it as a ghost. It is a BACKSTOP, not the primary
// reconcile (that is process liveness), so it is set generously: a
// background shell is very often a long-running dev server or test watch
// that legitimately runs for hours, and under-reporting a real one is the
// worse failure — the badge exists precisely to stop an agent waiting on
// such a command from looking idle. On a host where liveness reconcile
// works, an entry is refreshed on every dashboard poll and this never
// bites; where it does not, a ghost is bounded to this window.
const BgShellTTL = 2 * time.Hour

const bgShellAnonPrefix = "anon-"

// bgShellCommandMax bounds the command string kept per entry. The ledger
// is stored inline on the sessions row and re-serialised on every hook
// tick, so an agent launching a pathological one-liner must not be able
// to bloat that write path. The prefix is all the liveness match and the
// tooltip need.
const bgShellCommandMax = 512

// ParseBgShellSet decodes a sessions.bg_shells_json value. "" (the column
// default, and what an empty set encodes to) and malformed JSON both
// yield an empty set — the ledger is best-effort state, never a reason to
// fail a hook.
func ParseBgShellSet(s string) BgShellSet {
	if s == "" {
		return nil
	}
	var set BgShellSet
	if err := json.Unmarshal([]byte(s), &set); err != nil {
		return nil
	}
	return set
}

// Encode serialises the set for storage. An empty/nil set encodes to ""
// so the column stays at its DEFAULT for the common no-background-shells
// case, matching SubagentSet.Encode.
func (set BgShellSet) Encode() string {
	if len(set) == 0 {
		return ""
	}
	b, err := json.Marshal(set)
	if err != nil {
		return ""
	}
	return string(b)
}

// Sweep deletes entries not seen within BgShellTTL of now. Safe on a nil set.
func (set BgShellSet) Sweep(now time.Time) {
	for id, e := range set {
		if now.Sub(e.Seen) > BgShellTTL {
			delete(set, id)
		}
	}
}

// LiveCount reports how many entries are within BgShellTTL of now, without
// mutating the set. Read surfaces use this so a ghost stops being displayed
// as soon as it expires, even if no hook has fired since to Sweep it from
// storage.
func (set BgShellSet) LiveCount(now time.Time) int {
	n := 0
	for _, e := range set {
		if now.Sub(e.Seen) <= BgShellTTL {
			n++
		}
	}
	return n
}

// Live returns the entries within BgShellTTL of now, keyed by task id.
// The liveness reconcile needs the commands, not just the count.
func (set BgShellSet) Live(now time.Time) map[string]BgShellSeen {
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]BgShellSeen, len(set))
	for id, e := range set {
		if now.Sub(e.Seen) <= BgShellTTL {
			out[id] = e
		}
	}
	return out
}

// Add records a background shell starting. An empty id (a tool_response
// that carried no backgroundTaskId — a harness version change, or a shell
// backgrounded from the UI rather than by the tool) gets a synthetic anon
// key so the count still tracks; the liveness reconcile removes it on the
// same terms as a keyed entry, since it matches on the command. Returns
// the set (allocating if nil).
func (set BgShellSet) Add(id, command string, now time.Time) BgShellSet {
	if set == nil {
		set = BgShellSet{}
	}
	if id == "" {
		id = fmt.Sprintf("%s%d", bgShellAnonPrefix, now.UnixNano())
		for i := 0; ; i++ {
			if _, taken := set[id]; !taken {
				break
			}
			id = fmt.Sprintf("%s%d-%d", bgShellAnonPrefix, now.UnixNano(), i)
		}
	}
	set[id] = BgShellSeen{Command: truncateBgShellCommand(command), Seen: now}
	return set
}

// Refresh re-stamps an entry the liveness reconcile just proved alive, so
// a genuinely long-running background shell never ages out through
// BgShellTTL on a host where the reconcile works. Unknown ids are ignored:
// the reconcile never invents entries, it only confirms or retires them.
// Reports whether anything changed.
func (set BgShellSet) Refresh(id string, now time.Time) bool {
	e, known := set[id]
	if !known || !e.Seen.Before(now) {
		return false
	}
	e.Seen = now
	set[id] = e
	return true
}

// Remove records a background shell ending — a TaskStop naming its
// task_id, or the liveness reconcile retiring an entry whose process is
// gone. A known id is deleted; an unknown non-empty id is a no-op. An
// empty id falls back to dropping the oldest entry, anon entries first,
// so a TaskStop whose payload lacked a task_id still decrements rather
// than leaking.
func (set BgShellSet) Remove(id string) {
	if len(set) == 0 {
		return
	}
	if id != "" {
		delete(set, id)
		return
	}
	if anon := set.oldest(true); anon != "" {
		delete(set, anon)
		return
	}
	if victim := set.oldest(false); victim != "" {
		delete(set, victim)
	}
}

// oldest returns the key with the earliest Seen — restricted to synthetic
// anon entries when anonOnly is set — or "" when none match.
func (set BgShellSet) oldest(anonOnly bool) string {
	var key string
	var seen time.Time
	for id, e := range set {
		if anonOnly && !strings.HasPrefix(id, bgShellAnonPrefix) {
			continue
		}
		if key == "" || e.Seen.Before(seen) {
			key, seen = id, e.Seen
		}
	}
	return key
}

// truncateBgShellCommand bounds a recorded command to bgShellCommandMax
// runes, cutting on a rune boundary so the stored JSON stays valid UTF-8.
func truncateBgShellCommand(s string) string {
	if len(s) <= bgShellCommandMax {
		return s
	}
	r := []rune(s)
	if len(r) <= bgShellCommandMax {
		return s
	}
	return string(r[:bgShellCommandMax])
}
