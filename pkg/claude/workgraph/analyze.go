package workgraph

import (
	"fmt"
	"sort"
	"strings"
)

// Analyze re-runs the static topology analysis on t and returns its non-fatal
// warnings (topology smells) in the same deterministic order load() produces.
// load() runs this during template loading and stores the result on t.Warnings;
// RebuildFromSnapshot deliberately skips it (the advance path doesn't need it),
// so callers that reconstruct a Template from an instance snapshot use Analyze
// to recover the warnings for display. Hard problems are discarded here — a
// snapshot was a valid template when it was instantiated, so re-flagging them
// would be noise. Recomputes from scratch, so it is idempotent.
func (t *Template) Analyze() []string {
	t.Warnings = nil
	t.analyzeGraph(func(string, ...any) {}) // discard hard problems; warnings land on t.Warnings
	return t.Warnings
}

// analyzeGraph runs static topology checks over the parsed chart (Edges /
// MermaidNodes / Entry). Hard problems are appended via add; non-fatal smells
// accumulate on t.Warnings.
//
// It assumes the structural checks in validate() (chart nodes exist, entry is
// computed/validated) have already run, and only walks when the result would be
// meaningful: reachability needs at least one entry that actually exists, and
// the per-node can-reach-terminal walk is skipped when there is no terminal at
// all (a single "no terminal" problem is the real root cause then).
//
// Every edge endpoint is a declared MermaidNode — the mermaid parser records a
// node for anything it sees on an edge — so the chart node set (MermaidNodes)
// covers every id the walks can touch; no endpoint dangles outside it.
func (t *Template) analyzeGraph(add func(string, ...any)) {
	if len(t.MermaidNodes) == 0 {
		return
	}

	// Forward and reverse adjacency over the chart node set.
	out := map[string][]string{}
	in := map[string][]string{}
	for _, e := range t.Edges {
		out[e.From] = append(out[e.From], e.To)
		in[e.To] = append(in[e.To], e.From)
	}

	ids := sortedMermaidIDs(t.MermaidNodes)

	// Terminals: chart nodes with no outgoing edge.
	var terminals []string
	for _, id := range ids {
		if len(out[id]) == 0 {
			terminals = append(terminals, id)
		}
	}

	// 1. Reachability — every node must be reachable from some entry.
	//    Only walk from entries that actually exist; if none do, the missing /
	//    empty-entry problems raised in validate() already cover it, and walking
	//    from nothing would spuriously flag every node as unreachable.
	var validEntries []string
	for _, id := range t.Entry {
		if _, ok := t.MermaidNodes[id]; ok {
			validEntries = append(validEntries, id)
		}
	}
	if len(validEntries) > 0 {
		reached := reachable(validEntries, out)
		// Report the entries actually walked from, not the raw declared list —
		// any non-existent declared entry is flagged separately in validate().
		entryList := strings.Join(validEntries, ", ")
		for _, id := range ids {
			if !reached[id] {
				add("node %q is unreachable: no path to it from any entry node (entry: %s)", id, entryList)
			}
		}
	}

	// 3. Terminal sanity / 2. can-reach-terminal. With no terminal the workgraph
	//    can never complete and every node trivially fails the reverse walk, so
	//    report that single root cause instead of flagging every node.
	if len(terminals) == 0 {
		add("flow.mmd: no terminal node — every node has an outgoing edge, so the workgraph can never complete")
	} else {
		canReach := reachable(terminals, in)
		for _, id := range ids {
			if !canReach[id] {
				add("node %q cannot reach any terminal node: an instance could never complete from there", id)
			}
		}
	}

	// 4. Enum coverage — an enum value with no matching outgoing edge is a
	//    forgotten-edge smell (the instance stops on that outcome). Warn only;
	//    it is sometimes a deliberate terminal-on-that-outcome.
	for _, id := range sortedNodeIDs(t.Nodes) {
		n := t.Nodes[id]
		if n.Verify.Kind != VerifyEnum {
			continue
		}
		if _, ok := t.MermaidNodes[id]; !ok {
			continue // orphan node yaml, already flagged in validate()
		}
		labeled := map[string]bool{}
		for _, e := range t.OutEdges(id) {
			if e.Label != "" {
				labeled[e.Label] = true
			}
		}
		for _, v := range n.Verify.Values {
			if !labeled[v] {
				t.Warnings = append(t.Warnings, fmt.Sprintf(
					"node %q: enum value %q has no outgoing edge (-->|%s| ...); an instance will stop on that outcome", id, v, v))
			}
		}
	}

	// 5. Loop exit (JOH-39) — a cycle (a back-edge loop, e.g. test -->|fail|
	//    implement) with NO edge leaving it can only ever terminate by hitting the
	//    visit cap (a forced failure), never by completing: there is no
	//    break-on-pass exit. Warn so the author adds the exit edge (e.g. the |pass|
	//    branch out of the loop). Emitted once per cycle, from its smallest member,
	//    so the warning order stays deterministic.
	for _, id := range ids {
		scc := stronglyConnected(id, out, in)
		selfLoop := false
		for _, nb := range out[id] {
			if nb == id {
				selfLoop = true
			}
		}
		if len(scc) < 2 && !selfLoop {
			continue // not part of a cycle
		}
		members := sortedSet(scc)
		if members[0] != id {
			continue // only the smallest member of the cycle emits, to dedupe
		}
		hasExit := false
		for m := range scc {
			for _, nb := range out[m] {
				if !scc[nb] {
					hasExit = true
				}
			}
		}
		if !hasExit {
			t.Warnings = append(t.Warnings, fmt.Sprintf(
				"loop {%s} has no exit edge: it can only terminate by hitting max_visits, never by completing — add a break/exit edge (e.g. a |pass| branch out of the loop)",
				strings.Join(members, ", ")))
		}
	}
}

// stronglyConnected returns the strongly-connected component containing id: the
// nodes both reachable FROM id (via out) and able to REACH id (via in). A node
// with no cycle yields just {id}; a real cycle yields ≥2 members (or {id} when id
// has a self-edge, which the caller checks separately).
func stronglyConnected(id string, out, in map[string][]string) map[string]bool {
	fwd := reachable([]string{id}, out)
	back := reachable([]string{id}, in)
	scc := map[string]bool{}
	for n := range fwd {
		if back[n] {
			scc[n] = true
		}
	}
	return scc
}

// sortedSet returns the keys of a set, sorted — for deterministic warning text.
func sortedSet(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reachable returns the set of nodes reachable from any seed by following adj
// (a plain BFS). Seeds are included in the result.
func reachable(seeds []string, adj map[string][]string) map[string]bool {
	seen := make(map[string]bool, len(adj))
	queue := make([]string, 0, len(seeds))
	for _, s := range seeds {
		if !seen[s] {
			seen[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}
