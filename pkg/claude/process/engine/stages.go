package engine

import (
	"fmt"
	"math"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// The compound rework loop: what a failed program-backed check or review stage
// does to the compound it belongs to.
//
// The whole slice rests on one product rule. A compound task's authored
// retry.maxAttempts is its ONLY rework budget, it lives on the prepared do
// child, and it bounds how many times that do stage executes — whether an
// execution was spent by the do program failing or by a gate sending the work
// back. There is deliberately no per-gate budget, no restarted window at a
// later gate, and nothing durable about any of this: a gate's ceiling is
// derived from its do anchor on the spot, and the reset is a move over one
// parent's ordered child list.

// gateAnchor returns the do stage a prepared gate renders its verdict over, and
// reports false for every node that is not such a gate.
//
// A gate is a test or review child of a compound, because those are the stages
// whose non-success is a statement about the do stage's work rather than a
// failure of their own. The plan stage is deliberately NOT one — a failed plan
// program stays fail-fast in this slice — and neither is the do stage itself,
// which retries in place out of the same budget like any ordinary task.
func gateAnchor(definition *Definition, node definitionNode) (definitionNode, bool) {
	if node.parent < 0 || (node.stage != model.StageTest && node.stage != model.StageReview) {
		return definitionNode{}, false
	}
	parent := definition.nodes[node.parent]
	if parent.doAnchor < 0 {
		return definitionNode{}, false
	}
	return definition.nodes[parent.doAnchor], true
}

// The human approval gate: what a person deciding about a compound's PLAN does
// to that compound.
//
// It is the same local shape as the program gate above, with the budget rule
// inverted. A program gate spends the compound's one authored budget, because
// nothing about a failing check asked a person for anything. A human approval
// gate has no budget at all: every reopened window costs exactly one explicit,
// audited human rework, which is already the throttle a budget would have been.
// So there is nothing durable here either — the window's identity is the plan
// child's own attempt counter, and the reset is a two-node move inside one
// parent.

// The fixed verdicts of a prepared plan-approval gate. A compound's stages carry
// no authored edges, so unlike an authored decision there is no outcome
// vocabulary for an author to have written: approve completes the gate and
// readies do, rework returns the plan to ready and reopens the window later.
const (
	ApprovalApprove = "approve"
	ApprovalRework  = "rework"
)

// approvalVerdicts is the gate's prepared verdict vocabulary, in the same sorted
// order Prepare gives an authored decision's outcomes.
func approvalVerdicts() []string { return []string{ApprovalApprove, ApprovalRework} }

// approvalAnchor returns the plan stage a prepared approval gate renders its
// verdict over, and reports false for every node that is not such a gate.
//
// It is deliberately a SIBLING of gateAnchor rather than a case inside it. The
// two anchors answer the same question — which stage is this gate's work — but
// everything that reads gateAnchor is about the do-stage budget and the blocked
// disposition, neither of which an approval gate has. Keeping them apart is what
// stops an approval gate from silently acquiring a do budget or the ability to
// park on an operator.
func approvalAnchor(definition *Definition, node definitionNode) (definitionNode, bool) {
	if node.parent < 0 || node.stage != model.StagePlanApproval {
		return definitionNode{}, false
	}
	parent := definition.nodes[node.parent]
	if parent.planAnchor < 0 {
		return definitionNode{}, false
	}
	return definition.nodes[parent.planAnchor], true
}

// verdictAnchor returns the stage whose work a prepared gate's verdict is ABOUT:
// the do stage for a program-backed check or review, and the plan stage for a
// human approval gate. It exists for the one caller that genuinely means "the
// work this verdict sent back" regardless of which kind of gate rendered it —
// deriving StageReset evidence — and it changes neither anchor's own semantics.
func verdictAnchor(definition *Definition, node definitionNode) (definitionNode, bool) {
	if anchor, ok := gateAnchor(definition, node); ok {
		return anchor, true
	}
	return approvalAnchor(definition, node)
}

// approvalGatedPlan reports whether a prepared node is the plan stage of a
// compound that also expanded a human approval gate. It is the exact mirror of
// approvalAnchor — same pair of prepared anchors, read from the other end — and
// like it costs one parent lookup rather than a walk of the child list.
func approvalGatedPlan(definition *Definition, node definitionNode) bool {
	return node.parent >= 0 && node.stage == model.StagePlan &&
		definition.nodes[node.parent].approvalGate >= 0
}

// executableAttemptCeiling is how many attempts a node may actually have, and
// the one derivation every rule that bounds an attempt goes through: planning's
// budget guard and the load boundary's attempt bound both read it, so they
// cannot drift apart.
//
// For an ordinary task it is the node's own authored-or-raised ceiling. For a
// compound gate it is DERIVED from the do anchor's current ceiling and stored
// nowhere: a gate runs at most once per do execution, so the compound's single
// budget already bounds it, and writing an AttemptCeilings entry for a gate
// would be a second copy of a fact the do anchor already holds.
//
// A human-approved plan stage is the third case, and it is derived too: its
// ceiling is one above whatever it has already run, because every attempt past
// the first is bought by one explicit human rework. What actually bounds those
// runs is the STATUS of the plan stage — the reset that readies it again is the
// only thing a rework does, and nothing else in the run can make a done plan
// ready — so a durable ceiling entry here would be an invented lifetime limit on
// an audited human action rather than a safety property. The ProgramFailed
// disposition deliberately does not read this: a failed plan program is still
// fail-fast against its AUTHORED ceiling.
func executableAttemptCeiling(checkpoint Checkpoint, definition *Definition, node definitionNode) int {
	if anchor, ok := gateAnchor(definition, node); ok {
		return attemptCeiling(checkpoint, anchor)
	}
	if approvalGatedPlan(definition, node) {
		return saturatingNextAttempt(checkpoint, node.id)
	}
	return attemptCeiling(checkpoint, node)
}

// saturatingNextAttempt is "one more than this node has already run", clamped at
// the last representable attempt instead of wrapping.
//
// It is used where the answer is a BOUND or a description — the approval-gated
// plan's derived ceiling, and the attempt a reset says the work will next carry.
// Neither may go negative: a wrapped ceiling would make the load boundary reject
// a checkpoint whose own counter it had just accepted and would let the planning
// guard wave a negative attempt through, and wrapped evidence would claim a run
// is about to execute an attempt that cannot exist.
//
// It is deliberately NOT what mints an attempt. nextAttempt still adds without
// clamping, because saturating there would hand out the same attempt number
// twice; the planning transition refuses the wrap instead. Neither is a lifetime
// budget — no reachable run comes near this, and the analogous arithmetic in
// raiseAttemptCeiling has been guarded all along.
func saturatingNextAttempt(checkpoint Checkpoint, nodeID string) int {
	if attempts := checkpoint.Attempts[nodeID]; attempts < math.MaxInt {
		return attempts + 1
	}
	return math.MaxInt
}

// blockableTask reports whether a prepared task could ever have parked on an
// operator: it has an authored retry policy of its own, or it is a compound
// gate whose do anchor has one. A gate spends that anchor's budget rather than
// a budget of its own, so exhausting it is exactly the case an author who asked
// for retries wanted a person to look at.
func blockableTask(definition *Definition, node definitionNode) bool {
	if node.retryAuthored {
		return true
	}
	anchor, ok := gateAnchor(definition, node)
	return ok && anchor.retryAuthored
}

// resetCompoundStages is the local rework rule, and the ONE place a completed
// stage moves backwards.
//
// It returns the do stage to ready and returns every child strictly after it
// through the failed gate to pending, in the same reducer step. Passed
// intervening gates are among them on purpose: they rendered their verdict over
// work that is about to be replaced, so they have to run again.
//
// Everything it does is bounded by ONE parent's ordered child list. No edge is
// settled — a compound's stages have none — no attempt counter is touched, the
// parent stays running, and no node outside this parent is read or written, so
// two compounds reworking side by side cannot see each other. Stages after the
// gate are already pending and are deliberately left alone.
func resetCompoundStages(next *Checkpoint, definition *Definition, gateIndex int) error {
	gate := definition.nodes[gateIndex]
	parent := definition.nodes[gate.parent]
	if status := next.Nodes[parent.id]; status != NodeRunning {
		return fmt.Errorf("%w: gate %q requires a running compound parent; got %q",
			ErrInvalidTransition, gate.id, status)
	}
	work := definition.nodes[parent.doAnchor]
	if status := next.Nodes[work.id]; status != NodeDone {
		return fmt.Errorf("%w: compound %q cannot re-run stage %q from %q",
			ErrInvalidTransition, parent.id, work.id, status)
	}
	first := slices.Index(parent.children, parent.doAnchor)
	last := slices.Index(parent.children, gateIndex)
	if first < 0 || last <= first {
		return fmt.Errorf("%w: gate %q does not follow the do stage of compound %q",
			ErrInvalidTransition, gate.id, parent.id)
	}
	next.Nodes[work.id] = NodeReady
	for _, childIndex := range parent.children[first+1 : last+1] {
		next.Nodes[definition.nodes[childIndex].id] = NodePending
	}
	return nil
}

// resetPlanApproval is what a human rework verdict does, and the ONLY other
// place a completed stage moves backwards.
//
// It is deliberately much smaller than resetCompoundStages, because a rework
// verdict says less: the plan is to be redone, and the approval will be asked
// again for whatever plan comes back. Every stage after the approval is still
// pending — nothing downstream of an unapproved plan has run — so there is
// nothing to sweep, and touching them would be reaching past what the human
// said. Exactly two nodes move: the plan returns to ready so ordinary planning
// mints its next attempt, and the gate returns to pending so the next plan
// success reopens the window with a fresh obligation.
//
// No edge is settled, no attempt counter is touched, no ceiling is written, the
// parent stays running, and nothing outside this parent is read or written, so
// two compounds awaiting approval side by side cannot see each other.
func resetPlanApproval(next *Checkpoint, definition *Definition, gateIndex int) error {
	gate := definition.nodes[gateIndex]
	plan, ok := approvalAnchor(definition, gate)
	if !ok {
		return fmt.Errorf("%w: node %q is not a compound plan-approval gate", ErrInvalidTransition, gate.id)
	}
	parent := definition.nodes[gate.parent]
	if status := next.Nodes[parent.id]; status != NodeRunning {
		return fmt.Errorf("%w: gate %q requires a running compound parent; got %q",
			ErrInvalidTransition, gate.id, status)
	}
	if status := next.Nodes[plan.id]; status != NodeDone {
		return fmt.Errorf("%w: compound %q cannot re-run stage %q from %q",
			ErrInvalidTransition, parent.id, plan.id, status)
	}
	if status := next.Nodes[gate.id]; status != NodeReady {
		return fmt.Errorf("%w: gate %q cannot rework from %q", ErrInvalidTransition, gate.id, status)
	}
	// The last representable window can still be APPROVED — approving asks for no
	// further plan run — but reworking it would ready a plan whose next attempt
	// cannot be minted, wedging the branch with work nothing can plan. Refused
	// here, before anything moves, so the window simply stays open.
	if next.Attempts[plan.id] >= math.MaxInt {
		return fmt.Errorf("%w: compound %q cannot re-run stage %q past attempt %d",
			ErrInvalidTransition, parent.id, plan.id, math.MaxInt)
	}
	next.Nodes[plan.id] = NodeReady
	next.Nodes[gate.id] = NodePending
	return nil
}

// retryBlockedGate is the ONE place an operator resolution raises the attempt
// ceiling of a node OTHER than the one being resolved. It is deliberately its
// own named helper so it can never be mistaken for the ordinary blocked-task
// retry beside it, which raises the resolved node's own ceiling.
//
// The asymmetry is the single-budget rule showing through. A parked gate has no
// budget to open: the compound's one budget lives on its do stage, and what the
// operator is asking for by retrying a failed check or review is another pass at
// the WORK. So the fresh authored-size window lands on the do anchor, and the
// same local reset a failed-but-affordable gate would have performed runs here.
func retryBlockedGate(next *Checkpoint, definition *Definition, gateIndex int) error {
	gate := definition.nodes[gateIndex]
	anchor, ok := gateAnchor(definition, gate)
	if !ok {
		return fmt.Errorf("%w: node %q is not a compound check or review gate", ErrInvalidTransition, gate.id)
	}
	if err := raiseAttemptCeiling(next, anchor); err != nil {
		return err
	}
	return resetCompoundStages(next, definition, gateIndex)
}

// StageReset is one compound rework a committed transition performed: which
// parent it happened inside, which gate asked for it, which work stage runs
// again, and the attempt that next run will carry.
type StageReset struct {
	ParentNodeID    string
	GateNodeID      string
	WorkNodeID      string
	NextWorkAttempt int
}

// StageReset reports whether the transition that named nodeID reworked a
// compound, and what that rework was. It is how a caller derives human-facing
// evidence from a committed transition without the reducer having to emit
// anything — the same before/after seam JoinArrivals and the executor's blocked
// evidence already use.
//
// It is a TARGETED lookup rather than a scan, and that is the point. At most
// one reset can happen per transition, and every transition that can cause one
// already NAMES the gate: an observation and an operator-recorded outcome name
// their command's node, a resolution names the parked node, and a decision names
// the node it rendered a verdict for. So the caller
// passes that node, this reads only that node's own prepared parent, and a
// commit that reworked nothing pays a single map lookup. Nothing ever walks
// another compound; the one walk here is bounded by the named gate's OWN
// parent's child list, for the fail-closed reason below.
//
// It is fail-closed in both directions: the named node must be a prepared gate —
// a check or review over the do stage, or a human approval over the plan stage —
// and the before/after delta must demonstrate THAT gate's reset: the gate
// returned to pending, which nothing but a reset ever does, inside a compound
// whose work stage had actually completed, and with no LATER stage of the same
// compound reset alongside it. That last condition is what keeps the recorded
// gate the one that actually rendered the verdict: a program-gate reset sweeps
// every stage from just after the work through the failed gate back to pending,
// so an intervening gate that merely got caught up in it satisfies everything
// else while not being the cause. An approval rework moves only its own two
// nodes, so it satisfies the condition trivially.
//
// The next work attempt is read from the BEFORE checkpoint deliberately. One
// commit can hold the observation, the reset, and the follow-on plan the reset
// made possible, so the after-state's counter may already have advanced to that
// very attempt; before-plus-one names it either way.
//
// It is never read back: the checkpoint stays authoritative and evidence is
// never replayed.
func (d *Definition) StageReset(nodeID string, before, after Checkpoint) (StageReset, bool) {
	if d == nil {
		return StageReset{}, false
	}
	gateIndex, ok := d.index[nodeID]
	if !ok {
		return StageReset{}, false
	}
	gate := d.nodes[gateIndex]
	work, ok := verdictAnchor(d, gate)
	if !ok {
		return StageReset{}, false
	}
	if !returnedToPending(before, after, gate.id) || before.Nodes[work.id] != NodeDone {
		return StageReset{}, false
	}
	parent := d.nodes[gate.parent]
	position := slices.Index(parent.children, gateIndex)
	if position < 0 {
		return StageReset{}, false
	}
	for _, childIndex := range parent.children[position+1:] {
		if returnedToPending(before, after, d.nodes[childIndex].id) {
			return StageReset{}, false
		}
	}
	return StageReset{
		ParentNodeID: parent.id, GateNodeID: gate.id, WorkNodeID: work.id,
		NextWorkAttempt: saturatingNextAttempt(before, work.id),
	}, true
}

// returnedToPending reports the one status move only a compound reset makes.
// Every other transition moves a node forwards.
func returnedToPending(before, after Checkpoint, nodeID string) bool {
	return after.Nodes[nodeID] == NodePending && before.Nodes[nodeID] != NodePending
}
