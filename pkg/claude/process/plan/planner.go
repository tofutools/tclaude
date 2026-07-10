package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

func Plan(st *state.State, tmpl *model.Template) ([]Command, error) {
	if st == nil {
		return nil, fmt.Errorf("process state is nil")
	}
	if tmpl == nil {
		return nil, fmt.Errorf("process template is nil")
	}
	if !AllowsExecution(st.Status) {
		return nil, nil
	}

	var commands []Command
	for _, nodeID := range sortedNodeIDs(st.Nodes) {
		node := st.Nodes[nodeID]
		if node.Parent != "" {
			childCommands, err := planStageChild(st, tmpl, nodeID, node)
			if err != nil {
				return nil, err
			}
			commands = append(commands, childCommands...)
			continue
		}
		templateNode, ok := tmpl.Nodes[nodeID]
		if !ok {
			return nil, fmt.Errorf("node %q is not in template", nodeID)
		}
		nodeCommands, err := planNode(st, tmpl, nodeID, node, templateNode)
		if err != nil {
			return nil, err
		}
		commands = append(commands, nodeCommands...)
	}
	return commands, nil
}

// AllowsExecution is the shared planner/executor lifecycle gate. Recovery may
// finish issued commands only while the planner would allow new commands.
func AllowsExecution(status state.RunStatus) bool {
	switch status {
	case state.RunStatusCompleted,
		state.RunStatusFailed,
		state.RunStatusCanceled,
		state.RunStatusPaused,
		state.RunStatusDirty,
		state.RunStatusInconsistent:
		return false
	default:
		return true
	}
}

func planNode(st *state.State, tmpl *model.Template, nodeID string, node state.NodeState, templateNode model.Node) ([]Command, error) {
	switch node.Status {
	case state.NodeStatusReady:
		return planReadyNode(st, tmpl, nodeID, node, templateNode)
	case state.NodeStatusRunning:
		return planRunningNode(st, nodeID, node, templateNode)
	case state.NodeStatusCompleted:
		return planCompletedNode(st, tmpl, nodeID, node, templateNode)
	case state.NodeStatusFailed:
		return planFailedNode(st, tmpl, nodeID, templateNode)
	case state.NodeStatusBlocked:
		return planBlockedNode(st, tmpl, nodeID, node, templateNode)
	default:
		return nil, nil
	}
}

// planBlockedNode makes a compound node's authored fail-edge decision
// reachable without treating poison as failure. The poisoned child and parent
// remain blocked while the independent decision obligation is resolved.
func planBlockedNode(st *state.State, tmpl *model.Template, nodeID string, node state.NodeState, templateNode model.Node) ([]Command, error) {
	if node.Parent != "" || node.BlockedNodeID == "" {
		return nil, nil
	}
	targetID := ResolveFailEdge(templateNode.Next)
	if targetID == "" {
		return nil, nil
	}
	targetTemplate, ok := tmpl.Nodes[targetID]
	if !ok || targetTemplate.Type != model.NodeTypeDecision || targetTemplate.Performer == nil || targetTemplate.Performer.Kind != model.PerformerHuman {
		return nil, nil
	}
	target, ok := st.Nodes[targetID]
	if !ok {
		return nil, fmt.Errorf("blocked node %q escalation target %q is not in state", nodeID, targetID)
	}
	if target.Status != state.NodeStatusPending {
		return nil, nil
	}
	cmd := newCommand(CommandKindActivateNode, st.RunID, nodeID, "blocked-to", targetID, fmt.Sprintf("attempt-%d", node.BlockedAttempt))
	cmd.NodeID = nodeID
	cmd.TargetNodeID = targetID
	cmd.SourceNodeStatus = state.NodeStatusBlocked
	cmd.NodeStatus = state.NodeStatusReady
	cmd.Attempt = node.BlockedAttempt
	if commandOutstanding(st, cmd.ID) {
		return nil, nil
	}
	return []Command{cmd}, nil
}

func planRunningNode(st *state.State, nodeID string, node state.NodeState, templateNode model.Node) ([]Command, error) {
	if node.ActiveAttempt == nil || strings.TrimSpace(node.ActiveAttempt.CommandID) == "" {
		return nil, nil
	}
	source, ok := st.OutstandingCommands[node.ActiveAttempt.CommandID]
	if !ok || source.Status != state.CommandStatusObserved {
		return nil, nil
	}
	attempt := node.ActiveAttempt.Attempt
	if attempt <= 0 {
		attempt = node.Attempt
	}
	cmd := newCommand(CommandKindSettleAttempt, st.RunID, nodeID, fmt.Sprintf("attempt-%d", attempt), "settle")
	cmd.NodeID = nodeID
	cmd.Attempt = attempt
	cmd.MaxAttempts = maxAttempts(templateNode.Retry)
	cmd.SourceCommandID = source.ID
	if commandOutstanding(st, cmd.ID) {
		return nil, nil
	}
	return []Command{cmd}, nil
}

func planReadyNode(st *state.State, tmpl *model.Template, nodeID string, node state.NodeState, templateNode model.Node) ([]Command, error) {
	switch templateNode.Type {
	case model.NodeTypeTask:
		if templateNode.IsCompound() {
			specs := model.ExpandNode(nodeID, templateNode)
			cmd := newCommand(CommandKindExpandNode, st.RunID, nodeID, "expand")
			cmd.NodeID = nodeID
			cmd.Children = ExpansionInits(nodeID, specs)
			if commandOutstanding(st, cmd.ID) {
				return nil, nil
			}
			return []Command{cmd}, nil
		}
		attempt := node.Attempt + 1
		cmd := newCommand(CommandKindStartAttempt, st.RunID, nodeID, fmt.Sprintf("attempt-%d", attempt), "start")
		cmd.NodeID = nodeID
		cmd.Attempt = attempt
		cmd.Performer = templateNode.Performer
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case model.NodeTypeDecision:
		if node.Attempt > 0 {
			_, source, linked, err := poisonEscalationSource(st, tmpl, nodeID)
			if err != nil {
				return nil, err
			}
			if linked && (source.Status != state.NodeStatusBlocked || source.BlockedAttempt != node.Attempt) {
				return nil, nil
			}
		}
		cmd := newCommand(CommandKindRecordDecision, st.RunID, nodeID, "decision")
		cmd.NodeID = nodeID
		cmd.Attempt = node.Attempt
		cmd.Performer = templateNode.Performer
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case model.NodeTypeWait:
		if waitSatisfied(st, nodeID) {
			return activationCommand(st, tmpl, nodeID, ResolvePassEdge(templateNode.Next, "pass"))
		}
		cmd := waitCommand(st.RunID, nodeID, templateNode.Wait)
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case model.NodeTypeStart:
		return activationCommand(st, tmpl, nodeID, ResolvePassEdge(templateNode.Next, "pass"))
	case model.NodeTypeEnd:
		return completeRunCommands(st, nodeID, TerminalRunStatus(templateNode)), nil
	default:
		return nil, fmt.Errorf("unsupported node type %q", templateNode.Type)
	}
}

func planCompletedNode(st *state.State, tmpl *model.Template, nodeID string, node state.NodeState, templateNode model.Node) ([]Command, error) {
	switch templateNode.Type {
	case model.NodeTypeTask:
		outcome := "pass"
		if node.ActiveAttempt != nil && strings.TrimSpace(node.ActiveAttempt.Outcome) != "" {
			outcome = strings.ToLower(strings.TrimSpace(node.ActiveAttempt.Outcome))
		}
		if IsFailOutcome(outcome) {
			return planFailedNode(st, tmpl, nodeID, templateNode)
		}
		return activationCommand(st, tmpl, nodeID, ResolvePassEdge(templateNode.Next, outcome))
	case model.NodeTypeDecision:
		if strings.TrimSpace(node.ChosenEdge) == "" {
			return nil, nil
		}
		target, ok := DecisionEdge(templateNode.Next, node.ChosenEdge)
		if !ok {
			return nil, fmt.Errorf("decision node %q has no edge for verdict %q; available edges: %s", nodeID, node.ChosenEdge, strings.Join(sortedEdgeKeys(templateNode.Next), ", "))
		}
		if command, found, err := escalationResolutionCommand(st, tmpl, nodeID, node, target); err != nil {
			return nil, err
		} else if found {
			if command.Kind == "" {
				return nil, nil
			}
			return []Command{command}, nil
		}
		return activationCommand(st, tmpl, nodeID, target)
	case model.NodeTypeStart, model.NodeTypeWait:
		return activationCommand(st, tmpl, nodeID, ResolvePassEdge(templateNode.Next, "pass"))
	case model.NodeTypeEnd:
		return completeRunCommands(st, nodeID, TerminalRunStatus(templateNode)), nil
	default:
		return nil, fmt.Errorf("unsupported node type %q", templateNode.Type)
	}
}

// escalationResolutionCommand recognizes a decision reached from a poisoned
// compound node's fail edge. A choice back to that node means retry; a choice
// to a canceled end means cancel. Both flow through ResolveBlocked so the
// release is generation-bound and recorded as an admin decision.
func escalationResolutionCommand(st *state.State, tmpl *model.Template, decisionID string, decision state.NodeState, targetID string) (Command, bool, error) {
	decisionTemplate, ok := tmpl.Nodes[decisionID]
	if !ok || decisionTemplate.Performer == nil || decisionTemplate.Performer.Kind != model.PerformerHuman {
		return Command{}, false, nil
	}
	blockedID, blocked, linked, err := poisonEscalationSource(st, tmpl, decisionID)
	if err != nil {
		return Command{}, false, err
	}
	if !linked {
		return Command{}, false, nil
	}
	if decision.Attempt <= 0 || blocked.Status != state.NodeStatusBlocked || blocked.BlockedAttempt != decision.Attempt || blocked.BlockedNodeID == "" {
		// This decision was never offered for the current poison generation,
		// or explicit unblock already made it stale. Suppress ordinary routing.
		return Command{}, true, nil
	}
	if len(decision.Decisions) == 0 {
		return Command{}, false, fmt.Errorf("completed escalation decision %q has no decision record", decisionID)
	}
	record := decision.Decisions[len(decision.Decisions)-1]

	resolution := state.BlockDecision("")
	if targetID == blockedID {
		resolution = state.BlockDecisionRetry
	} else if target, ok := tmpl.Nodes[targetID]; ok && target.Type == model.NodeTypeEnd && TerminalRunStatus(target) == state.RunStatusCanceled {
		resolution = state.BlockDecisionCancel
	} else {
		return Command{}, true, fmt.Errorf("escalation decision %q choice %q must retry blocked node %q or target a canceled end", decisionID, decision.ChosenEdge, blockedID)
	}
	cmd := newCommand(CommandKindResolveBlock, st.RunID, decisionID, blockedID, fmt.Sprintf("attempt-%d", blocked.BlockedAttempt), string(resolution))
	cmd.NodeID = decisionID
	cmd.TargetNodeID = blockedID
	cmd.BlockedAttempt = blocked.BlockedAttempt
	cmd.BlockDecision = resolution
	cmd.Actor = record.Actor
	cmd.Reason = fmt.Sprintf("decision by %s selected %q", record.Actor, record.Verdict)
	cmd.EvidenceRef = record.EvidenceRef
	if commandOutstanding(st, cmd.ID) {
		// The decision is linked and its generation-bound resolution is already
		// claimed. Suppress ordinary edge routing while recovery resumes it.
		return Command{}, true, nil
	}
	return cmd, true, nil
}

func poisonEscalationSource(st *state.State, tmpl *model.Template, decisionID string) (string, state.NodeState, bool, error) {
	var sourceID string
	for _, candidateID := range sortedNodeIDs(st.Nodes) {
		candidate := st.Nodes[candidateID]
		if candidate.Parent != "" {
			continue
		}
		templateNode, ok := tmpl.Nodes[candidateID]
		if !ok || !templateNode.IsCompound() || ResolveFailEdge(templateNode.Next) != decisionID {
			continue
		}
		if sourceID != "" {
			return "", state.NodeState{}, false, fmt.Errorf("decision node %q escalates multiple compound nodes (%q and %q)", decisionID, sourceID, candidateID)
		}
		sourceID = candidateID
	}
	if sourceID == "" {
		return "", state.NodeState{}, false, nil
	}
	return sourceID, st.Nodes[sourceID], true, nil
}

func planFailedNode(st *state.State, tmpl *model.Template, nodeID string, templateNode model.Node) ([]Command, error) {
	node := st.Nodes[nodeID]
	if SettleNodeStatus("fail", node.Attempt, templateNode.Retry) == state.NodeStatusReady {
		cmd := newCommand(CommandKindActivateNode, st.RunID, nodeID, "retry", fmt.Sprintf("attempt-%d", node.Attempt+1))
		cmd.NodeID = nodeID
		cmd.TargetNodeID = nodeID
		cmd.NodeStatus = state.NodeStatusReady
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	}
	target := ResolveFailEdge(templateNode.Next)
	if target == "" {
		return completeRunCommands(st, nodeID, state.RunStatusFailed), nil
	}
	return activationCommand(st, tmpl, nodeID, target)
}

func planStageChild(st *state.State, tmpl *model.Template, nodeID string, node state.NodeState) ([]Command, error) {
	parent, ok := st.Nodes[node.Parent]
	if !ok {
		return nil, fmt.Errorf("stage child %q references undeclared parent %q", nodeID, node.Parent)
	}
	specs, err := CompoundSpecs(tmpl, node.Parent, parent)
	if err != nil {
		return nil, err
	}
	spec, err := StageSpecFor(specs, nodeID)
	if err != nil {
		return nil, err
	}
	switch node.Status {
	case state.NodeStatusReady:
		// A re-entering gate whose work evidence is unchanged short-circuits:
		// the previous verdict stands as an engine decision, no performer runs.
		if spec.Stage.IsGateStage() {
			if hash, ok := ShortCircuitHash(parent.Children, specs, st.Nodes, nodeID); ok {
				cmd := newCommand(CommandKindShortCircuit, st.RunID, nodeID, "short-circuit", settleGeneration(node), hash)
				cmd.NodeID = nodeID
				cmd.EvidenceHash = hash
				cmd.DecisionCount = len(node.Decisions)
				if commandOutstanding(st, cmd.ID) {
					return nil, nil
				}
				return []Command{cmd}, nil
			}
		}
		attempt := node.Attempt + 1
		cmd := newCommand(CommandKindStartAttempt, st.RunID, nodeID, fmt.Sprintf("attempt-%d", attempt), "start")
		cmd.NodeID = nodeID
		cmd.Attempt = attempt
		cmd.Performer = spec.Performer
		if !spec.Stage.IsGateStage() {
			cmd.RetryMode = model.RetryMode(spec.Retry)
			if node.PendingFeedback != nil {
				cmd.Feedback = node.PendingFeedback.Feedback
				cmd.FeedbackFrom = node.PendingFeedback.FromNodeID
			}
		}
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case state.NodeStatusRunning:
		if node.ActiveAttempt == nil || strings.TrimSpace(node.ActiveAttempt.CommandID) == "" {
			return nil, nil
		}
		source, ok := st.OutstandingCommands[node.ActiveAttempt.CommandID]
		if !ok || source.Status != state.CommandStatusObserved {
			return nil, nil
		}
		attempt := node.ActiveAttempt.Attempt
		if attempt <= 0 {
			attempt = node.Attempt
		}
		cmd := newCommand(CommandKindSettleAttempt, st.RunID, nodeID, fmt.Sprintf("attempt-%d", attempt), "settle")
		cmd.NodeID = nodeID
		cmd.Attempt = attempt
		cmd.MaxAttempts = StageMaxAttempts(spec)
		cmd.SourceCommandID = source.ID
		if spec.Stage.IsGateStage() {
			cmd.WorkEvidenceHash = WorkEvidenceHash(parent.Children, specs, st.Nodes, nodeID)
		}
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case state.NodeStatusCompleted:
		if spec.Stage == model.StageDone {
			// The reducer completes the parent atomically with the done stage;
			// a completed done marker needs no further stage commands.
			return nil, nil
		}
		return planSettledStageTransition(st, nodeID, node, parent, specs, EffectiveStageOutcome(node))
	case state.NodeStatusFailed:
		return planSettledStageTransition(st, nodeID, node, parent, specs, "fail")
	case state.NodeStatusSkipped:
		// A skip is a completed-by-decision stage outcome. Its durable block
		// resolution record, rather than a performer settle, supplies the audit.
		if node.BlockResolution == nil || (node.BlockResolution.Decision != state.BlockDecisionSkip && node.BlockResolution.Decision != state.BlockDecisionCancel) {
			return nil, fmt.Errorf("skipped stage child %q has no audited skip/cancel block resolution", nodeID)
		}
		return planSettledStageTransition(st, nodeID, node, parent, specs, "pass")
	default:
		return nil, nil
	}
}

func planSettledStageTransition(st *state.State, nodeID string, node state.NodeState, parent state.NodeState, specs []model.StageSpec, outcome string) ([]Command, error) {
	settle := StageSettle{
		ChildID:   nodeID,
		Outcome:   outcome,
		Attempt:   node.Attempt,
		FailCount: node.FailCount,
	}
	if node.ActiveAttempt != nil {
		settle.Feedback = node.ActiveAttempt.Feedback
		settle.EvidenceRef = node.ActiveAttempt.EvidenceRef
	}
	transition, err := NextAfterStage(node.Parent, parent.Children, specs, st.Nodes, settle)
	if err != nil {
		return nil, err
	}
	switch transition.Kind {
	case TransitionActivateChild:
		next, ok := st.Nodes[transition.NextChildID]
		if !ok {
			return nil, fmt.Errorf("stage child %q is not in state", transition.NextChildID)
		}
		if next.Status != state.NodeStatusPending {
			return nil, nil
		}
		cmd := newCommand(CommandKindActivateNode, st.RunID, nodeID, "to", transition.NextChildID, settleGeneration(node))
		cmd.NodeID = nodeID
		cmd.TargetNodeID = transition.NextChildID
		cmd.NodeStatus = state.NodeStatusReady
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case TransitionCompleteParent:
		done, ok := st.Nodes[transition.DoneChildID]
		if !ok {
			return nil, fmt.Errorf("done stage %q is not in state", transition.DoneChildID)
		}
		if done.Status != state.NodeStatusPending {
			return nil, nil
		}
		cmd := newCommand(CommandKindActivateNode, st.RunID, nodeID, "to", transition.DoneChildID, settleGeneration(node))
		cmd.NodeID = nodeID
		cmd.TargetNodeID = transition.DoneChildID
		cmd.NodeStatus = state.NodeStatusCompleted
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case TransitionRetryChild:
		cmd := newCommand(CommandKindActivateNode, st.RunID, nodeID, "retry", fmt.Sprintf("attempt-%d", node.Attempt+1))
		cmd.NodeID = nodeID
		cmd.TargetNodeID = nodeID
		cmd.NodeStatus = state.NodeStatusReady
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case TransitionFeedbackLoop:
		target, ok := st.Nodes[transition.TargetStageID]
		if !ok {
			return nil, fmt.Errorf("feedback target %q is not in state", transition.TargetStageID)
		}
		if target.Status != state.NodeStatusCompleted {
			// The loop already re-entered (target re-readied or running again).
			return nil, nil
		}
		cmd := newCommand(CommandKindGateFeedback, st.RunID, nodeID, "feedback", settleGeneration(node))
		cmd.NodeID = nodeID
		cmd.Attempt = node.Attempt
		cmd.DecisionCount = len(node.Decisions)
		cmd.TargetNodeID = transition.TargetStageID
		cmd.Gates = transition.ResetGates
		cmd.ResetCounters = transition.ResetCounters
		cmd.Feedback = transition.Feedback
		cmd.Reason = transition.Reason
		cmd.EvidenceRef = transition.EvidenceRef
		if commandOutstanding(st, cmd.ID) {
			return nil, nil
		}
		return []Command{cmd}, nil
	case TransitionPoison:
		return poisonCommands(st, nodeID, node, transition), nil
	default:
		return nil, fmt.Errorf("unsupported stage transition %q", transition.Kind)
	}
}

// poisonCommands emits ONE block command covering the failed stage child and
// its parent mirror (TargetNodeID). A single command keeps both node_blocked
// events in one append batch (CAS-level, not a crash guarantee): a child-only
// intermediate checkpoint would violate the blocked-mirror invariant and
// stall a replanning executor. The future unblock flow (poison resolution,
// TCL-279) needs the same child+parent pairing in reverse — a child-only
// unblock would trip blocked_parent_without_blocked_child, and auto-complete
// never clears BlockedReason/BlockedOwner.
func poisonCommands(st *state.State, nodeID string, node state.NodeState, transition StageTransition) []Command {
	if node.Status == state.NodeStatusBlocked {
		return nil
	}
	block := newCommand(CommandKindBlockNode, st.RunID, nodeID, "block", fmt.Sprintf("attempt-%d", node.Attempt))
	block.NodeID = nodeID
	block.TargetNodeID = node.Parent
	block.Attempt = node.Attempt
	block.Reason = transition.Reason
	block.Owner = transition.Owner
	if commandOutstanding(st, block.ID) {
		return nil
	}
	return []Command{block}
}

// StageSpecFor finds the template-derived spec for a stage child id.
func StageSpecFor(specs []model.StageSpec, childID string) (model.StageSpec, error) {
	for _, spec := range specs {
		if spec.ChildID == childID {
			return spec, nil
		}
	}
	return model.StageSpec{}, fmt.Errorf("stage child %q is not derivable from its parent template", childID)
}

func activationCommand(st *state.State, tmpl *model.Template, fromNodeID, targetNodeID string) ([]Command, error) {
	if strings.TrimSpace(targetNodeID) == "" {
		return nil, nil
	}
	target, ok := st.Nodes[targetNodeID]
	if !ok {
		return nil, fmt.Errorf("target node %q is not in state", targetNodeID)
	}
	targetTemplate, ok := tmpl.Nodes[targetNodeID]
	if !ok {
		return nil, fmt.Errorf("target node %q is not in template", targetNodeID)
	}
	if target.Status != state.NodeStatusPending {
		return nil, nil
	}
	status := state.NodeStatusReady
	if targetTemplate.Type == model.NodeTypeEnd {
		status = state.NodeStatusCompleted
	}
	cmd := newCommand(CommandKindActivateNode, st.RunID, fromNodeID, "to", targetNodeID)
	cmd.NodeID = fromNodeID
	cmd.TargetNodeID = targetNodeID
	cmd.SourceNodeStatus = state.NodeStatusCompleted
	cmd.NodeStatus = status
	if commandOutstanding(st, cmd.ID) {
		return nil, nil
	}
	out := []Command{cmd}
	if targetTemplate.Type == model.NodeTypeEnd {
		out = append(out, completeRunCommands(st, targetNodeID, TerminalRunStatus(targetTemplate))...)
	}
	return out, nil
}

func waitSatisfied(st *state.State, nodeID string) bool {
	for _, wait := range st.Waits {
		if wait.NodeID == nodeID && wait.Status == state.WaitStatusSatisfied {
			return true
		}
	}
	for _, timer := range st.Timers {
		if timer.NodeID == nodeID && timer.Status == state.WaitStatusSatisfied {
			return true
		}
	}
	return false
}

func completeRunCommands(st *state.State, nodeID string, status state.RunStatus) []Command {
	cmd := completeRunCommand(st.RunID, nodeID, status)
	if commandOutstanding(st, cmd.ID) {
		return nil
	}
	return []Command{cmd}
}

func completeRunCommand(runID, nodeID string, status state.RunStatus) Command {
	cmd := newCommand(CommandKindCompleteRun, runID, nodeID, string(status))
	cmd.NodeID = nodeID
	cmd.RunStatus = status
	return cmd
}

func waitCommand(runID, nodeID string, wait *model.WaitConfig) Command {
	if wait != nil && strings.TrimSpace(wait.Signal) != "" && strings.TrimSpace(wait.Duration) == "" && strings.TrimSpace(wait.Until) == "" {
		cmd := newCommand(CommandKindWaitSignal, runID, nodeID, "signal", wait.Signal)
		cmd.NodeID = nodeID
		cmd.Signal = wait.Signal
		cmd.WaitKind = state.WaitKindSignal
		cmd.WaitID = deterministicSlotID(CommandKindWaitSignal, runID, nodeID, "signal", wait.Signal)
		return cmd
	}
	cmd := newCommand(CommandKindSetTimer, runID, nodeID, "timer")
	cmd.NodeID = nodeID
	cmd.WaitKind = state.WaitKindTimer
	cmd.WaitID = deterministicSlotID(CommandKindSetTimer, runID, nodeID, "timer")
	if wait != nil {
		cmd.Duration = wait.Duration
		cmd.Until = wait.Until
		cmd.Signal = wait.Signal
	}
	return cmd
}

// settleGeneration distinguishes successive settles of the same stage child
// inside deterministic idempotency keys, so a loop re-entry plans fresh
// commands instead of colliding with an observed slot from an earlier window:
// attempts for work stages, verdict count for gates (a short-circuit settles
// a gate without consuming an attempt, but always appends a verdict).
func settleGeneration(node state.NodeState) string {
	if node.Stage.IsGateStage() {
		return fmt.Sprintf("decisions-%d", len(node.Decisions))
	}
	return fmt.Sprintf("attempt-%d", node.Attempt)
}

func maxAttempts(retry *model.RetryPolicy) int {
	return model.RetryBudget(retry)
}

func deterministicSlotID(kind CommandKind, runID, nodeID string, parts ...string) string {
	keyParts := append([]string{nodeID}, parts...)
	cmd := newCommand(kind, runID, keyParts...)
	return "slot_" + strings.TrimPrefix(cmd.ID, "cmd_")
}

func commandOutstanding(st *state.State, commandID string) bool {
	command, ok := st.OutstandingCommands[commandID]
	if !ok {
		return false
	}
	return command.Status == state.CommandStatusIssued || command.Status == state.CommandStatusObserved
}

func sortedNodeIDs(nodes map[string]state.NodeState) []string {
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedEdgeKeys(next model.Next) []string {
	keys := make([]string, 0, len(next))
	for key := range next {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
