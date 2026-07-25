package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// Definition is the immutable executable projection of one pinned template
// and its immutable run parameters. Its fields stay private so preparation is
// the only way to construct executable state and callers cannot mutate it.
// Nodes are held in a deterministic topological order with the template's
// entry node first; an explicit start-typed node is optional.
type Definition struct {
	nodes []definitionNode
	index map[string]int
	edges []definitionEdge
	// joinAnyIncoming is the prepared index of every inbound edge of a join: any
	// reducer, in deterministic order. It exists so deriving join evidence is a
	// walk of exactly the edges that can carry it rather than of the graph: a
	// definition with no join: any node has an empty index and pays nothing.
	joinAnyIncoming []int
}

type definitionNodeKind uint8

const (
	definitionStart definitionNodeKind = iota + 1
	definitionTask
	definitionEnd
	definitionDecision
	definitionParallel
	// definitionCompound is an authored task node that declares plan, check, or
	// review stages. It runs no program of its own: preparation expanded it into
	// ordinary prepared stage children, and the parent exists to hold the
	// authored edges and the ordered child list.
	definitionCompound
	// definitionStageDone is the engine-owned final stage of a compound
	// expansion. Advancing it is what completes the parent and settles the
	// parent's authored outgoing route, exactly once.
	definitionStageDone
)

type definitionNode struct {
	id      string
	kind    definitionNodeKind
	program ProgramCommand // task nodes only
	// maxAttempts is the authored retry budget of a task node, first attempt
	// included, and 1 for a task with no authored retry. It is the ONLY thing
	// preparation carries about retry: the reducer needs a ceiling to compare
	// the checkpoint's attempt counter against, and nothing else about the
	// authored policy is executable in this slice.
	maxAttempts int
	// retryAuthored records whether the author asked for retries AT ALL, which
	// maxAttempts alone cannot say: an explicit maxAttempts: 1 and no policy at
	// all both resolve to a budget of one. It is what decides the exhaustion
	// disposition — an explicit policy parks the branch for an operator, no
	// policy stays fail-fast — so the two shapes have to stay distinguishable.
	retryAuthored bool
	terminal      RunStatus // end nodes only
	verdicts      []string  // decision nodes only: sorted authored outcome labels
	outgoing      []int     // edge indices, sorted by outcome label
	incoming      []int     // edge indices, in edge order
	// joinAll marks a convergence node authored with join: all. Such a node
	// activates once its complete candidate input set has settled with one or
	// more arrivals, instead of the non-join exactly-one-arrival rule.
	joinAll bool
	// joinAny marks a convergence node authored with join: any: it activates on
	// its FIRST arrival without waiting for the rest of its candidate set. It is
	// the one node kind whose incoming edges can still settle after it activated,
	// and the only one whose edges may hold EdgeArrivedLate.
	joinAny bool
	// children are the prepared indices of a compound parent's derived stages, in
	// the order model.ExpandNode returned them. It is the ONLY thing that
	// sequences a compound: the stages carry no prepared or durable edges, so
	// advancing one is a step along this list rather than a second graph.
	children []int
	// parent is the prepared index of the compound parent whose expansion
	// produced this node, or -1 for an authored node.
	parent int
	// stage is the derived stage kind of a compound child, and the empty kind for
	// an authored node. It is the minimum private identity the rework rules need:
	// which child is the work, and which are the gates that render a verdict over
	// it. Nothing durable ever names it — expansion stays a pure projection of the
	// pinned template, re-derived identically on every cold load.
	stage model.StageKind
	// doAnchor is a compound parent's prepared do-child index, and -1 everywhere
	// else. It is what makes "this gate's work stage" an O(1) lookup instead of a
	// second scan of the child list, and it is the single node the compound's one
	// rework budget lives on.
	doAnchor int
	// planAnchor is a compound parent's prepared plan-child index, and -1
	// everywhere else — including on a compound that authored no plan stage. It
	// is the doAnchor of the approval gate: the minimum private anchor that makes
	// "the work this approval renders its verdict over" an O(1) lookup, and the
	// node whose attempt counter IS the approval window's identity.
	planAnchor int
	// approvalGate is a compound parent's prepared plan-approval-gate child
	// index, and -1 everywhere else — including on a compound whose plan needs no
	// human approval. It is the other direction of the same pair: planAnchor
	// answers "which work is this gate about", and this answers "is this plan
	// gated by a person", which is what the plan-stage ceiling rule turns on. Both
	// are prepared once so neither question is ever a walk of the child list.
	approvalGate int
}

type definitionEdge struct {
	from      string
	outcome   string
	to        string
	fromIndex int
	toIndex   int
}

// Prepare performs all immutable work once: complete authoring validation,
// engine eligibility, deterministic topological ordering, compound stage
// expansion, edge identity derivation, decision verdict binding, parameter
// binding, and final bound-program validation. Plan, Apply, and Advance reuse
// the result.
//
// Compound expansion happens HERE and nowhere else. It is a pure projection of
// the pinned template, so the one-time prepared definition is exactly where it
// belongs: a run records no expansion of its own, and a cold load re-derives
// the identical stages from the same immutable snapshot.
func Prepare(tmpl *model.Template, params map[string]string) (*Definition, error) {
	if err := RequireEligible(tmpl); err != nil {
		return nil, err
	}
	order, err := topologicalOrder(tmpl)
	if err != nil {
		return nil, err
	}
	definition := &Definition{
		nodes: make([]definitionNode, 0, len(order)),
		index: make(map[string]int, len(order)),
	}
	// appendNode is the single insertion point, so the id->index map and the
	// prepared order cannot disagree. Authoring rejects every child-id collision
	// before this runs (model.validateExpansionCollisions); the guard is here so
	// a caller reaching preparation another way fails closed instead of silently
	// aliasing two nodes onto one index.
	appendNode := func(prepared definitionNode) (int, error) {
		if _, exists := definition.index[prepared.id]; exists {
			return 0, fmt.Errorf("%w: node id %q is prepared twice", ErrInvalidDefinition, prepared.id)
		}
		index := len(definition.nodes)
		definition.index[prepared.id] = index
		definition.nodes = append(definition.nodes, prepared)
		return index, nil
	}
	for _, nodeID := range order {
		node := tmpl.Nodes[nodeID]
		prepared := definitionNode{
			id:           nodeID,
			parent:       -1,
			doAnchor:     -1,
			planAnchor:   -1,
			approvalGate: -1,
			joinAll:      node.Join == model.JoinAll,
			joinAny:      node.Join == model.JoinAny,
		}
		switch node.Type {
		case model.NodeTypeStart:
			prepared.kind = definitionStart
		case model.NodeTypeParallel:
			prepared.kind = definitionParallel
		case model.NodeTypeTask:
			if node.IsCompound() {
				// The parent runs no program: its do work is one of the derived
				// stages below. It keeps the authored edges and the child list.
				prepared.kind = definitionCompound
				break
			}
			prepared.kind = definitionTask
			// Eligibility already admitted only the retry shape this engine
			// executes, so the budget is taken as authored; RetryBudget resolves an
			// absent policy to the fail-fast single attempt.
			prepared.maxAttempts = model.RetryBudget(node.Retry)
			prepared.retryAuthored = node.Retry != nil
			program, err := bindProgram(nodeID, *node.Performer, params)
			if err != nil {
				return nil, err
			}
			prepared.program = program
		case model.NodeTypeDecision:
			prepared.kind = definitionDecision
			prepared.verdicts = sortedOutcomes(node.Next)
		case model.NodeTypeEnd:
			prepared.kind = definitionEnd
			prepared.terminal = terminalStatus(node.Result)
		}
		parentIndex, err := appendNode(prepared)
		if err != nil {
			return nil, err
		}
		if prepared.kind != definitionCompound {
			continue
		}
		// Derived stages are appended immediately after their parent, so the
		// prepared order stays a single deterministic sequence and a compound's
		// stages sit exactly where the parent did.
		for _, spec := range model.ExpandNode(nodeID, node) {
			// Every stage but the do stage is a single fail-fast attempt:
			// eligibility rejects stage retry and approvalRetry policies, so no
			// other child carries an authored budget.
			child := definitionNode{
				id: spec.ChildID, parent: parentIndex, doAnchor: -1, planAnchor: -1,
				approvalGate: -1, stage: spec.Stage, maxAttempts: 1,
			}
			switch spec.Stage {
			case model.StageDone:
				child.kind = definitionStageDone
			case model.StagePlanApproval:
				// The one prepared stage nobody dispatches. It is an ordinary
				// decision as far as the reducer, the durable obligation outbox, and
				// every decision surface are concerned; only its verdict vocabulary
				// is fixed rather than authored, because a compound's stages have no
				// authored edges for an author to have named outcomes on.
				//
				// bindProgram is deliberately never called for it: model.ExpandNode
				// gives it a synthetic human performer that describes who decides,
				// not a program to run.
				child.kind = definitionDecision
				child.verdicts = approvalVerdicts()
			default:
				child.kind = definitionTask
				program, err := bindProgram(spec.ChildID, *spec.Performer, params)
				if err != nil {
					return nil, err
				}
				child.program = program
			}
			// The compound's authored retry budget belongs to the do stage and to
			// nothing else. It is the SINGLE rework budget: it bounds do executions
			// whether a do program failed or a gate sent the work back, which is why
			// no gate is ever prepared with one of its own.
			if spec.Stage == model.StageDo {
				child.maxAttempts = model.RetryBudget(node.Retry)
				child.retryAuthored = node.Retry != nil
			}
			childIndex, err := appendNode(child)
			if err != nil {
				return nil, err
			}
			definition.nodes[parentIndex].children = append(definition.nodes[parentIndex].children, childIndex)
			switch spec.Stage {
			case model.StageDo:
				definition.nodes[parentIndex].doAnchor = childIndex
			case model.StagePlan:
				definition.nodes[parentIndex].planAnchor = childIndex
			case model.StagePlanApproval:
				definition.nodes[parentIndex].approvalGate = childIndex
			}
		}
		if definition.nodes[parentIndex].doAnchor < 0 {
			return nil, fmt.Errorf("%w: compound %q expanded without a do stage", ErrInvalidDefinition, nodeID)
		}
		// An approval gate whose plan child is missing would be a decision nothing
		// could ever derive a window identity for. Expansion only ever emits the
		// gate directly after the plan stage, so this fails closed on a caller that
		// reached preparation with an expansion this engine did not derive.
		if parent := definition.nodes[parentIndex]; parent.approvalGate >= 0 && parent.planAnchor < 0 {
			return nil, fmt.Errorf("%w: compound %q expanded an approval gate without a plan stage",
				ErrInvalidDefinition, nodeID)
		}
	}
	for fromIndex := range definition.nodes {
		from := &definition.nodes[fromIndex]
		// Derived stages have no authored edges, and the engine gives them no
		// synthetic ones: their sequence is the parent's ordered child list.
		if from.parent >= 0 {
			continue
		}
		next := tmpl.Nodes[from.id].Next
		for _, outcome := range sortedOutcomes(next) {
			toIndex, ok := definition.index[next[outcome]]
			if !ok {
				return nil, fmt.Errorf("%w: edge %q/%q targets unknown node", ErrInvalidDefinition, from.id, outcome)
			}
			edgeIndex := len(definition.edges)
			definition.edges = append(definition.edges, definitionEdge{
				from: from.id, outcome: outcome, to: next[outcome],
				fromIndex: fromIndex, toIndex: toIndex,
			})
			from.outgoing = append(from.outgoing, edgeIndex)
		}
	}
	for edgeIndex, edge := range definition.edges {
		to := &definition.nodes[edge.toIndex]
		to.incoming = append(to.incoming, edgeIndex)
		if to.joinAny {
			definition.joinAnyIncoming = append(definition.joinAnyIncoming, edgeIndex)
		}
	}
	return definition, nil
}

// topologicalOrder returns every node exactly once, the entry node first, in a
// deterministic order that always places edge sources before their targets.
// Eligibility guarantees acyclicity, so a leftover node fails closed as an
// invalid definition rather than looping. It is also where the sole-structural-
// entry requirement fails closed: the template's start must be the graph's only
// source node, whatever kind it is.
func topologicalOrder(tmpl *model.Template) ([]string, error) {
	remaining := make(map[string]int, len(tmpl.Nodes))
	for nodeID := range tmpl.Nodes {
		remaining[nodeID] = 0
	}
	for _, node := range tmpl.Nodes {
		for _, target := range node.Next {
			if _, ok := remaining[target]; ok {
				remaining[target]++
			}
		}
	}
	candidates := []string{}
	for nodeID, count := range remaining {
		if count == 0 {
			candidates = append(candidates, nodeID)
		}
	}
	sort.Strings(candidates)
	if len(candidates) != 1 || candidates[0] != tmpl.Start {
		return nil, fmt.Errorf("%w: start must be the only entry node", ErrInvalidDefinition)
	}
	order := make([]string, 0, len(tmpl.Nodes))
	for len(candidates) > 0 {
		current := candidates[0]
		candidates = candidates[1:]
		order = append(order, current)
		released := []string{}
		for _, outcome := range sortedOutcomes(tmpl.Nodes[current].Next) {
			target := tmpl.Nodes[current].Next[outcome]
			remaining[target]--
			if remaining[target] == 0 {
				released = append(released, target)
			}
		}
		sort.Strings(released)
		candidates = mergeSorted(candidates, released)
	}
	if len(order) != len(tmpl.Nodes) {
		return nil, fmt.Errorf("%w: graph contains a cycle", ErrInvalidDefinition)
	}
	return order, nil
}

func mergeSorted(left, right []string) []string {
	if len(right) == 0 {
		return left
	}
	merged := make([]string, 0, len(left)+len(right))
	i, j := 0, 0
	for i < len(left) && j < len(right) {
		if left[i] <= right[j] {
			merged = append(merged, left[i])
			i++
		} else {
			merged = append(merged, right[j])
			j++
		}
	}
	merged = append(merged, left[i:]...)
	merged = append(merged, right[j:]...)
	return merged
}

func sortedOutcomes(next model.Next) []string {
	outcomes := make([]string, 0, len(next))
	for outcome := range next {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	return outcomes
}

// DecisionVerdicts returns the authored verdict vocabulary of one prepared
// decision node. Views derive selectable verdicts from here so the durable
// checkpoint never copies definition state.
func (d *Definition) DecisionVerdicts(nodeID string) ([]string, bool) {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok || node.kind != definitionDecision {
		return nil, false
	}
	return append([]string(nil), node.verdicts...), true
}

// ApprovalAttempt reports the plan attempt one prepared plan-approval gate's
// CURRENT window is bound to, and false for every node that is not such a gate
// — including an ordinary authored decision, which opens exactly once.
//
// It is the single derivation of the approval half of decision identity, shared
// by the reducer's staleness check, the executor's evidence, and the API view,
// so those three cannot drift apart. The number is read from the plan child's
// own monotonic counter, which is why nothing durable stores it: a window is
// exactly "the approval of plan attempt N", and the counter is what already
// says which N that is.
func (d *Definition) ApprovalAttempt(checkpoint Checkpoint, nodeID string) (int, bool) {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok {
		return 0, false
	}
	plan, ok := approvalAnchor(d, node)
	if !ok {
		return 0, false
	}
	return checkpoint.Attempts[plan.id], true
}

// RequiredDecisionAttempt is the exact attempt a verdict for one prepared
// decision has to name, and it is deterministic from the prepared node: an
// authored one-shot decision requires zero, so every caller that predates the
// field keeps working, and a recurring plan-approval gate requires its current
// window's plan attempt.
func (d *Definition) RequiredDecisionAttempt(checkpoint Checkpoint, nodeID string) int {
	if attempt, ok := d.ApprovalAttempt(checkpoint, nodeID); ok {
		return attempt
	}
	return 0
}

// DecisionEdge returns the structural authored edge one verdict selects.
func (d *Definition) DecisionEdge(nodeID, verdict string) (ChosenEdge, bool) {
	if d == nil {
		return ChosenEdge{}, false
	}
	index, ok := d.index[nodeID]
	if !ok || d.nodes[index].kind != definitionDecision {
		return ChosenEdge{}, false
	}
	for _, edgeIndex := range d.nodes[index].outgoing {
		edge := d.edges[edgeIndex]
		if edge.outcome == verdict {
			return ChosenEdge{From: edge.from, Outcome: edge.outcome, To: edge.to}, true
		}
	}
	return ChosenEdge{}, false
}

// JoinArrival is one arrival at a join: any reducer, as evidence: which join,
// which authored edge got there, and whether it was the one that won.
type JoinArrival struct {
	JoinNodeID string
	From       string
	Outcome    string
	Winner     bool
}

// JoinArrivals reports the join: any arrivals that settled between two
// checkpoints of the same run, in deterministic prepared order. It is how a
// caller derives human-facing evidence from a committed transition without the
// reducer having to emit anything — the same shape the executor already uses to
// derive a newly parked decision from before/after checkpoints.
//
// It is deliberately NOT a graph diff. It walks the prepared joinAnyIncoming
// index — exactly the edges that can carry a join arrival — so the cost is the
// inbound degree of the template's join: any reducers, and a definition without
// one returns on the first line. It runs once per durable commit, not per
// transition, and reads two map lookups per indexed edge.
func (d *Definition) JoinArrivals(before, after Checkpoint) []JoinArrival {
	if d == nil || len(d.joinAnyIncoming) == 0 {
		return nil
	}
	var arrivals []JoinArrival
	for _, edgeIndex := range d.joinAnyIncoming {
		edge := d.edges[edgeIndex]
		if before.Edges[edge.from][edge.outcome] != EdgeUnresolved {
			continue
		}
		switch after.Edges[edge.from][edge.outcome] {
		case EdgeArrived:
			arrivals = append(arrivals, JoinArrival{
				JoinNodeID: edge.to, From: edge.from, Outcome: edge.outcome, Winner: true})
		case EdgeArrivedLate:
			arrivals = append(arrivals, JoinArrival{
				JoinNodeID: edge.to, From: edge.from, Outcome: edge.outcome})
		}
	}
	return arrivals
}

func bindProgram(nodeID string, performer model.Performer, params map[string]string) (ProgramCommand, error) {
	for _, reference := range model.ParamReferences(performer.Run) {
		value, ok := params[reference]
		if !ok {
			return ProgramCommand{}, fmt.Errorf("%w: node %q run parameter %q is missing", ErrInvalidProgramBinding, nodeID, reference)
		}
		if strings.TrimSpace(value) == "" {
			return ProgramCommand{}, fmt.Errorf("%w: node %q run parameter %q is blank", ErrInvalidProgramBinding, nodeID, reference)
		}
	}
	for index, arg := range performer.Args {
		for _, reference := range model.ParamReferences(arg) {
			if _, ok := params[reference]; !ok {
				return ProgramCommand{}, fmt.Errorf("%w: node %q argument %d parameter %q is missing", ErrInvalidProgramBinding, nodeID, index, reference)
			}
		}
	}
	bound := model.InterpolatePerformer(performer, params)
	if strings.TrimSpace(bound.Run) == "" {
		return ProgramCommand{}, fmt.Errorf("%w: node %q run is blank after interpolation", ErrInvalidProgramBinding, nodeID)
	}
	return ProgramCommand{
		Profile: bound.Profile,
		Run:     bound.Run,
		Args:    append([]string(nil), bound.Args...),
		Timeout: bound.Timeout,
	}, nil
}

// DecodeCheckpoint is the persistence/load boundary: strict JSON shape
// decoding is followed by semantic validation against the prepared definition.
// Pure in-memory engine cycles operate on the typed Checkpoint instead.
func DecodeCheckpoint(data []byte, definition *Definition) (Checkpoint, error) {
	var checkpoint Checkpoint
	if err := strictjson.Decode(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: decode: %v", ErrInvalidCheckpoint, err)
	}
	if err := ValidateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// ValidateCheckpoint is the boundary validator, run ONLY at the trusted load
// (DecodeCheckpoint) and creation (Initialize) boundaries — never inside the
// per-transition runtime cycle. Plan/Apply/Advance deliberately do not call it,
// so the O(nodes + edges) whole-checkpoint scan happens once when untrusted
// bytes enter, not on every transition. It enforces concrete structural safety
// against an immutable prepared definition: checkpoint version and bounded run
// id, exact known node and edge references, known status/disposition enum
// values, attempt counters inside the node's current budget, and unique bounded
// command/decision/blocked obligation entries whose deterministic identity,
// attempt, and node binding are compatible with a known node. It
// deliberately does NOT reconstruct whether stored values are ones the reducer
// could have produced: the private checkpoint is authoritative and every
// supported write path preserves those invariants, so tamper detection on an
// ordinary load is a deferred product decision rather than a gap here. It also
// deliberately does NOT run the whole-graph exclusive-decision classification
// (single active node, single failure, pending-arrival exclusion, activation
// proofs): those are current-slice expectations demoted to ClassifyCheckpoint
// so Processes can load and persist structurally safe parallel-ready state
// before parallelism semantics settle. It never re-runs static template or
// graph-shape validation, which happened once in Prepare.
func ValidateCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	return validateCheckpoint(checkpoint, definition)
}

// ClassifyCheckpoint is the exhaustive, deliberately strict SEQUENTIAL-slice
// invariant proof. It runs the runtime structural validator and then the
// whole-graph exclusive-decision classification the runtime hot path never
// performs. Sequential and exclusive-decision tests call it after every
// transition so that behavior keeps its full bug-finding coverage.
//
// It describes a single-token run: one active node, one outbox entry, one
// failure. Fan-out deliberately violates all three, so parallel templates are
// NOT classifiable and parallel tests assert their properties directly. It is a
// test/diagnostic entry point, not a hot path.
func ClassifyCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return err
	}
	return classifyCheckpoint(checkpoint, definition)
}

// edgeCounts aggregates the dispositions on one side of a node. arrivedLate is
// counted separately from arrived precisely because a late arrival settles its
// edge without being a candidate for anything: it can neither activate a target
// nor stand in for the winner.
type edgeCounts struct {
	unresolved  int
	arrived     int
	notTaken    int
	arrivedLate int
}

func validateCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	if definition == nil || len(definition.nodes) == 0 {
		return fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	if checkpoint.Version != CheckpointVersion {
		return invalid("version must be %d; got %d", CheckpointVersion, checkpoint.Version)
	}
	if !validRunID(checkpoint.RunID) {
		return invalid("runId must be a lowercase runtime identifier of at most 128 bytes")
	}
	switch checkpoint.Status {
	case RunRunning, RunCompleted, RunFailed, RunCanceled:
	default:
		return invalid("unknown run status %q", checkpoint.Status)
	}
	if len(checkpoint.Nodes) != len(definition.nodes) {
		return invalid("nodes must contain exactly the %d prepared nodes", len(definition.nodes))
	}
	for nodeID, status := range checkpoint.Nodes {
		if _, ok := definitionNodeByID(definition, nodeID); !ok {
			return invalid("nodes contains unknown node %q", nodeID)
		}
		switch status {
		case NodePending, NodeReady, NodeRunning, NodeDone, NodeFailed, NodeSkipped,
			NodeBlocked, NodeCanceled:
		default:
			return invalid("node %q has unknown status %q", nodeID, status)
		}
	}
	if err := validateEdgeShape(checkpoint, definition); err != nil {
		return err
	}
	if err := validateAttemptCeilings(checkpoint, definition); err != nil {
		return err
	}
	if err := validateAttempts(checkpoint, definition); err != nil {
		return err
	}
	if err := validateCommands(checkpoint, definition); err != nil {
		return err
	}
	if err := validateAwaitingDecisions(checkpoint, definition); err != nil {
		return err
	}
	return validateBlocked(checkpoint, definition)
}

// validateAttempts is a narrow structural check on the sparse attempt counter.
// It enforces exactly two things: the entry names a KNOWN reference (a prepared
// program task), and the value is within the node's CURRENT budget — at least
// the first attempt, at most the checkpoint's own authored-or-raised ceiling.
// The map is keyed by node id, so its size is bounded by the prepared node set
// without a separate count check.
//
// It is explicitly NOT a claim that the number is one the reducer could have
// produced. The ceiling is what an operator retry durably raised, so the bound
// deliberately follows the checkpoint rather than the authored budget, and the
// checkpoint under ~/.tclaude/data is authoritative: reconstructing reachability
// here would be tamper detection on an ordinary load, which is deferred by
// explicit product decision.
//
// It also deliberately says nothing about which nodes SHOULD have an entry. The
// counter is sparse on purpose, so this stays a bound on the values rather than
// a whole-graph proof about statuses.
func validateAttempts(checkpoint Checkpoint, definition *Definition) error {
	for nodeID, attempt := range checkpoint.Attempts {
		node, ok := definitionNodeByID(definition, nodeID)
		if !ok || node.kind != definitionTask {
			return fmt.Errorf("%w: attempts names %q, which is not a prepared program task", ErrInvalidCheckpoint, nodeID)
		}
		// The ceiling, not the authored budget: an operator retry durably raised
		// it, and both this bound and planning's own guard read the same helper
		// so a legitimately retried run cannot fail to cold-load. For a compound
		// gate that helper derives the bound from the do anchor, because a gate
		// re-runs once per do execution and has no ceiling entry of its own.
		if ceiling := executableAttemptCeiling(checkpoint, definition, node); attempt < 1 || attempt > ceiling {
			return fmt.Errorf("%w: node %q attempt %d is outside its budget of 1..%d",
				ErrInvalidCheckpoint, nodeID, attempt, ceiling)
		}
	}
	return nil
}

// validateAttemptCeilings is a narrow structural check on the sparse ceiling
// override, and deliberately nothing more. It enforces exactly two things:
//
//   - the entry names a KNOWN reference — a prepared program task with an
//     authored retry policy, since only such a node can ever be blocked and
//     therefore only such a node can ever be retried into a raised ceiling;
//   - the value is CANONICAL — strictly above the authored budget, because an
//     absent entry already means the authored budget, so repeating it would
//     give one logical state two durable encodings.
//
// It is explicitly NOT a proof that the value is one the reducer produced, and
// there is no upper bound. The private checkpoint under ~/.tclaude/data is
// authoritative and every supported write path preserves the invariant, so
// reconstructing reachability here would be a tamper check on an ordinary load
// — deferred by explicit product decision — and any bound it could impose would
// be an invented lifetime limit on an audited operator retry rather than a
// safety property.
func validateAttemptCeilings(checkpoint Checkpoint, definition *Definition) error {
	for nodeID, ceiling := range checkpoint.AttemptCeilings {
		node, ok := definitionNodeByID(definition, nodeID)
		if !ok || node.kind != definitionTask || !node.retryAuthored {
			return fmt.Errorf("%w: attemptCeilings names %q, which is not a prepared program task with an authored retry policy",
				ErrInvalidCheckpoint, nodeID)
		}
		if ceiling <= node.maxAttempts {
			return fmt.Errorf("%w: node %q ceiling %d must be above its authored budget of %d",
				ErrInvalidCheckpoint, nodeID, ceiling, node.maxAttempts)
		}
	}
	return nil
}

// validateBlocked enforces the durable blocked outbox is unique, bounded, and
// bound to genuinely parked branches of a running run, in both directions.
//
// The reverse direction matters as much as the forward one: a node left blocked
// with no obligation would be a branch no operator surface can see and no
// resolution can name — a silently stranded run — so the count is checked too.
func validateBlocked(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	parked := 0
	for _, status := range checkpoint.Nodes {
		if status == NodeBlocked {
			parked++
		}
	}
	if len(checkpoint.Blocked) != parked {
		return invalid("blocked holds %d obligations for %d blocked nodes", len(checkpoint.Blocked), parked)
	}
	if len(checkpoint.Blocked) == 0 {
		return nil
	}
	if checkpoint.Status != RunRunning {
		return invalid("blocked obligations require a running run")
	}
	seen := make(map[string]struct{}, len(checkpoint.Blocked))
	for _, obligation := range checkpoint.Blocked {
		// A compound gate carries no authored policy of its own — the compound's
		// single budget lives on the do stage it gates — so the reference check
		// follows that anchor rather than demanding a policy the prepared shape
		// never puts there. It stays a stage-kind and parent scoped relaxation:
		// nothing about the parent's or the siblings' current state is proved.
		node, ok := definitionNodeByID(definition, obligation.NodeID)
		if !ok || node.kind != definitionTask || !blockableTask(definition, node) {
			return invalid("blocked node %q is not a prepared program task with an authored retry policy", obligation.NodeID)
		}
		if _, dup := seen[obligation.NodeID]; dup {
			return invalid("duplicate blocked obligation for node %q", obligation.NodeID)
		}
		seen[obligation.NodeID] = struct{}{}
		if checkpoint.Nodes[obligation.NodeID] != NodeBlocked {
			return invalid("blocked node %q must have blocked status", obligation.NodeID)
		}
		// A branch parks only after an attempt really ran, and that attempt is
		// the identity a resolution binds to, so a blocked node without one has
		// no exact identity to resolve against.
		if checkpoint.Attempts[obligation.NodeID] < 1 {
			return invalid("blocked node %q has no recorded attempt to resolve", obligation.NodeID)
		}
		if len(obligation.Reason) > MaxBlockedReasonBytes || !utf8.ValidString(obligation.Reason) {
			return invalid("blocked node %q reason must be at most %d bytes of UTF-8",
				obligation.NodeID, MaxBlockedReasonBytes)
		}
	}
	return nil
}

// validateCommands enforces the durable command outbox is unique, bounded, and
// deterministically bound to known running program tasks. The slice is
// parallel-ready but the count is bounded by the prepared node set and each
// entry must name a distinct running task carrying its exact deterministic
// request, so malformed, unknown, or duplicate commands fail closed at the
// load/persist boundary regardless of how many the current reducer produces.
func validateCommands(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	if len(checkpoint.Commands) > len(definition.nodes) {
		return invalid("commands must not exceed the %d prepared nodes", len(definition.nodes))
	}
	seen := make(map[string]struct{}, len(checkpoint.Commands))
	for i := range checkpoint.Commands {
		command := checkpoint.Commands[i]
		if command.Kind != CommandProgram {
			return invalid("command kind must be %q", CommandProgram)
		}
		node, ok := definitionNodeByID(definition, command.NodeID)
		if !ok || node.kind != definitionTask {
			return invalid("command node %q is not a program task", command.NodeID)
		}
		if _, dup := seen[command.NodeID]; dup {
			return invalid("duplicate command for node %q", command.NodeID)
		}
		seen[command.NodeID] = struct{}{}
		// The attempt is taken from the counter, not from the command, so a
		// durable outbox entry that names any attempt other than its node's
		// current one fails closed here — including one whose id and Attempt
		// field agree with each other but not with the run's own state.
		expected := programCommand(checkpoint.RunID, node, checkpoint.Attempts[command.NodeID])
		if !commandsEqual(command, expected) {
			return invalid("command does not match the deterministic bound request for node %q", command.NodeID)
		}
		if checkpoint.Nodes[command.NodeID] != NodeRunning {
			return invalid("command node %q must be running", command.NodeID)
		}
	}
	return nil
}

// validateAwaitingDecisions enforces the durable decision obligation outbox is
// unique, bounded, and bound to known ready decision nodes on a running run.
// Like validateCommands it is a structural boundary check that is agnostic to
// how many obligations the current reducer produces.
func validateAwaitingDecisions(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	if len(checkpoint.AwaitingDecisions) == 0 {
		return nil
	}
	if checkpoint.Status != RunRunning {
		return invalid("awaiting decisions require a running run")
	}
	if len(checkpoint.AwaitingDecisions) > len(definition.nodes) {
		return invalid("awaiting decisions must not exceed the %d prepared nodes", len(definition.nodes))
	}
	seen := make(map[string]struct{}, len(checkpoint.AwaitingDecisions))
	for _, obligation := range checkpoint.AwaitingDecisions {
		node, ok := definitionNodeByID(definition, obligation.NodeID)
		if !ok || node.kind != definitionDecision {
			return invalid("awaiting decision node %q is not a prepared decision", obligation.NodeID)
		}
		if _, dup := seen[obligation.NodeID]; dup {
			return invalid("duplicate awaiting decision for node %q", obligation.NodeID)
		}
		seen[obligation.NodeID] = struct{}{}
		if checkpoint.Nodes[obligation.NodeID] != NodeReady {
			return invalid("awaiting decision node %q must be ready", obligation.NodeID)
		}
		// A plan-approval window is "the approval of plan attempt N", and N is
		// derived from the plan child's counter rather than stored, so a window
		// whose plan never ran has no identity for a verdict to bind to. This is
		// the same known-reference rule validateBlocked already applies to a parked
		// branch, and for the same reason: without it the obligation would fail
		// OPEN — the derived window would read as zero, which is exactly what an
		// authored decision requires, so a no-attempt verdict would decide it.
		//
		// It is an O(1) lookup on an obligation this loop is already visiting, not
		// a scan: only a prepared approval gate answers, and only about its own
		// prepared plan anchor.
		if attempt, approval := definition.ApprovalAttempt(checkpoint, obligation.NodeID); approval && attempt < 1 {
			return invalid("awaiting approval gate %q has no recorded plan attempt to decide", obligation.NodeID)
		}
	}
	return nil
}

// classifyCheckpoint is the demoted whole-graph exclusive-decision proof. It
// assumes validateCheckpoint already passed and encodes the TCL-650 slice's
// current expectations — at most one active node, closure marks everything
// unreachable as skipped, activation requires a fully settled candidate set
// with exactly one arrival, and a run holds at most one failure or done end.
// These are eligibility/test expectations, not universal checkpoint validity.
func classifyCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}

	// Classify every node against its incoming and outgoing dispositions.
	// These local checks encode the single-token exclusive semantics: at most
	// one arrival per node, closure marks everything unreachable as skipped,
	// and activation requires a fully settled candidate set.
	activeIndex, doomedIndex, doneEndIndex := -1, -1, -1
	for index, node := range definition.nodes {
		in := countDispositions(checkpoint, definition, node.incoming)
		out := countDispositions(checkpoint, definition, node.outgoing)
		status := checkpoint.Nodes[node.id]
		// The prepared order puts the entry node first, and it is the one node
		// no arrival ever activates: Initialize makes it ready, so every
		// candidate-set rule below is stated about non-entry nodes.
		entry := index == 0
		switch status {
		case NodePending:
			if entry {
				return invalid("entry node cannot be pending")
			}
			if in.unresolved == 0 {
				return invalid("pending node %q has a settled candidate set and must be ready or skipped", node.id)
			}
			// Settlement always resolves a target's whole candidate set in the
			// same reducer pass, so an arrival never rests on a pending node.
			if in.arrived != 0 {
				return invalid("pending node %q cannot hold an arrived incoming edge", node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("pending node %q cannot have settled outgoing edges", node.id)
			}
		case NodeReady, NodeRunning:
			if activeIndex >= 0 {
				return invalid("nodes %q and %q are both active", definition.nodes[activeIndex].id, node.id)
			}
			activeIndex = index
			if !entry && (in.unresolved > 0 || in.arrived != 1) {
				return invalid("active node %q requires a settled candidate set with exactly one arrival", node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("active node %q cannot have settled outgoing edges", node.id)
			}
			if status == NodeRunning {
				if node.kind != definitionTask {
					return invalid("only a task node may be running; got %q", node.id)
				}
				if command := checkpoint.FirstCommand(); command == nil || command.NodeID != node.id {
					return invalid("running node %q requires its outstanding command", node.id)
				}
			}
		case NodeDone:
			if !entry && (in.unresolved > 0 || in.arrived != 1) {
				return invalid("done node %q requires a settled candidate set with exactly one arrival", node.id)
			}
			switch node.kind {
			case definitionStart, definitionTask:
				if out.arrived != len(node.outgoing) {
					return invalid("done node %q must have arrived outgoing edges", node.id)
				}
			case definitionDecision:
				if out.arrived != 1 || out.notTaken != len(node.outgoing)-1 {
					return invalid("done decision %q must have exactly one chosen edge with every alternative closed", node.id)
				}
			case definitionEnd:
				if doneEndIndex >= 0 {
					return invalid("end nodes %q and %q are both done", definition.nodes[doneEndIndex].id, node.id)
				}
				doneEndIndex = index
			}
		case NodeSkipped:
			if len(node.incoming) == 0 || in.notTaken != len(node.incoming) {
				return invalid("skipped node %q requires every incoming edge to be not taken", node.id)
			}
			if out.notTaken != len(node.outgoing) {
				return invalid("skipped node %q requires every outgoing edge to be not taken", node.id)
			}
		case NodeFailed, NodeCanceled:
			if doomedIndex >= 0 {
				return invalid("nodes %q and %q both doomed the run", definition.nodes[doomedIndex].id, node.id)
			}
			doomedIndex = index
			if node.kind != definitionTask {
				return invalid("only a program task may fail or be canceled; got %q", node.id)
			}
			if !entry && (in.unresolved > 0 || in.arrived != 1) {
				return invalid("%s node %q requires a settled candidate set with exactly one arrival", status, node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("%s node %q cannot have settled outgoing edges", status, node.id)
			}
		case NodeBlocked:
			// A parked branch holds the sequential slice's single live token: it
			// is not final, and an operator resolution moves it again.
			if activeIndex >= 0 {
				return invalid("nodes %q and %q are both active", definition.nodes[activeIndex].id, node.id)
			}
			activeIndex = index
			if node.kind != definitionTask {
				return invalid("only a program task may block; got %q", node.id)
			}
			if !entry && (in.unresolved > 0 || in.arrived != 1) {
				return invalid("blocked node %q requires a settled candidate set with exactly one arrival", node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("blocked node %q cannot have settled outgoing edges", node.id)
			}
		default:
			return invalid("node %q has unknown status %q", node.id, status)
		}
	}

	if obligation := checkpoint.FirstAwaitingDecision(); obligation != nil {
		if checkpoint.Status != RunRunning {
			return invalid("awaiting decision requires a running run")
		}
		node, ok := definitionNodeByID(definition, obligation.NodeID)
		if !ok || node.kind != definitionDecision {
			return invalid("awaiting decision node %q is not a prepared decision", obligation.NodeID)
		}
		if checkpoint.Nodes[obligation.NodeID] != NodeReady {
			return invalid("awaiting decision node %q must be ready", obligation.NodeID)
		}
	}

	switch checkpoint.Status {
	case RunRunning:
		if doomedIndex >= 0 {
			return invalid("running run cannot contain doomed node %q", definition.nodes[doomedIndex].id)
		}
		if doneEndIndex >= 0 {
			return invalid("running run cannot contain done end node %q", definition.nodes[doneEndIndex].id)
		}
		if activeIndex < 0 {
			return invalid("running run must have one ready, running, or blocked node")
		}
		active := definition.nodes[activeIndex]
		if checkpoint.Nodes[active.id] == NodeReady && checkpoint.FirstCommand() != nil {
			return invalid("ready node %q cannot coexist with an outstanding command", active.id)
		}
		if active.kind == definitionDecision {
			if obligation := checkpoint.FirstAwaitingDecision(); obligation == nil || obligation.NodeID != active.id {
				return invalid("ready decision %q requires its awaiting-decision obligation", active.id)
			}
		} else if obligation := checkpoint.FirstAwaitingDecision(); obligation != nil {
			return invalid("awaiting decision %q disagrees with active node %q", obligation.NodeID, active.id)
		}
	case RunCompleted, RunCanceled, RunFailed:
		if checkpoint.FirstCommand() != nil {
			return invalid("terminal run cannot have an outstanding command")
		}
		if activeIndex >= 0 {
			return invalid("terminal run cannot have active node %q", definition.nodes[activeIndex].id)
		}
		if doneEndIndex >= 0 {
			if doomedIndex >= 0 {
				return invalid("terminal run cannot have both a done end and a failed or canceled task")
			}
			if definition.nodes[doneEndIndex].terminal != checkpoint.Status {
				return invalid("terminal run status %q disagrees with reached end status %q",
					checkpoint.Status, definition.nodes[doneEndIndex].terminal)
			}
			for _, node := range definition.nodes {
				if status := checkpoint.Nodes[node.id]; status != NodeDone && status != NodeSkipped {
					return invalid("terminal run requires node %q to be done or skipped; got %q", node.id, status)
				}
			}
			break
		}
		if checkpoint.Status != RunFailed && checkpoint.Status != RunCanceled {
			return invalid("terminal run status %q requires a done end node", checkpoint.Status)
		}
		if doomedIndex < 0 {
			return invalid("doomed run must contain one failed or canceled task, or a failed end result")
		}
		if got := doomStatus(checkpoint); got != checkpoint.Status {
			return invalid("terminal run status %q disagrees with its doomed node, which implies %q", checkpoint.Status, got)
		}
	default:
		return invalid("unknown run status %q", checkpoint.Status)
	}
	return nil
}

func validateEdgeShape(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	sources := 0
	for _, node := range definition.nodes {
		if len(node.outgoing) == 0 {
			continue
		}
		sources++
		dispositions, ok := checkpoint.Edges[node.id]
		if !ok || len(dispositions) != len(node.outgoing) {
			return invalid("edges must contain exactly the %d authored outcomes of node %q", len(node.outgoing), node.id)
		}
		for _, edgeIndex := range node.outgoing {
			edge := definition.edges[edgeIndex]
			switch dispositions[edge.outcome] {
			// The boundary decodes the disposition vocabulary and nothing more.
			// Winner/late-arrival semantics are maintained locally by the reducer;
			// no cross-graph semantic revalidation is performed on load.
			case EdgeUnresolved, EdgeArrived, EdgeNotTaken, EdgeArrivedLate:
			default:
				return invalid("edge %q/%q has unknown disposition %q", edge.from, edge.outcome, dispositions[edge.outcome])
			}
		}
	}
	if len(checkpoint.Edges) != sources {
		return invalid("edges must contain exactly the %d authored source nodes", sources)
	}
	return nil
}

func countDispositions(checkpoint Checkpoint, definition *Definition, edgeIndices []int) edgeCounts {
	var counts edgeCounts
	for _, edgeIndex := range edgeIndices {
		edge := definition.edges[edgeIndex]
		switch checkpoint.Edges[edge.from][edge.outcome] {
		case EdgeArrived:
			counts.arrived++
		case EdgeNotTaken:
			counts.notTaken++
		case EdgeArrivedLate:
			counts.arrivedLate++
		default:
			counts.unresolved++
		}
	}
	return counts
}

func definitionNodeByID(definition *Definition, nodeID string) (definitionNode, bool) {
	if definition == nil {
		return definitionNode{}, false
	}
	index, ok := definition.index[nodeID]
	if !ok {
		return definitionNode{}, false
	}
	return definition.nodes[index], true
}

func validRunID(runID string) bool {
	if len(runID) == 0 || len(runID) > 128 {
		return false
	}
	first := runID[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return false
	}
	for _, value := range []byte(runID) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-' {
			continue
		}
		return false
	}
	return true
}

func terminalStatus(result string) RunStatus {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "fail", "failed", "failure", "error":
		return RunFailed
	case "cancel", "canceled", "cancelled":
		return RunCanceled
	default:
		return RunCompleted
	}
}

func commandsEqual(left, right Command) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.NodeID == right.NodeID &&
		left.Attempt == right.Attempt &&
		left.Program.Profile == right.Program.Profile && left.Program.Run == right.Program.Run &&
		left.Program.Timeout == right.Program.Timeout && slices.Equal(left.Program.Args, right.Program.Args)
}
