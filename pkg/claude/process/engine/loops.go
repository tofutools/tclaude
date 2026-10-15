package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Bounded loops (tcl-eu70x9).
//
// A decision outcome bounded by model.LoopBound goes back to earlier work.
// Authoring proves the shape (model.validateLoops): the graph without bounded
// edges is acyclic, a loop body is entered only at the edge's target, left
// only through decisions, holds no join: any, and two bodies nest or are
// disjoint. On that shape a round is well defined, and the reducer keeps its
// one-way settlement inside a round:
//
//   - Taking the edge with rounds left starts the next round: every body node
//     goes back to pending, every edge leaving a body node back to unresolved,
//     and the target becomes ready. Attempt counters are never reset — each
//     task gets a fresh attempt window above the attempts it already ran, and
//     a decision's window counter moves on, so evidence and staleness checks
//     keep their monotonic identities.
//   - An edge that LEAVES the body is not closed while the loop is live: the
//     decision may still send the work round again, and a closed exit would
//     have skipped downstream work for good. Such exits stay unresolved until
//     the decision completes some other way, and are closed then.
//   - Past the limit, the decision follows its onExhausted outcome, or the
//     outcome that goes back is refused and the decider picks another.
//
// Checkpoint.Iterations counts how often each bounded edge was taken in the
// current round of every loop around it: the round is that count plus one.

// ErrLoopExhausted refuses a verdict that would start a round past its
// loop's limit when the loop names no onExhausted outcome.
var ErrLoopExhausted = errors.New("process loop has used up its rounds")

// definitionLoop is one prepared bounded back edge.
type definitionLoop struct {
	edge          int
	from, to      int
	outcome       string
	maxIterations int
	onExhausted   string
	// body holds the prepared indices the round repeats, sorted: the authored
	// body nodes, their compound stages, and their subprocess bodies.
	body []int
	// exits are the edge indices leaving the body.
	exits []int
	// inner are the loops nested inside this one, whose rounds restart with it.
	inner []int
}

// pendingLoop is one scope's loop before the edge indices exist.
type pendingLoop struct {
	from, outcome, to string
	maxIterations     int
	onExhausted       string
	body              []string
	exits             [][2]string
}

// prepareLoops turns the scopes' loops into prepared ones once every node and
// edge has its index.
func prepareLoops(definition *Definition, pending []pendingLoop) error {
	edgeIndex := func(from, outcome string) (int, bool) {
		index, ok := definition.index[from]
		if !ok {
			return 0, false
		}
		for _, candidate := range definition.nodes[index].outgoing {
			if definition.edges[candidate].outcome == outcome {
				return candidate, true
			}
		}
		return 0, false
	}
	for _, scoped := range pending {
		edge, ok := edgeIndex(scoped.from, scoped.outcome)
		if !ok {
			return fmt.Errorf("%w: loop edge %q/%q is not prepared", ErrInvalidDefinition, scoped.from, scoped.outcome)
		}
		loop := definitionLoop{
			edge: edge, from: definition.edges[edge].fromIndex, to: definition.edges[edge].toIndex,
			outcome: scoped.outcome, maxIterations: scoped.maxIterations, onExhausted: scoped.onExhausted,
		}
		for _, id := range scoped.body {
			index, ok := definition.index[id]
			if !ok {
				return fmt.Errorf("%w: loop body node %q is not prepared", ErrInvalidDefinition, id)
			}
			loop.body = append(loop.body, index)
			node := definition.nodes[index]
			loop.body = append(loop.body, node.children...)
			if node.kind == definitionSubprocess {
				for inner := index + 1; inner < node.scopeEnd; inner++ {
					loop.body = append(loop.body, inner)
					loop.body = append(loop.body, definition.nodes[inner].children...)
				}
			}
		}
		slices.Sort(loop.body)
		loop.body = slices.Compact(loop.body)
		for _, exit := range scoped.exits {
			index, ok := edgeIndex(exit[0], exit[1])
			if !ok {
				return fmt.Errorf("%w: loop exit %q/%q is not prepared", ErrInvalidDefinition, exit[0], exit[1])
			}
			loop.exits = append(loop.exits, index)
		}
		definition.loops = append(definition.loops, loop)
	}
	for index := range definition.edges {
		definition.edges[index].loop = -1
	}
	for li, loop := range definition.loops {
		definition.edges[loop.edge].loop = li
		source := &definition.nodes[loop.from]
		source.sourceOf = append(source.sourceOf, li)
		for _, exit := range loop.exits {
			definition.edges[exit].exitOf = append(definition.edges[exit].exitOf, li)
		}
		for _, member := range loop.body {
			definition.nodes[member].loops = append(definition.nodes[member].loops, li)
		}
		for other, candidate := range definition.loops {
			if other != li && len(candidate.body) < len(loop.body) && containsAll(loop.body, candidate.body) {
				definition.loops[li].inner = append(definition.loops[li].inner, other)
			}
		}
	}
	// Innermost first: a node's round is the round of the tightest loop.
	for index := range definition.nodes {
		slices.SortStableFunc(definition.nodes[index].loops, func(a, b int) int {
			return len(definition.loops[a].body) - len(definition.loops[b].body)
		})
	}
	return nil
}

func containsAll(sorted, subset []int) bool {
	for _, value := range subset {
		if _, ok := slices.BinarySearch(sorted, value); !ok {
			return false
		}
	}
	return true
}

// scopeLoops collects one prepared scope's loops under its id prefix.
func scopeLoops(tmpl *model.Template, prefix string) []pendingLoop {
	var out []pendingLoop
	for _, loop := range model.Loops(tmpl, model.NormalizeEdges(tmpl)) {
		pending := pendingLoop{
			from: prefix + loop.From, outcome: loop.Outcome, to: prefix + loop.To,
			maxIterations: loop.MaxIterations, onExhausted: loop.OnExhausted,
		}
		for _, id := range loop.Body {
			pending.body = append(pending.body, prefix+id)
		}
		for _, exit := range loop.Exits {
			pending.exits = append(pending.exits, [2]string{prefix + exit.From, exit.Outcome})
		}
		out = append(out, pending)
	}
	return out
}

// isBoundedOutcome reports whether a decision's outcome is a bounded back
// edge, and which loop.
func (d *Definition) loopOf(node definitionNode, outcome string) (int, bool) {
	for _, li := range node.sourceOf {
		if d.loops[li].outcome == outcome {
			return li, true
		}
	}
	return 0, false
}

// iterationsTaken is how often a loop's edge was taken in the current round of
// the loops around it.
func iterationsTaken(checkpoint Checkpoint, definition *Definition, loop definitionLoop) int {
	from := definition.nodes[loop.from]
	return checkpoint.Iterations[from.id][loop.outcome]
}

// loopHasRounds reports whether taking the loop's edge may start another round.
func loopHasRounds(checkpoint Checkpoint, definition *Definition, loop definitionLoop) bool {
	return iterationsTaken(checkpoint, definition, loop)+1 < loop.maxIterations
}

// routeDecision is the decision's verdict as the reducer applies it: a bounded
// outcome with rounds left starts the next round (iterate), one past the limit
// follows onExhausted, and every other verdict settles as authored.
func routeDecision(checkpoint Checkpoint, definition *Definition, node definitionNode, verdict string) (route string, iterate int, err error) {
	li, bounded := definition.loopOf(node, verdict)
	if !bounded {
		return verdict, -1, nil
	}
	loop := definition.loops[li]
	if loopHasRounds(checkpoint, definition, loop) {
		return verdict, li, nil
	}
	if loop.onExhausted == "" {
		return "", -1, fmt.Errorf("%w: %w: %q has run all %d rounds of its loop back to %q; choose another outcome",
			ErrInvalidDecisionVerdict, ErrLoopExhausted, verdict, loop.maxIterations, definition.nodes[loop.to].id)
	}
	return loop.onExhausted, -1, nil
}

// iterateLoop starts the next round of one loop. The decision that took the
// edge is ready and every other body node has finished its round, so nothing
// in the body has a command, an obligation, or a parked branch to strand.
func iterateLoop(next *Checkpoint, definition *Definition, li int) error {
	loop := definition.loops[li]
	for _, index := range loop.body {
		node := definition.nodes[index]
		status := next.Nodes[node.id]
		if index == loop.from {
			// A person decides a ready decision; an agent decider is running.
			if status != NodeReady && status != NodeRunning {
				return fmt.Errorf("%w: loop decision %q is %q, not deciding", ErrInvalidTransition, node.id, status)
			}
			continue
		}
		if status != NodeDone && status != NodeSkipped {
			return fmt.Errorf("%w: node %q is %q; a loop starts its next round only once its body has finished",
				ErrInvalidTransition, node.id, status)
		}
	}
	from := definition.nodes[loop.from]
	if next.Iterations == nil {
		next.Iterations = make(map[string]map[string]int, 1)
	}
	if next.Iterations[from.id] == nil {
		next.Iterations[from.id] = make(map[string]int, 1)
	}
	next.Iterations[from.id][loop.outcome]++
	// Loops inside this body start their own count again with the new round.
	for _, inner := range loop.inner {
		innerLoop := definition.loops[inner]
		source := definition.nodes[innerLoop.from].id
		delete(next.Iterations[source], innerLoop.outcome)
		if len(next.Iterations[source]) == 0 {
			delete(next.Iterations, source)
		}
	}
	for _, index := range loop.body {
		node := definition.nodes[index]
		next.Nodes[node.id] = NodePending
		switch {
		case node.awaitsHuman() && node.parent < 0:
			// An authored decision's window counter: a verdict formed in an
			// earlier round names an older window and is refused as stale.
			if next.Attempts[node.id] == math.MaxInt {
				return fmt.Errorf("%w: decision %q window counter would overflow", ErrInvalidTransition, node.id)
			}
			recordAttempt(next, node.id, next.Attempts[node.id]+1)
		case node.runsCommand():
			if err := openLoopAttemptWindow(next, definition, node); err != nil {
				return err
			}
		}
		for _, edgeIndex := range node.outgoing {
			edge := definition.edges[edgeIndex]
			next.Edges[edge.from][edge.outcome] = EdgeUnresolved
		}
	}
	to := definition.nodes[loop.to]
	next.Nodes[to.id] = NodeReady
	if to.awaitsHuman() {
		addObligation(next, to.id)
	}
	return nil
}

// openLoopAttemptWindow gives a task that already ran a fresh authored-size
// attempt window for the new round, above the attempts it has used. Gates and
// approval-gated plans derive their ceilings from their anchors, so only a
// task that owns its ceiling gets an entry.
func openLoopAttemptWindow(next *Checkpoint, definition *Definition, node definitionNode) error {
	attempts := next.Attempts[node.id]
	if attempts == 0 {
		return nil
	}
	if _, gate := gateAnchor(definition, node); gate || approvalGatedPlan(definition, node) {
		return nil
	}
	if attempts > math.MaxInt-node.maxAttempts {
		return fmt.Errorf("%w: node %q attempt ceiling would overflow", ErrInvalidTransition, node.id)
	}
	raised := attempts + node.maxAttempts
	if raised <= attemptCeiling(*next, node) {
		return nil
	}
	if next.AttemptCeilings == nil {
		next.AttemptCeilings = make(map[string]int, 1)
	}
	next.AttemptCeilings[node.id] = raised
	return nil
}

// deferredExit reports whether an edge leaves the body of a loop that is still
// live — its decision has not finished — so closing it now could wrongly skip
// work the next round may yet reach.
func deferredExit(checkpoint Checkpoint, definition *Definition, edgeIndex int) bool {
	for _, li := range definition.edges[edgeIndex].exitOf {
		switch checkpoint.Nodes[definition.nodes[definition.loops[li].from].id] {
		case NodeDone, NodeSkipped:
		default:
			return true
		}
	}
	return false
}

// LoopRound is the round a node of a loop body is in, out of its loop's
// limit. Round counts from 1.
type LoopRound struct {
	Round int
	Of    int
	// From and Outcome name the loop's bounded edge.
	From    string
	Outcome string
}

// RoundOf reports the round of the innermost loop around a node, and false for
// a node in no loop.
func (d *Definition) RoundOf(checkpoint Checkpoint, nodeID string) (LoopRound, bool) {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok || len(node.loops) == 0 {
		return LoopRound{}, false
	}
	loop := d.loops[node.loops[0]]
	return LoopRound{Round: iterationsTaken(checkpoint, d, loop) + 1, Of: loop.maxIterations,
		From: d.nodes[loop.from].id, Outcome: loop.outcome}, true
}

// LoopEdge is a bounded back edge as the read surfaces describe it.
type LoopEdge struct {
	From          string `json:"from"`
	Outcome       string `json:"outcome"`
	To            string `json:"to"`
	MaxIterations int    `json:"maxIterations"`
	OnExhausted   string `json:"onExhausted,omitempty"`
	// Round is the round the loop's body is in now.
	Round int `json:"round"`
}

// Loops describes every bounded back edge with its current round.
func (d *Definition) Loops(checkpoint Checkpoint) []LoopEdge {
	if d == nil {
		return nil
	}
	out := make([]LoopEdge, 0, len(d.loops))
	for _, loop := range d.loops {
		out = append(out, LoopEdge{
			From: d.nodes[loop.from].id, Outcome: loop.outcome, To: d.nodes[loop.to].id,
			MaxIterations: loop.maxIterations, OnExhausted: loop.onExhausted,
			Round: iterationsTaken(checkpoint, d, loop) + 1,
		})
	}
	return out
}

// LoopStep is what one verdict did to a loop: started round Round (Iterated),
// or found the rounds used up and followed onExhausted.
type LoopStep struct {
	Loop      LoopEdge
	Iterated  bool
	Exhausted bool
}

// LoopStepOf derives what a committed verdict did to a loop from the
// checkpoints around it, for evidence.
func (d *Definition) LoopStepOf(nodeID, verdict string, before, after Checkpoint) (LoopStep, bool) {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok {
		return LoopStep{}, false
	}
	li, bounded := d.loopOf(node, verdict)
	if !bounded {
		return LoopStep{}, false
	}
	loop := d.loops[li]
	edge := LoopEdge{
		From: nodeID, Outcome: verdict, To: d.nodes[loop.to].id,
		MaxIterations: loop.maxIterations, OnExhausted: loop.onExhausted,
		Round: iterationsTaken(after, d, loop) + 1,
	}
	if iterationsTaken(after, d, loop) > iterationsTaken(before, d, loop) {
		return LoopStep{Loop: edge, Iterated: true}, true
	}
	edge.Round = iterationsTaken(before, d, loop) + 1
	return LoopStep{Loop: edge, Exhausted: true}, true
}

// validateIterations is the load-boundary check of the round counters: each
// names a bounded edge and stays below its limit.
func validateIterations(checkpoint Checkpoint, definition *Definition) error {
	for from, outcomes := range checkpoint.Iterations {
		node, ok := definitionNodeByID(definition, from)
		if !ok || len(outcomes) == 0 {
			return fmt.Errorf("%w: iterations names %q, which sources no loop", ErrInvalidCheckpoint, from)
		}
		for outcome, taken := range outcomes {
			li, bounded := definition.loopOf(node, outcome)
			if !bounded {
				return fmt.Errorf("%w: iterations names %q/%q, which is not a bounded loop edge", ErrInvalidCheckpoint, from, outcome)
			}
			if limit := definition.loops[li].maxIterations; taken < 1 || taken >= limit {
				return fmt.Errorf("%w: loop %q/%q has taken %d rounds back, outside 1..%d",
					ErrInvalidCheckpoint, from, outcome, taken, limit-1)
			}
		}
	}
	return nil
}
