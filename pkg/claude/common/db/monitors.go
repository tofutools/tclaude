package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MonitorSet is the per-session ledger of Claude Code MONITORS (the
// `Monitor` tool: a long-running watch whose stdout lines are streamed
// back into the conversation as events) believed to be running right now,
// keyed by the harness's taskId. It is persisted as JSON in
// sessions.monitors_json and is the source of truth behind the dashboard's
// "👁+N" badge.
//
// It is the third sibling of SubagentSet (subagents.go) and BgShellSet
// (bgshells.go), and it exists for the same reason they do: an agent that
// is watching a CI job, tailing a log, or polling a PR has work
// outstanding past the end of its turn, and without a ledger it renders as
// plain `idle`.
//
// A monitor is a background task in the SAME id namespace as a background
// shell — `TaskStop` accepts either, and a command monitor is reported by
// the harness as task_type `local_bash`. What makes it a separate ledger
// rather than more BgShellSet entries is that its two kinds retire on
// different evidence:
//
//   - A COMMAND monitor runs a real shell process below the harness, so
//     the process-liveness reconcile that keeps BgShellSet honest applies
//     to it unchanged, and is likewise its primary signal.
//   - A WEBSOCKET monitor (`ws` input instead of `command`) runs inside
//     the harness process. There is no descendant process to match, so
//     offering it to that reconcile would retire it instantly and always.
//     Entries carry WS so the reconcile can decline to have an opinion.
//
// Monitors also carry evidence background shells do not: a non-persistent
// watch has a harness-enforced deadline (`timeout_ms`), recorded here as
// Deadline. That is a genuine upper bound rather than a heuristic — the
// harness kills the watch at it — and it is the only retirement signal a
// websocket monitor has besides an explicit TaskStop.
//
// The remaining lossiness is identical to BgShellSet's: Claude Code fires
// a PostToolUse hook when a monitor LAUNCHES and no hook at all when it
// ends (the completion arrives in-transcript as a task notification, which
// no hook observes). MonitorTTL is the backstop of last resort.
type MonitorSet map[string]MonitorSeen

// MonitorSeen is one ledger entry.
type MonitorSeen struct {
	// Command is the watch script, for a command monitor. It is what the
	// liveness reconcile matches a live process against — the harness
	// exposes no PID for a background task — and is empty for a WS entry.
	Command string `json:"cmd,omitempty"`
	// Label is the monitor's own `description` input, or the socket URL
	// for a WS watch. It exists for the badge tooltip: a monitor's command
	// is often an unreadable poll loop, whereas its description is written
	// to be read ("errors in deploy.log").
	Label string `json:"label,omitempty"`
	// WS marks a websocket watch — one with no descendant process, which
	// the liveness reconcile must decline rather than retire. See
	// MonitorSet's doc comment.
	WS bool `json:"ws,omitempty"`
	// Seen is when the entry was last proved alive: stamped at launch,
	// then refreshed by the liveness reconcile.
	Seen time.Time `json:"seen"`
	// Deadline is when the harness will end this watch on its own, derived
	// from the launch payload's timeout_ms. Zero for a `persistent: true`
	// monitor, which runs until the session ends or a TaskStop kills it.
	//
	// Unlike Seen this is never refreshed: it is an absolute bound taken
	// once at launch, and the whole point of it is that it does not move.
	Deadline time.Time `json:"deadline,omitzero"`
}

// MonitorTTL is how long an entry survives with no evidence before it is
// treated as a ghost. It matches BgShellTTL and is a BACKSTOP for the same
// reasons: for a command monitor the process reconcile is the real signal,
// and for any non-persistent monitor Deadline is a tighter bound already.
// It only truly binds a PERSISTENT WEBSOCKET monitor on which nothing else
// has an opinion — and there, under-reporting real work is the worse
// failure, so it is set generously.
const MonitorTTL = 2 * time.Hour

const monitorAnonPrefix = "anon-"

// monitorCommandMax bounds the command and label kept per entry, for the
// same write-path reason as bgShellCommandMax: the ledger is stored inline
// on the sessions row and re-serialised on every hook tick.
const monitorCommandMax = 512

// ParseMonitorSet decodes a sessions.monitors_json value. "" (the column
// default, and what an empty set encodes to) and malformed JSON both yield
// an empty set — the ledger is best-effort state, never a reason to fail a
// hook.
func ParseMonitorSet(s string) MonitorSet {
	if s == "" {
		return nil
	}
	var set MonitorSet
	if err := json.Unmarshal([]byte(s), &set); err != nil {
		return nil
	}
	return set
}

// Encode serialises the set for storage. An empty/nil set encodes to ""
// so the column stays at its DEFAULT for the common no-monitors case.
func (set MonitorSet) Encode() string {
	if len(set) == 0 {
		return ""
	}
	b, err := json.Marshal(set)
	if err != nil {
		return ""
	}
	return string(b)
}

// live reports whether one entry is still believed to be running at now:
// within MonitorTTL of its last sighting AND not past its harness-enforced
// deadline. A zero Deadline (a persistent monitor) imposes no bound.
func (e MonitorSeen) live(now time.Time) bool {
	if now.Sub(e.Seen) > MonitorTTL {
		return false
	}
	return e.Deadline.IsZero() || now.Before(e.Deadline)
}

// Sweep deletes entries no longer live at now. Safe on a nil set.
func (set MonitorSet) Sweep(now time.Time) {
	for id, e := range set {
		if !e.live(now) {
			delete(set, id)
		}
	}
}

// LiveCount reports how many entries are live at now, without mutating the
// set. Read surfaces use this so a ghost stops being displayed as soon as
// it expires, even if no hook has fired since to Sweep it from storage.
func (set MonitorSet) LiveCount(now time.Time) int {
	n := 0
	for _, e := range set {
		if e.live(now) {
			n++
		}
	}
	return n
}

// Live returns the entries still believed to be running at now, keyed by
// task id. The liveness reconcile needs the commands, not just the count.
func (set MonitorSet) Live(now time.Time) map[string]MonitorSeen {
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]MonitorSeen, len(set))
	for id, e := range set {
		if e.live(now) {
			out[id] = e
		}
	}
	return out
}

// Add records a monitor starting. deadline is the zero time for a
// persistent watch. An empty id (a tool_response that carried no taskId —
// a harness version change) gets a synthetic anon key so the count still
// tracks; a command entry is then retired by the liveness reconcile on the
// same terms as a keyed one, since that matches on the command. Returns
// the set (allocating if nil).
func (set MonitorSet) Add(id, command, label string, ws bool, now, deadline time.Time) MonitorSet {
	if set == nil {
		set = MonitorSet{}
	}
	if id == "" {
		id = fmt.Sprintf("%s%d", monitorAnonPrefix, now.UnixNano())
		for i := 0; ; i++ {
			if _, taken := set[id]; !taken {
				break
			}
			id = fmt.Sprintf("%s%d-%d", monitorAnonPrefix, now.UnixNano(), i)
		}
	}
	set[id] = MonitorSeen{
		Command:  truncateMonitorText(command),
		Label:    truncateMonitorText(label),
		WS:       ws,
		Seen:     now,
		Deadline: deadline,
	}
	return set
}

// Refresh re-stamps an entry the liveness reconcile just proved alive, so
// a long-running monitor is never aged out by MonitorTTL on a host where
// the reconcile works. It does NOT move Deadline: a watch the harness will
// end at a fixed time is not extended by still being alive now. Unknown
// ids are ignored — the reconcile never invents entries. Reports whether
// anything changed.
func (set MonitorSet) Refresh(id string, now time.Time) bool {
	e, known := set[id]
	if !known || !e.Seen.Before(now) {
		return false
	}
	e.Seen = now
	set[id] = e
	return true
}

// Has reports whether the ledger knows this id. It is what lets a TaskStop
// be routed to the one ledger that owns the task, rather than guessed at
// across both.
func (set MonitorSet) Has(id string) bool {
	if id == "" {
		return false
	}
	_, ok := set[id]
	return ok
}

// Remove records a monitor ending — a TaskStop naming its task_id, or the
// liveness reconcile retiring an entry whose process is gone. A known id is
// deleted; an unknown non-empty id is a no-op. An empty id falls back to
// dropping the oldest entry, anon entries first.
func (set MonitorSet) Remove(id string) {
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
func (set MonitorSet) oldest(anonOnly bool) string {
	var key string
	var seen time.Time
	for id, e := range set {
		if anonOnly && !strings.HasPrefix(id, monitorAnonPrefix) {
			continue
		}
		if key == "" || e.Seen.Before(seen) {
			key, seen = id, e.Seen
		}
	}
	return key
}

// truncateMonitorText bounds a recorded command or label to
// monitorCommandMax runes, cutting on a rune boundary so the stored JSON
// stays valid UTF-8.
func truncateMonitorText(s string) string {
	if len(s) <= monitorCommandMax {
		return s
	}
	r := []rune(s)
	if len(r) <= monitorCommandMax {
		return s
	}
	return string(r[:monitorCommandMax])
}
