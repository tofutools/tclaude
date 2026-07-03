package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SubagentSet is the per-session ledger of sub-agents believed to be
// running right now, keyed by the harness's agent_id (Claude Code stamps
// it on every hook fired from inside a sub-agent). It is persisted as
// JSON in sessions.subagents_json and is the source of truth behind
// sessions.subagent_count / the dashboard's "🤖+N" badge.
//
// Why a ledger and not a bare counter: the hook stream is LOSSY. Claude
// Code fires no hooks at all on a user interrupt (anthropics/
// claude-code#11189), SubagentStop has no documented guarantee for
// aborts/errors/process death, and a hook callback can itself fail (DB
// busy, hook timeout, binary replaced mid-flight). A +1/-1 counter
// turns every lost event into permanent drift; a ledger self-heals:
//
//   - a lost SubagentStart is repaired by Sight() — the sub-agent's own
//     tool hooks re-add it the moment it is next seen;
//   - a lost SubagentStop is repaired by Sweep() — an entry that stops
//     being seen ages out after SubagentTTL;
//   - known-zero boundaries (main-thread SessionStart, session exit,
//     the .jsonl interrupt marker) clear the ledger outright, since
//     sub-agents live inside the harness process.
//
// Entries whose agent_id is unknown (a SubagentStart payload without
// one) get a synthetic "anon-…" key so legacy counting still works;
// Sight() folds an anon entry into the first real id it sees.
type SubagentSet map[string]SubagentSeen

// SubagentSeen is one ledger entry: the sub-agent's type (for
// debugging/tooltips; may be empty) and when a hook last proved it alive.
type SubagentSeen struct {
	Type string    `json:"type,omitempty"`
	Seen time.Time `json:"seen"`
}

// SubagentTTL is how long a ledger entry survives without any hook
// naming its agent_id before Sweep/LiveCount treat it as a phantom
// (a sub-agent whose SubagentStop was lost). Generous on purpose: a
// sub-agent deep in a long LLM turn fires no tool hooks for minutes.
const SubagentTTL = 15 * time.Minute

const subagentAnonPrefix = "anon-"

// ParseSubagentSet decodes a sessions.subagents_json value. "" (the
// column default, and what an empty set encodes to) and malformed JSON
// both yield an empty set — the ledger is best-effort state, never a
// reason to fail a hook.
func ParseSubagentSet(s string) SubagentSet {
	if s == "" {
		return nil
	}
	var set SubagentSet
	if err := json.Unmarshal([]byte(s), &set); err != nil {
		return nil
	}
	return set
}

// Encode serialises the set for storage. An empty/nil set encodes to ""
// (not "{}"): the read side uses "" to tell "ledger maintained, empty"
// apart from nothing at all only via subagent_count, and keeping the
// column at its DEFAULT for the common no-subagents case avoids churn.
func (set SubagentSet) Encode() string {
	if len(set) == 0 {
		return ""
	}
	b, err := json.Marshal(set)
	if err != nil {
		return ""
	}
	return string(b)
}

// Sweep deletes entries not seen within SubagentTTL of now — the
// self-healing for a lost SubagentStop. Safe on a nil set.
func (set SubagentSet) Sweep(now time.Time) {
	for id, e := range set {
		if now.Sub(e.Seen) > SubagentTTL {
			delete(set, id)
		}
	}
}

// LiveCount reports how many entries are within SubagentTTL of now,
// without mutating the set. This is what read surfaces (the dashboard)
// use, so a phantom stops being displayed as soon as it expires even if
// no hook has fired since to Sweep it from storage.
func (set SubagentSet) LiveCount(now time.Time) int {
	n := 0
	for _, e := range set {
		if now.Sub(e.Seen) <= SubagentTTL {
			n++
		}
	}
	return n
}

// Add records a sub-agent starting. An empty id (a SubagentStart payload
// that carried no agent_id) gets a synthetic anon key so the count still
// tracks; Sight later folds it into the real id if one shows up.
// Returns the set (allocating if nil).
func (set SubagentSet) Add(id, agentType string, now time.Time) SubagentSet {
	if set == nil {
		set = SubagentSet{}
	}
	if id == "" {
		id = fmt.Sprintf("%s%d", subagentAnonPrefix, now.UnixNano())
		for i := 0; ; i++ {
			if _, taken := set[id]; !taken {
				break
			}
			id = fmt.Sprintf("%s%d-%d", subagentAnonPrefix, now.UnixNano(), i)
		}
	}
	set[id] = SubagentSeen{Type: agentType, Seen: now}
	return set
}

// Sight records live evidence of a sub-agent: any hook fired from inside
// it (PreToolUse, PostToolUse, …) proves it is running. A known id gets
// its Seen refreshed; an unknown id is added — that is the repair for a
// lost SubagentStart. A newly sighted id consumes the oldest anon entry,
// on the theory that the anon entry was this very sub-agent counted at a
// Start whose payload lacked the id (otherwise one sub-agent would count
// twice). Returns the set (allocating if nil).
func (set SubagentSet) Sight(id, agentType string, now time.Time) SubagentSet {
	if id == "" {
		return set
	}
	if set == nil {
		set = SubagentSet{}
	}
	if _, known := set[id]; !known {
		if anon := set.oldest(true); anon != "" {
			delete(set, anon)
		}
	}
	set[id] = SubagentSeen{Type: agentType, Seen: now}
	return set
}

// Remove records a sub-agent ending. A known id is deleted; an unknown
// non-empty id is a no-op (its Start was lost, or a sibling event —
// e.g. a sub-agent-context SessionEnd — already removed it). An empty
// id (a SubagentStop payload without agent_id) falls back to legacy
// decrement semantics: drop the oldest entry, anon entries first.
func (set SubagentSet) Remove(id string) {
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

// oldest returns the key with the earliest Seen — restricted to
// synthetic anon entries when anonOnly is set — or "" when none match.
func (set SubagentSet) oldest(anonOnly bool) string {
	var key string
	var seen time.Time
	for id, e := range set {
		if anonOnly && !strings.HasPrefix(id, subagentAnonPrefix) {
			continue
		}
		if key == "" || e.Seen.Before(seen) {
			key, seen = id, e.Seen
		}
	}
	return key
}
