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
// condition is met (see joinReady): a single-predecessor node or a JoinAny node
// readies on this one arrival; a JoinAll node readies only when no other
// predecessor can still deliver a token.
//
// Join — JoinAll is "every predecessor ON A TAKEN PATH is done", NOT "every
// predecessor, period". The distinction is decided by reachability rather than
// by waiting for a literal settled-state on every incoming node, because a raw
// "wait for all predecessors" rule deadlocks the moment a node has a loop-back
// or not-taken predecessor (e.g. the example's `implement`, fed by
// `test -->|fail|` and `review -->|changes|`, would wait forever on its own
// not-yet-run successors). A JoinAll target fires when no still-live node can
// reach one of its predecessors WITHOUT passing through the target itself —
// which correctly (a) ignores downstream loop-back predecessors, (b) ignores
// predecessors on a not-taken branch (they will be skipped), and (c) still
// waits on a genuinely concurrent live predecessor of a parallel fork-join.
//
// Skip — once the ready set is known, any still-pending node no longer
// forward-reachable from the live frontier (currently-live nodes + freshly
// readied successors) is dead and is skipped. This never skips a loop-back
// target or a join still fed by a live path, and it transitively abandons a
// whole dead sub-branch, not just its first node.
//
// Advance is single-step and does not re-enter loops: a target already past
// pending (live/settled) is left alone, so a loop-back into an already-run node
// is a no-op here. Re-running a node across a loop iteration (visit counting,
// status reset) is the Step 6 engine's job; this helper computes one settle's
// immediate consequences.
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

	// Forward adjacency over the chart node set.
	out := map[string][]string{}
	for _, e := range t.Edges {
		out[e.From] = append(out[e.From], e.To)
	}

	// The live frontier as it stands BEFORE this settle's readies: every node
	// still live (the just-settled node is excluded — st() forces it settled).
	var liveNodes []string
	for _, id := range sortedMermaidIDs(t.MermaidNodes) {
		if st(id) == NodeLive {
			liveNodes = append(liveNodes, id)
		}
	}

	// 1. Taken edges → ready successors, in chart order, deduped, join-gated.
	readySet := map[string]bool{}
	for _, e := range t.OutEdges(settledID) {
		if !edgeTaken(e, outcome) || readySet[e.To] {
			continue
		}
		if st(e.To) != NodePending {
			continue // already live/settled — leave it be (loop-back / re-entry)
		}
		if joinReady(t, e.To, settledID, liveNodes, out) {
			readySet[e.To] = true
			res.Ready = append(res.Ready, e.To)
		}
	}

	// 2. Forward reachability for skip. Seeds are the live frontier plus the
	//    freshly readied successors. A pending node not reachable from a seed
	//    can never run again, so it is dead.
	seeds := append(append([]string(nil), liveNodes...), res.Ready...)
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

// joinReady reports whether target may become ready now that settledID has
// delivered a token along a taken edge into it. A node with at most one
// predecessor, or an explicit JoinAny, fires on this single arrival. A JoinAll
// node (the default for a multi-incoming node) fires only when no OTHER
// predecessor can still deliver a future token — i.e. no currently-live node
// can reach that predecessor without passing through target. settledID has just
// delivered, so it never blocks; a predecessor reachable from the live frontier
// (including a live predecessor itself, as a BFS seed) still owes a token and
// holds the join.
func joinReady(t *Template, target, settledID string, liveNodes []string, out map[string][]string) bool {
	var preds []string
	seenPred := map[string]bool{}
	for _, e := range t.Edges {
		if e.To == target && !seenPred[e.From] {
			seenPred[e.From] = true
			preds = append(preds, e.From)
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
	// JoinAll: a predecessor still owes a token if a live node can still reach
	// it without routing through target (a loop that re-enters target first does
	// not count as "before target runs").
	reachFromLive := reachableExcluding(liveNodes, out, target)
	for _, p := range preds {
		if p == settledID {
			continue // just delivered
		}
		if reachFromLive[p] {
			return false // p can still deliver — hold the join
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

// reachableExcluding is a BFS like analyze.go's reachable(), but with one node
// treated as a wall: it is never seeded, never traversed through, and never
// appears in the result. The JoinAll check uses it to ask "can a live node
// still reach this predecessor WITHOUT first routing back through the join
// target" — a loop that re-enters the target before reaching the predecessor
// must not count as the predecessor still owing a token.
func reachableExcluding(seeds []string, adj map[string][]string, exclude string) map[string]bool {
	seen := make(map[string]bool, len(adj))
	queue := make([]string, 0, len(seeds))
	for _, s := range seeds {
		if s == exclude || seen[s] {
			continue
		}
		seen[s] = true
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if next == exclude || seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	return seen
}
