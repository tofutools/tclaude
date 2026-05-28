package workflow

// advance.go holds the pure successor-advance + branch-skip logic. It is the
// shared brain behind both the manual dashboard PATCH path (Step 3) and the
// future auto-engine (Step 6): given a template, the node that just settled and
// its outcome, plus the current run-state of every node, it decides which
// successors become ready and which now-unreachable branches are skipped.
//
// Nothing here touches a store — the caller (agentd) reconstructs the template
// from the instance snapshot, maps its DB statuses onto NodeRunState, calls
// Advance, then applies the proposed transitions and recomputes instance status.

import "sort"

// NodeRunState is the coarse, storage-agnostic run state Advance reasons over.
// The caller collapses its persisted statuses onto these three:
//
//   - ready / running / awaiting_verify → NodeLive: the node has not settled, so
//     it will still fire its outgoing edges; it keeps its successors reachable.
//   - done / failed / skipped → NodeSettled: it already fired its taken edges,
//     or never will; it no longer propagates reachability.
//   - pending (anything else) → NodePending: not yet activated.
type NodeRunState int

const (
	NodePending NodeRunState = iota
	NodeLive
	NodeSettled
)

// AdvanceResult is the set of transitions Advance proposes. Both slices hold
// node ids, are disjoint, and are deterministically ordered (Ready in chart/
// edge order, Skipped sorted). The caller applies them only to nodes that are
// still pending — readying or skipping a node that has already moved on is a
// no-op — appends the matching events, then recomputes instance status.
type AdvanceResult struct {
	Ready   []string // pending successors that should become ready now
	Skipped []string // pending branches that are no longer reachable
}

// Advance computes the successor/skip transitions after settledID settles with
// the given outcome. It is pure: it reads only the template topology and the
// supplied state and returns proposed changes without mutating anything.
//
// Branch selection — an outgoing edge of settledID is "taken" when its label
// equals outcome, plus, on the success outcome (OutcomePass), every unlabeled
// edge (the success path). Each taken edge's target becomes ready once its join
// condition is met: a single-predecessor node or a JoinAny node readies
// immediately; a JoinAll node (the default for a multi-incoming node) readies
// only when every other predecessor has already settled.
//
// Skip — once the ready set is known, any still-pending node that is no longer
// forward-reachable from a live node (a currently-live node or a freshly
// readied successor) is dead and is skipped. Reachability-based skip — rather
// than "the targets of the not-taken edges" — is what makes this correct around
// loops and joins: it never skips a loop-back target or a join still fed by a
// live path, and it transitively abandons a whole dead sub-branch, not just its
// first node.
//
// Advance is single-step. A settle that BOTH supplies a join's last arrival AND
// orphans one of that join's other predecessors in the same step can leave the
// join pending until the orphaned predecessor is skipped and a subsequent
// settle re-evaluates it; the Step 6 engine will iterate Advance to a fixpoint.
func Advance(t *Template, settledID, outcome string, state map[string]NodeRunState) AdvanceResult {
	res := AdvanceResult{Ready: []string{}, Skipped: []string{}}
	if t == nil {
		return res
	}

	// The just-settled node is settled regardless of what the caller passed,
	// so a caller that hands us pre-patch state still gets correct joins.
	st := func(id string) NodeRunState {
		if id == settledID {
			return NodeSettled
		}
		return state[id]
	}

	// 1. Taken edges → ready successors, in chart order, deduped, join-gated.
	readySet := map[string]bool{}
	for _, e := range t.OutEdges(settledID) {
		if !edgeTaken(e, outcome) || readySet[e.To] {
			continue
		}
		if st(e.To) != NodePending {
			continue // already live/settled — leave it be
		}
		if joinSatisfied(t, e.To, settledID, st) {
			readySet[e.To] = true
			res.Ready = append(res.Ready, e.To)
		}
	}

	// 2. Forward reachability for skip. Seeds are the live frontier — every
	//    currently-live node plus the freshly readied successors. A pending node
	//    not reachable from a seed can never run again, so it is dead.
	out := map[string][]string{}
	for _, e := range t.Edges {
		out[e.From] = append(out[e.From], e.To)
	}
	var seeds []string
	for _, id := range sortedMermaidIDs(t.MermaidNodes) {
		if st(id) == NodeLive {
			seeds = append(seeds, id)
		}
	}
	seeds = append(seeds, res.Ready...)
	alive := reachable(seeds, out)

	for _, id := range sortedMermaidIDs(t.MermaidNodes) {
		if st(id) == NodePending && !alive[id] && !readySet[id] {
			res.Skipped = append(res.Skipped, id)
		}
	}
	return res
}

// edgeTaken reports whether an outgoing edge is followed for the given outcome.
// A labeled edge is taken when its label equals the outcome; an unlabeled edge
// is the success path, taken only on OutcomePass.
func edgeTaken(e Edge, outcome string) bool {
	if e.Label == "" {
		return outcome == OutcomePass
	}
	return e.Label == outcome
}

// joinSatisfied reports whether target's join condition is met given that
// settledID just settled on a taken edge into it. A node with at most one
// predecessor, or an explicit JoinAny, fires on this single arrival. A JoinAll
// node (the default for a multi-incoming node) fires only when every other
// predecessor has already settled (done/failed/skipped).
func joinSatisfied(t *Template, target, settledID string, st func(string) NodeRunState) bool {
	preds := map[string]bool{}
	for _, e := range t.Edges {
		if e.To == target {
			preds[e.From] = true
		}
	}
	if len(preds) <= 1 {
		return true
	}
	join := JoinAll
	if n := t.Nodes[target]; n != nil && n.Join != "" {
		join = n.Join
	}
	if join == JoinAny {
		return true
	}
	for p := range preds {
		if p == settledID {
			continue
		}
		if st(p) != NodeSettled {
			return false
		}
	}
	return true
}

// AllowedOutcomes returns the outcome labels valid for a node, sorted: an
// enum-verified node's declared values plus "fail", or {fail, pass} otherwise.
// The dashboard renders these as the manual outcome choices, and the PATCH
// handler validates a submitted outcome against them.
func (t *Template) AllowedOutcomes(nodeID string) []string {
	n := t.Nodes[nodeID]
	if n == nil {
		return []string{OutcomeFail, OutcomePass}
	}
	set := t.allowedOutcomes(n)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FailHalts reports whether a fail outcome at nodeID halts the whole instance.
// A failure halts unless the node opts into on_fail: continue AND actually has a
// |fail| outgoing edge to follow — otherwise there is nowhere for the fail path
// to go and the instance can make no further progress from there.
func (t *Template) FailHalts(nodeID string) bool {
	n := t.Nodes[nodeID]
	if n == nil || n.OnFail != OnFailContinue {
		return true
	}
	for _, e := range t.OutEdges(nodeID) {
		if e.Label == OutcomeFail {
			return false
		}
	}
	return true
}

// RebuildFromSnapshot reconstructs the topology-relevant parts of a Template
// from an instance snapshot: the mermaid chart (parsed back into edges and
// chart nodes, with Entry recomputed) and the per-node definitions the caller
// rehydrated from each node's stored detail JSON. The result carries enough for
// Advance / FailHalts / AllowedOutcomes — Edges, MermaidNodes, Nodes, Entry —
// without re-reading the template from disk, so a running instance stays
// immune to later edits of its source template.
//
// nodes may be partial or nil; a missing node def just means defaults (JoinAll
// for a multi-incoming node, {pass,fail} outcomes), which is exactly what an
// empty snapshot detail would imply. The mermaid must parse, or an error is
// returned.
func RebuildFromSnapshot(mermaid string, nodes map[string]*Node) (*Template, error) {
	dir, mnodes, edges, err := parseMermaid(mermaid)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = map[string]*Node{}
	}
	t := &Template{
		Mermaid:      mermaid,
		Direction:    dir,
		MermaidNodes: mnodes,
		Edges:        edges,
		Nodes:        nodes,
	}
	t.Entry = t.computeEntry()
	return t, nil
}
