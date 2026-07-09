package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func CheckInvariants(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	diagnostics = append(diagnostics, EnumFieldsAreValid(st)...)
	diagnostics = append(diagnostics, WaitingNodesHaveWaitRecords(st)...)
	diagnostics = append(diagnostics, RunningAttemptsHaveCommandOrActor(st)...)
	diagnostics = append(diagnostics, OutstandingCommandsAreWellFormed(st)...)
	diagnostics = append(diagnostics, CompletedDecisionsHaveOneChosenEdge(st)...)
	diagnostics = append(diagnostics, BlockedNodesHaveReasonAndOwner(st)...)
	diagnostics = append(diagnostics, DecisionActorsAreValid(st)...)
	diagnostics = append(diagnostics, CompoundLinkageIsConsistent(st)...)
	diagnostics = append(diagnostics, CompletedStageChildrenHaveEvidence(st)...)
	return diagnostics
}

// CheckTemplateInvariants verifies recorded state against the run's pinned
// template. It complements CheckInvariants: state-only checks cannot see
// whether a recorded expansion matches what the template actually derives.
func CheckTemplateInvariants(st *State, tmpl *model.Template) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	if tmpl == nil {
		return Diagnostics{diagError("nil_template", "", "process template is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Parent != "" {
			continue
		}
		templateNode, ok := tmpl.Nodes[nodeID]
		if !ok {
			diagnostics = append(diagnostics, diagError("node_not_in_template", "nodes."+nodeID, fmt.Sprintf("node %q is not declared in template", nodeID)))
			continue
		}
		specs := model.ExpandNode(nodeID, templateNode)
		if len(node.Children) == 0 {
			if len(specs) > 0 && nodeProgressedWithoutExpansion(node.Status) {
				diagnostics = append(diagnostics, diagError(
					"compound_node_without_expansion",
					"nodes."+nodeID,
					fmt.Sprintf("compound node %q has status %q but no recorded expansion", nodeID, node.Status),
				))
			}
			continue
		}
		if len(specs) == 0 {
			diagnostics = append(diagnostics, diagError(
				"expansion_without_compound_template",
				"nodes."+nodeID+".children",
				fmt.Sprintf("node %q records an expansion but template node is not compound", nodeID),
			))
			continue
		}
		diagnostics = append(diagnostics, compareExpansion(st, nodeID, node, specs)...)
	}
	return diagnostics
}

func compareExpansion(st *State, nodeID string, node NodeState, specs []model.StageSpec) Diagnostics {
	var diagnostics Diagnostics
	if len(node.Children) != len(specs) {
		diagnostics = append(diagnostics, diagError(
			"expansion_template_mismatch",
			"nodes."+nodeID+".children",
			fmt.Sprintf("node %q records %d children but template derives %d", nodeID, len(node.Children), len(specs)),
		))
		return diagnostics
	}
	for i, spec := range specs {
		childID := node.Children[i]
		if childID != spec.ChildID {
			diagnostics = append(diagnostics, diagError(
				"expansion_template_mismatch",
				fmt.Sprintf("nodes.%s.children[%d]", nodeID, i),
				fmt.Sprintf("node %q child %q does not match template-derived child %q", nodeID, childID, spec.ChildID),
			))
			continue
		}
		child, ok := st.Nodes[childID]
		if !ok {
			continue // flagged by CompoundLinkageIsConsistent
		}
		if child.Stage != spec.Stage || child.StepID != spec.StepID {
			diagnostics = append(diagnostics, diagError(
				"expansion_template_mismatch",
				"nodes."+childID,
				fmt.Sprintf("child %q has stage %q step %q; template derives stage %q step %q", childID, child.Stage, child.StepID, spec.Stage, spec.StepID),
			))
		}
	}
	return diagnostics
}

func nodeProgressedWithoutExpansion(status NodeStatus) bool {
	switch status {
	case NodeStatusPending, NodeStatusReady:
		return false
	default:
		return true
	}
}

// CompoundLinkageIsConsistent checks the recorded parent/child expansion
// linkage without consulting the template: back-pointers, stage shape, and
// the parent status being explainable by its children.
func CompoundLinkageIsConsistent(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Parent != "" {
			diagnostics = append(diagnostics, checkStageChild(st, nodeID, node)...)
		} else if node.Stage != "" || node.StepID != "" {
			diagnostics = append(diagnostics, diagError("stage_without_parent", "nodes."+nodeID, fmt.Sprintf("node %q carries stage metadata but no parent", nodeID)))
		}
		if len(node.Children) > 0 {
			diagnostics = append(diagnostics, checkExpandedParent(st, nodeID, node)...)
		}
	}
	return diagnostics
}

func checkStageChild(st *State, nodeID string, node NodeState) Diagnostics {
	var diagnostics Diagnostics
	path := "nodes." + nodeID
	if !node.Stage.IsValid() {
		diagnostics = append(diagnostics, diagError("invalid_stage", path+".stage", fmt.Sprintf("stage child %q has invalid stage %q", nodeID, node.Stage)))
	}
	if (node.Stage == model.StageTest) != (node.StepID != "") {
		diagnostics = append(diagnostics, diagError("stage_step_mismatch", path+".stepId", fmt.Sprintf("stage child %q stage %q and step id %q are inconsistent", nodeID, node.Stage, node.StepID)))
	}
	if len(node.Children) > 0 {
		diagnostics = append(diagnostics, diagError("nested_expansion", path+".children", fmt.Sprintf("stage child %q must not itself be expanded", nodeID)))
	}
	parent, ok := st.Nodes[node.Parent]
	if !ok {
		diagnostics = append(diagnostics, diagError("stage_child_unknown_parent", path+".parent", fmt.Sprintf("stage child %q references undeclared parent %q", nodeID, node.Parent)))
		return diagnostics
	}
	if !slices.Contains(parent.Children, nodeID) {
		diagnostics = append(diagnostics, diagError("stage_child_not_in_parent", path+".parent", fmt.Sprintf("stage child %q is not listed in parent %q children", nodeID, node.Parent)))
	}
	if node.Status == NodeStatusBlocked && parent.Status != NodeStatusBlocked {
		diagnostics = append(diagnostics, diagError("blocked_child_unblocked_parent", path+".status", fmt.Sprintf("stage child %q is blocked but parent %q is %q", nodeID, node.Parent, parent.Status)))
	}
	return diagnostics
}

func checkExpandedParent(st *State, nodeID string, node NodeState) Diagnostics {
	var diagnostics Diagnostics
	path := "nodes." + nodeID
	switch node.Status {
	case NodeStatusRunning, NodeStatusBlocked, NodeStatusCompleted:
	default:
		diagnostics = append(diagnostics, diagError("expanded_parent_invalid_status", path+".status", fmt.Sprintf("expanded node %q has status %q; expected running, blocked, or completed", nodeID, node.Status)))
	}
	seen := map[string]bool{}
	blockedChild := false
	var doneCompleted bool
	for i, childID := range node.Children {
		childPath := fmt.Sprintf("%s.children[%d]", path, i)
		if seen[childID] {
			diagnostics = append(diagnostics, diagError("duplicate_expansion_child", childPath, fmt.Sprintf("expanded node %q lists child %q twice", nodeID, childID)))
			continue
		}
		seen[childID] = true
		if !strings.HasPrefix(childID, nodeID+".") {
			diagnostics = append(diagnostics, diagError("expansion_child_bad_prefix", childPath, fmt.Sprintf("child %q must be prefixed with %q", childID, nodeID+".")))
		}
		child, ok := st.Nodes[childID]
		if !ok {
			diagnostics = append(diagnostics, diagError("expansion_child_missing", childPath, fmt.Sprintf("expanded node %q lists undeclared child %q", nodeID, childID)))
			continue
		}
		if child.Parent != nodeID {
			diagnostics = append(diagnostics, diagError("expansion_child_wrong_parent", childPath, fmt.Sprintf("child %q has parent %q; expected %q", childID, child.Parent, nodeID)))
		}
		if child.Status == NodeStatusBlocked {
			blockedChild = true
		}
		if isLast := i == len(node.Children)-1; (child.Stage == model.StageDone) != isLast {
			diagnostics = append(diagnostics, diagError("expansion_done_stage_misplaced", childPath, fmt.Sprintf("expanded node %q: exactly the last child must be the done stage", nodeID)))
		}
		if child.Stage == model.StageDone && child.Status == NodeStatusCompleted {
			doneCompleted = true
		}
	}
	switch node.Status {
	case NodeStatusCompleted:
		if !doneCompleted {
			diagnostics = append(diagnostics, diagError("expanded_parent_completed_without_done", path+".status", fmt.Sprintf("expanded node %q is completed but its done stage is not", nodeID)))
		}
	case NodeStatusRunning:
		if doneCompleted {
			diagnostics = append(diagnostics, diagError("expanded_parent_running_after_done", path+".status", fmt.Sprintf("expanded node %q is running but its done stage is completed", nodeID)))
		}
	case NodeStatusBlocked:
		if !blockedChild {
			diagnostics = append(diagnostics, diagError("blocked_parent_without_blocked_child", path+".status", fmt.Sprintf("expanded node %q is blocked but no child is blocked", nodeID)))
		}
	}
	return diagnostics
}

// CompletedStageChildrenHaveEvidence enforces claimed-done-is-not-done for
// recorded state: a completed stage child (other than the done marker) must
// have a settled attempt carrying an evidence ref.
func CompletedStageChildrenHaveEvidence(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Parent == "" || node.Stage == model.StageDone || node.Status != NodeStatusCompleted {
			continue
		}
		if node.ActiveAttempt != nil && strings.TrimSpace(node.ActiveAttempt.EvidenceRef) != "" {
			continue
		}
		diagnostics = append(diagnostics, diagError(
			"completed_stage_child_without_evidence",
			"nodes."+nodeID+".activeAttempt",
			fmt.Sprintf("completed stage child %q has no settled attempt with an evidence ref", nodeID),
		))
	}
	return diagnostics
}

func EnumFieldsAreValid(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	if !st.Status.IsValid() {
		diagnostics = append(diagnostics, diagError("invalid_run_status", "status", fmt.Sprintf("invalid run status %q", st.Status)))
	}
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if !node.Status.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_node_status", "nodes."+nodeID+".status", fmt.Sprintf("invalid node status %q", node.Status)))
		}
		if node.Type != "" && !nodeTypeIsValid(node.Type) {
			diagnostics = append(diagnostics, diagError("invalid_node_type", "nodes."+nodeID+".type", fmt.Sprintf("invalid node type %q", node.Type)))
		}
	}
	for _, commandID := range sortedKeys(st.OutstandingCommands) {
		command := st.OutstandingCommands[commandID]
		if !command.Status.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_command_status", "outstandingCommands."+commandID+".status", fmt.Sprintf("invalid command status %q", command.Status)))
		}
	}
	for _, waitID := range sortedKeys(st.Waits) {
		wait := st.Waits[waitID]
		if !wait.Kind.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_wait_kind", "waits."+waitID+".kind", fmt.Sprintf("invalid wait kind %q", wait.Kind)))
		}
		if !wait.Status.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_wait_status", "waits."+waitID+".status", fmt.Sprintf("invalid wait status %q", wait.Status)))
		}
	}
	for _, timerID := range sortedKeys(st.Timers) {
		timer := st.Timers[timerID]
		if !timer.Status.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_timer_status", "timers."+timerID+".status", fmt.Sprintf("invalid timer status %q", timer.Status)))
		}
	}
	return diagnostics
}

func WaitingNodesHaveWaitRecords(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if !isWaitingStatus(node.Status) {
			continue
		}
		if waitingRecordExists(st, nodeID, node.Status) {
			continue
		}
		diagnostics = append(diagnostics, diagError(
			"waiting_node_without_wait",
			"nodes."+nodeID,
			fmt.Sprintf("node %q has status %q but no pending wait/timer record", nodeID, node.Status),
		))
	}
	return diagnostics
}

func RunningAttemptsHaveCommandOrActor(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Status != NodeStatusRunning {
			continue
		}
		if len(node.Children) > 0 {
			// Expanded compound parents run while their stage children carry the
			// attempts; child-level invariants cover them.
			continue
		}
		if node.ActiveAttempt != nil && (strings.TrimSpace(node.ActiveAttempt.CommandID) != "" || strings.TrimSpace(string(node.ActiveAttempt.Actor)) != "") {
			continue
		}
		diagnostics = append(diagnostics, diagError(
			"running_attempt_without_command_or_actor",
			"nodes."+nodeID+".activeAttempt",
			fmt.Sprintf("running node %q must have an active attempt with commandId or actor", nodeID),
		))
	}
	return diagnostics
}

func OutstandingCommandsAreWellFormed(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, commandID := range sortedKeys(st.OutstandingCommands) {
		command := st.OutstandingCommands[commandID]
		path := "outstandingCommands." + commandID
		if strings.TrimSpace(command.ID) == "" {
			diagnostics = append(diagnostics, diagError("missing_command_id", path+".id", "outstanding command id is required"))
		} else if command.ID != commandID {
			diagnostics = append(diagnostics, diagError("command_id_key_mismatch", path+".id", fmt.Sprintf("command id %q does not match map key %q", command.ID, commandID)))
		}
		if !command.Kind.IsValid() {
			diagnostics = append(diagnostics, diagError("invalid_command_kind", path+".kind", fmt.Sprintf("invalid command kind %q", command.Kind)))
		}
		if command.NodeID != "" {
			if _, ok := st.Nodes[command.NodeID]; !ok {
				diagnostics = append(diagnostics, diagError("command_unknown_node", path+".nodeId", fmt.Sprintf("command node %q is not declared", command.NodeID)))
			}
		}
		if command.Attempt < 0 {
			diagnostics = append(diagnostics, diagError("invalid_command_attempt", path+".attempt", "command attempt must be non-negative"))
		}
		if len(command.Payload) > 0 {
			var compact bytes.Buffer
			err := json.Compact(&compact, command.Payload)
			sum := sha256.Sum256(compact.Bytes())
			recomputed := fmt.Sprintf("%x", sum)
			if err != nil {
				diagnostics = append(diagnostics, diagError("command_payload_hash_mismatch", path+".payloadHash", fmt.Sprintf("outstanding command payload has stored hash %q but is invalid JSON: %v", command.PayloadHash, err)))
			} else if command.PayloadHash == "" || command.PayloadHash != recomputed {
				diagnostics = append(diagnostics, diagError("command_payload_hash_mismatch", path+".payloadHash", fmt.Sprintf("outstanding command payload has stored hash %q, recomputed %q", command.PayloadHash, recomputed)))
			}
		}
		if command.Status == CommandStatusObserved {
			if command.Actor != "" && !ValidateActorRef(command.Actor) {
				diagnostics = append(diagnostics, diagError("invalid_command_actor", path+".actor", fmt.Sprintf("invalid observed command actor %q", command.Actor)))
			}
		}
	}
	return diagnostics
}

func CompletedDecisionsHaveOneChosenEdge(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Type != model.NodeTypeDecision || node.Status != NodeStatusCompleted {
			continue
		}
		if strings.TrimSpace(node.ChosenEdge) != "" && len(node.Decisions) == 1 && node.Decisions[0].Verdict == node.ChosenEdge {
			continue
		}
		diagnostics = append(diagnostics, diagError(
			"completed_decision_without_one_chosen_edge",
			"nodes."+nodeID+".decisions",
			fmt.Sprintf("completed decision node %q must have exactly one decision record and chosenEdge", nodeID),
		))
	}
	return diagnostics
}

func BlockedNodesHaveReasonAndOwner(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Status != NodeStatusBlocked {
			continue
		}
		if strings.TrimSpace(node.BlockedReason) != "" && strings.TrimSpace(node.BlockedOwner) != "" {
			continue
		}
		diagnostics = append(diagnostics, diagError(
			"blocked_node_without_reason_owner",
			"nodes."+nodeID,
			fmt.Sprintf("blocked node %q must have blockedReason and blockedOwner", nodeID),
		))
	}
	return diagnostics
}

func DecisionActorsAreValid(st *State) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "", "process state is nil")}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(st.Nodes) {
		node := st.Nodes[nodeID]
		for i, decision := range node.Decisions {
			if ValidateActorRef(decision.Actor) {
				continue
			}
			diagnostics = append(diagnostics, diagError(
				"invalid_decision_actor",
				fmt.Sprintf("nodes.%s.decisions[%d].actor", nodeID, i),
				fmt.Sprintf("decision actor %q must be human:<id>, agent:agt_<id>, or program:<cmd>@exit<code>", decision.Actor),
			))
		}
	}
	return diagnostics
}

func isWaitingStatus(status NodeStatus) bool {
	return strings.HasPrefix(string(status), "waiting_")
}

func waitingRecordExists(st *State, nodeID string, status NodeStatus) bool {
	for _, wait := range st.Waits {
		if wait.NodeID == nodeID && wait.Status == WaitStatusPending && waitKindMatchesStatus(wait.Kind, status) {
			return true
		}
	}
	if status != NodeStatusWaitingTimer {
		return false
	}
	for _, timer := range st.Timers {
		if timer.NodeID == nodeID && timer.Status == WaitStatusPending {
			return true
		}
	}
	return false
}

func waitKindMatchesStatus(kind WaitKind, status NodeStatus) bool {
	switch status {
	case NodeStatusWaitingHuman:
		return kind == WaitKindHuman
	case NodeStatusWaitingAgent:
		return kind == WaitKindAgent
	case NodeStatusWaitingProgram:
		return kind == WaitKindProgram
	case NodeStatusWaitingTimer:
		return kind == WaitKindTimer
	case NodeStatusWaitingSignal:
		return kind == WaitKindSignal
	default:
		return false
	}
}

func nodeTypeIsValid(nodeType model.NodeType) bool {
	switch nodeType {
	case model.NodeTypeTask, model.NodeTypeDecision, model.NodeTypeWait, model.NodeTypeStart, model.NodeTypeEnd:
		return true
	default:
		return false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
