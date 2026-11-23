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
// NAME-COLLISION WARNING: this agent_id is Claude Code's SUB-AGENT
// instance id from the hook payload (HookCallbackInput.AgentID) and has
// NOTHING to do with tclaude's own agent identity — the `agt_`-prefixed
// actor ids in the `agents` table and the sessions.agent_id column
// (which is resolved from the conv-id in SQL, see agentForConvExpr).
// The two namespaces must never be compared or joined.
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
//     sub-agents live inside the harness process. Claude Code's
//     SessionStart(compact) is not one: a subagent's compaction fires it
//     too, and no compaction ends a sub-agent.
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
	// Prompt reports that this sub-agent raised the permission or input
	// prompt the parent is currently parked on. Only that sub-agent acting
	// again is evidence the prompt was answered; another sub-agent's hook
	// says nothing about a prompt it did not raise, nor about one the main
	// thread raised.
	Prompt bool `json:"prompt,omitempty"`
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
//
// While the parent is parked on a prompt (keepPrompting), the entry of the
// sub-agent that raised it is kept however old: it is silent because it is
// waiting on the human, not because its stop was lost, and dropping it would
// lose the attribution that lets its next hook resolve the prompt. Outside a
// prompt the flag is stale and protects nothing.
func (set SubagentSet) Sweep(now time.Time, keepPrompting bool) {
	for id, e := range set {
		if keepPrompting && e.Prompt {
			continue
		}
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
	if prev, known := set[id]; !known {
		if anon := set.oldest(true); anon != "" {
			delete(set, anon)
		}
	} else if agentType == "" {
		// Tool hooks carry agent_id but usually no agent_type — keep the
		// type recorded at SubagentStart instead of blanking it.
		agentType = prev.Type
	}
	set[id] = SubagentSeen{Type: agentType, Seen: now, Prompt: set[id].Prompt}
	return set
}

// MarkPrompt records that the sub-agent with this id raised the prompt the
// parent is parked on. An empty or unknown id is a no-op.
func (set SubagentSet) MarkPrompt(id string) {
	if e, ok := set[id]; ok && id != "" {
		e.Prompt = true
		set[id] = e
	}
}

// HasPrompt reports whether the sub-agent with this id raised the prompt the
// parent is parked on.
func (set SubagentSet) HasPrompt(id string) bool {
	return id != "" && set[id].Prompt
}

// ClearPrompt forgets that the sub-agent with this id raised a prompt.
func (set SubagentSet) ClearPrompt(id string) {
	if e, ok := set[id]; ok && e.Prompt {
		e.Prompt = false
		set[id] = e
	}
}

// ClearPrompts forgets every sub-agent's prompt, for a fresh prompt episode:
// a flag left from an earlier prompt that the main thread moved past must
// not claim a later one.
func (set SubagentSet) ClearPrompts() {
	for id, e := range set {
		if e.Prompt {
			e.Prompt = false
			set[id] = e
		}
	}
}

// Has reports whether the ledger currently believes the sub-agent with
// this id is running. Callers use it to tell an event that names a
// sub-agent tclaude has actually seen from one naming an id it never
// heard of — a distinction the harness itself does not draw. An empty id
// is never a member: it names no sub-agent.
func (set SubagentSet) Has(id string) bool {
	if id == "" {
		return false
	}
	_, ok := set[id]
	return ok
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
