package state

import (
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
