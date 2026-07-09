package state

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func CheckInvariants(st *State) Diagnostics {
	var diagnostics Diagnostics
	diagnostics = append(diagnostics, WaitingNodesHaveWaitRecords(st)...)
	diagnostics = append(diagnostics, RunningAttemptsHaveCommandOrActor(st)...)
	diagnostics = append(diagnostics, CompletedDecisionsHaveOneChosenEdge(st)...)
	diagnostics = append(diagnostics, BlockedNodesHaveReasonAndOwner(st)...)
	diagnostics = append(diagnostics, DecisionActorsAreValid(st)...)
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
		if strings.TrimSpace(node.ChosenEdge) != "" && len(node.Decisions) == 1 {
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
		if wait.NodeID == nodeID && wait.Status == WaitStatusPending {
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
