package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// Definition is the immutable executable projection of one pinned template
// and its immutable run parameters. Its fields stay private so preparation is
// the only way to construct executable state and callers cannot mutate it.
// Nodes are held in a deterministic topological order with start first.
type Definition struct {
	nodes []definitionNode
	index map[string]int
	edges []definitionEdge
}

type definitionNodeKind uint8

const (
	definitionStart definitionNodeKind = iota + 1
	definitionTask
	definitionEnd
	definitionDecision
)

type definitionNode struct {
	id       string
	kind     definitionNodeKind
	program  ProgramCommand // task nodes only
	terminal RunStatus      // end nodes only
	verdicts []string       // decision nodes only: sorted authored outcome labels
	outgoing []int          // edge indices, sorted by outcome label
	incoming []int          // edge indices, in edge order
}

type definitionEdge struct {
	from      string
	outcome   string
	to        string
	fromIndex int
	toIndex   int
}

// Prepare performs all immutable work once: complete authoring validation,
// exclusive-decision eligibility, deterministic topological ordering, edge
// identity derivation, decision verdict binding, parameter binding, and final
// bound-program validation. Plan, Apply, and Advance reuse the result.
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
	for _, nodeID := range order {
		node := tmpl.Nodes[nodeID]
		prepared := definitionNode{id: nodeID}
		switch node.Type {
		case model.NodeTypeStart:
			prepared.kind = definitionStart
		case model.NodeTypeTask:
			prepared.kind = definitionTask
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
		definition.index[nodeID] = len(definition.nodes)
		definition.nodes = append(definition.nodes, prepared)
	}
	for fromIndex := range definition.nodes {
		from := &definition.nodes[fromIndex]
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
	}
	return definition, nil
}

// topologicalOrder returns every node exactly once, start first, in a
// deterministic order that always places edge sources before their targets.
// Eligibility guarantees acyclicity, so a leftover node fails closed as an
// invalid definition rather than looping.
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
// values, and unique bounded command/decision obligation entries whose
// deterministic identity and node binding are compatible with a known node. It
// deliberately does NOT run the whole-graph exclusive-decision classification
// (single active node, single failure, pending-arrival exclusion, activation
// proofs): those are current-slice expectations demoted to ClassifyCheckpoint
// so Processes can load and persist structurally safe parallel-ready state
// before parallelism semantics settle. It never re-runs static template or
// graph-shape validation, which happened once in Prepare.
func ValidateCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	return validateCheckpoint(checkpoint, definition)
}

// ClassifyCheckpoint is the exhaustive, deliberately strict current-slice
// invariant proof. It runs the runtime structural validator and then the
// whole-graph exclusive-decision classification the runtime hot path no longer
// performs on every transition. Tests call it after every engine-generated
// transition so the TCL-650 slice keeps its full bug-finding coverage without
// making those single-active/single-failure expectations the runtime
// compatibility contract. It is a test/diagnostic entry point, not a hot path.
func ClassifyCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return err
	}
	return classifyCheckpoint(checkpoint, definition)
}

// edgeCounts aggregates the dispositions on one side of a node.
type edgeCounts struct {
	unresolved int
	arrived    int
	notTaken   int
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
		case NodePending, NodeReady, NodeRunning, NodeDone, NodeFailed, NodeSkipped:
		default:
			return invalid("node %q has unknown status %q", nodeID, status)
		}
	}
	if err := validateEdgeShape(checkpoint, definition); err != nil {
		return err
	}
	if err := validateCommands(checkpoint, definition); err != nil {
		return err
	}
	return validateAwaitingDecisions(checkpoint, definition)
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
		expected := programCommand(checkpoint.RunID, node)
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
	activeIndex, failedIndex, doneEndIndex := -1, -1, -1
	for index, node := range definition.nodes {
		in := countDispositions(checkpoint, definition, node.incoming)
		out := countDispositions(checkpoint, definition, node.outgoing)
		status := checkpoint.Nodes[node.id]
		switch status {
		case NodePending:
			if index == 0 {
				return invalid("start node cannot be pending")
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
			if index != 0 && (in.unresolved > 0 || in.arrived != 1) {
				return invalid("active node %q requires a settled candidate set with exactly one arrival", node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("active node %q cannot have settled outgoing edges", node.id)
			}
			if status == NodeRunning {
				if node.kind != definitionTask {
					return invalid("only a task node may be running; got %q", node.id)
				}
				if command := checkpoint.OutstandingCommand(); command == nil || command.NodeID != node.id {
					return invalid("running node %q requires its outstanding command", node.id)
				}
			}
		case NodeDone:
			if index != 0 && (in.unresolved > 0 || in.arrived != 1) {
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
		case NodeFailed:
			if failedIndex >= 0 {
				return invalid("nodes %q and %q are both failed", definition.nodes[failedIndex].id, node.id)
			}
			failedIndex = index
			if node.kind != definitionTask {
				return invalid("only a program task may fail; got %q", node.id)
			}
			if in.unresolved > 0 || in.arrived != 1 {
				return invalid("failed node %q requires a settled candidate set with exactly one arrival", node.id)
			}
			if out.unresolved != len(node.outgoing) {
				return invalid("failed node %q cannot have settled outgoing edges", node.id)
			}
		default:
			return invalid("node %q has unknown status %q", node.id, status)
		}
	}

	if obligation := checkpoint.AwaitingDecision(); obligation != nil {
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
		if failedIndex >= 0 {
			return invalid("running run cannot contain failed node %q", definition.nodes[failedIndex].id)
		}
		if doneEndIndex >= 0 {
			return invalid("running run cannot contain done end node %q", definition.nodes[doneEndIndex].id)
		}
		if activeIndex < 0 {
			return invalid("running run must have one ready or running node")
		}
		active := definition.nodes[activeIndex]
		if checkpoint.Nodes[active.id] == NodeReady && checkpoint.OutstandingCommand() != nil {
			return invalid("ready node %q cannot coexist with an outstanding command", active.id)
		}
		if active.kind == definitionDecision {
			if obligation := checkpoint.AwaitingDecision(); obligation == nil || obligation.NodeID != active.id {
				return invalid("ready decision %q requires its awaiting-decision obligation", active.id)
			}
		} else if obligation := checkpoint.AwaitingDecision(); obligation != nil {
			return invalid("awaiting decision %q disagrees with active node %q", obligation.NodeID, active.id)
		}
	case RunCompleted, RunCanceled, RunFailed:
		if checkpoint.OutstandingCommand() != nil {
			return invalid("terminal run cannot have an outstanding command")
		}
		if activeIndex >= 0 {
			return invalid("terminal run cannot have active node %q", definition.nodes[activeIndex].id)
		}
		if doneEndIndex >= 0 {
			if failedIndex >= 0 {
				return invalid("terminal run cannot have both a done end and a failed task")
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
		if checkpoint.Status != RunFailed {
			return invalid("terminal run status %q requires a done end node", checkpoint.Status)
		}
		if failedIndex < 0 {
			return invalid("failed run must contain one failed task or a failed end result")
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
			case EdgeUnresolved, EdgeArrived, EdgeNotTaken:
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
		left.Program.Profile == right.Program.Profile && left.Program.Run == right.Program.Run &&
		left.Program.Timeout == right.Program.Timeout && slices.Equal(left.Program.Args, right.Program.Args)
}
