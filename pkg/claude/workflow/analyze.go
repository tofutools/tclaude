package workflow

import (
	"fmt"
	"strings"
)

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
		entryList := strings.Join(t.Entry, ", ")
		for _, id := range ids {
			if !reached[id] {
				add("node %q is unreachable: no path to it from any entry node (entry: %s)", id, entryList)
			}
		}
	}

	// 3. Terminal sanity / 2. can-reach-terminal. With no terminal the workflow
	//    can never complete and every node trivially fails the reverse walk, so
	//    report that single root cause instead of flagging every node.
	if len(terminals) == 0 {
		add("flow.mmd: no terminal node — every node has an outgoing edge, so the workflow can never complete")
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
