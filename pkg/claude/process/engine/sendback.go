package engine

import (
	"fmt"
	"math"
	"slices"
)

// Sending a run back to an earlier step (tk_e2fa2bd0 3b).
//
// A person sends the run back to a step of the template's own graph that has
// finished. Everything downstream of it — every node reachable by its forward
// edges, with their compound stages, agreement votes and subprocess bodies —
// goes back to pending, as if it had never run, and the step is ready again:
//
//   - edges leaving those nodes are unresolved again; edges into the step from
//     upstream stay as they were, which is what makes it ready;
//   - work still out downstream is withdrawn (agent steps) or the send-back is
//     refused (a program, which cannot be called back);
//   - decision windows, approval and vote obligations, and parked branches
//     downstream are dropped, so a verdict or resolution formed before the
//     rewind is refused as stale;
//   - attempt counters never move backwards: a task that already ran gets a
//     fresh authored-size window above them, and a loop decision's window
//     counter moves on, exactly as a new loop round does;
//   - a loop wholly downstream starts counting its rounds again.
//
// A parallel branch outside the rewind that already reached a node inside it
// would leave that node half-arrived, so such a send-back is refused.

// SendBack is the reducer payload for sending the run back to a step.
type SendBack struct {
	NodeID string `json:"nodeId"`
}

// SendBackSet is the prepared node ids a send-back to nodeID resets, the
// step itself first, or false when the step cannot be sent back to.
func (d *Definition) SendBackSet(nodeID string) ([]string, bool) {
	index, ok := d.index[nodeID]
	if !ok || !sendBackTarget(d.nodes[index]) {
		return nil, false
	}
	set := sendBackClosure(d, index)
	ids := make([]string, 0, len(set))
	ids = append(ids, nodeID)
	for _, i := range set {
		if i != index {
			ids = append(ids, d.nodes[i].id)
		}
	}
	return ids, true
}

// sendBackTarget reports whether a node can be sent back to: a step of the
// template's own graph, not its start or an end.
func sendBackTarget(node definitionNode) bool {
	if node.parent >= 0 || node.scope >= 0 {
		return false
	}
	switch node.kind {
	case definitionTask, definitionDecision, definitionParallel, definitionCompound, definitionSubprocess:
		return true
	}
	return false
}

// sendBackClosure is the sorted prepared indices downstream of index, index
// included: forward edges (bounded loop edges go back, not down), and every
// node's compound stages, gate votes and subprocess body.
func sendBackClosure(definition *Definition, index int) []int {
	seen := map[int]bool{}
	queue := []int{index}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if seen[i] {
			continue
		}
		seen[i] = true
		node := definition.nodes[i]
		for _, edgeIndex := range node.outgoing {
			if edge := definition.edges[edgeIndex]; edge.loop < 0 {
				queue = append(queue, edge.toIndex)
			}
		}
		queue = append(queue, node.children...)
		if node.scopeEnd > i {
			for inner := i + 1; inner < node.scopeEnd; inner++ {
				queue = append(queue, inner)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

func applySendBack(next *Checkpoint, definition *Definition, back SendBack) error {
	if next.Status != RunRunning || doomed(*next) {
		return fmt.Errorf("%w: run is not running", ErrStaleResolution)
	}
	index, ok := definition.index[back.NodeID]
	if !ok {
		return fmt.Errorf("%w: send_back names unknown node %q", ErrInvalidTransition, back.NodeID)
	}
	target := definition.nodes[index]
	if !sendBackTarget(target) {
		return fmt.Errorf("%w: %q is not a step of the template's own graph that a run can be sent back to", ErrInvalidTransition, target.id)
	}
	if next.Nodes[target.id] != NodeDone {
		return fmt.Errorf("%w: step %q has not finished (it is %s); only a finished step can be sent back to",
			ErrStaleResolution, target.id, next.Nodes[target.id])
	}
	set := sendBackClosure(definition, index)
	in := make(map[int]bool, len(set))
	for _, i := range set {
		in[i] = true
	}
	// Into the middle of a loop body: the body's earlier steps keep the exits
	// they settled when the loop last finished, which a new round could take
	// into steps already closed. Go back to where the loop starts instead.
	for _, loop := range definition.loops {
		if in[loop.from] && !in[loop.to] {
			return fmt.Errorf("%w: %q is inside the loop that goes back to %q; send the run back to %q instead",
				ErrInvalidTransition, target.id, definition.nodes[loop.to].id, definition.nodes[loop.to].id)
		}
	}
	for _, i := range set {
		node := definition.nodes[i]
		if command, out := commandForNode(*next, node.id); out && command.Kind == CommandProgram {
			return fmt.Errorf("%w: a program is running at %q, which cannot be called back; wait for it or cancel the run",
				ErrStaleResolution, node.id)
		}
		if i == index {
			continue
		}
		for _, edgeIndex := range node.incoming {
			edge := definition.edges[edgeIndex]
			if !in[edge.fromIndex] && edge.loop < 0 && next.Edges[edge.from][edge.outcome] != EdgeUnresolved &&
				next.Edges[edge.from][edge.outcome] != EdgeNotTaken {
				return fmt.Errorf("%w: a branch outside the send-back already reached %q", ErrStaleResolution, node.id)
			}
		}
	}

	for _, i := range set {
		node := definition.nodes[i]
		removeCommand(next, node.id)
		removeObligation(next, node.id)
		removeBlocked(next, node.id)
		next.Nodes[node.id] = NodePending
		_, approval := approvalAnchor(definition, node)
		switch {
		case node.awaitsHuman() && !approval:
			// A person's decision or vote opens a new window, so a verdict
			// formed before the send-back names the old one and is refused.
			// (A plan-approval gate's window is its plan's next attempt.)
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
	// A loop wholly downstream counts its rounds again.
	for _, loop := range definition.loops {
		if in[loop.from] && in[loop.to] {
			source := definition.nodes[loop.from].id
			delete(next.Iterations[source], loop.outcome)
			if len(next.Iterations[source]) == 0 {
				delete(next.Iterations, source)
			}
		}
	}
	next.Nodes[target.id] = NodeReady
	if target.awaitsHuman() {
		addObligation(next, target.id)
	}
	return nil
}
