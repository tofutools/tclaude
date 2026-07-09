package state

import (
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

type EventType string

const (
	EventRunInitialized           EventType = "run_initialized"
	EventRunStatusSet             EventType = "run_status_set"
	EventNodeAttemptStarted       EventType = "node_attempt_started"
	EventNodeAttemptSettled       EventType = "node_attempt_settled"
	EventDecisionRecorded         EventType = "decision_recorded"
	EventNodeBlocked              EventType = "node_blocked"
	EventNodeUnblocked            EventType = "node_unblocked"
	EventWaitCreated              EventType = "wait_created"
	EventWaitSatisfied            EventType = "wait_satisfied"
	EventTimerCreated             EventType = "timer_created"
	EventTimerSatisfied           EventType = "timer_satisfied"
	EventCommandIssued            EventType = "command_issued"
	EventCommandObserved          EventType = "command_observed"
	EventAdminRepairRecorded      EventType = "admin_repair_recorded"
	EventTemplateDivergenceMarked EventType = "template_divergence_marked"
)

type Event struct {
	Type EventType `json:"type"`
	Seq  int64     `json:"seq,omitempty"`
	At   time.Time `json:"at,omitempty"`

	LogChecksum string `json:"logChecksum,omitempty"`

	RunID               string              `json:"runId,omitempty"`
	RunStatus           RunStatus           `json:"runStatus,omitempty"`
	OriginalTemplateRef string              `json:"originalTemplateRef,omitempty"`
	CurrentTemplateRef  string              `json:"currentTemplateRef,omitempty"`
	TemplateDivergence  *TemplateDivergence `json:"templateDivergence,omitempty"`
	Nodes               []NodeInit          `json:"nodes,omitempty"`

	NodeID     string         `json:"nodeId,omitempty"`
	NodeType   model.NodeType `json:"nodeType,omitempty"`
	NodeStatus NodeStatus     `json:"nodeStatus,omitempty"`
	Assignee   string         `json:"assignee,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
	Actor      ActorRef       `json:"actor,omitempty"`
	CommandID  string         `json:"commandId,omitempty"`
	Outcome    string         `json:"outcome,omitempty"`

	Command     *OutstandingCommand `json:"command,omitempty"`
	ExternalRef string              `json:"externalRef,omitempty"`

	Decision   *DecisionRecord `json:"decision,omitempty"`
	ChosenEdge string          `json:"chosenEdge,omitempty"`

	Wait   *WaitRecord `json:"wait,omitempty"`
	WaitID string      `json:"waitId,omitempty"`

	Timer   *TimerRecord `json:"timer,omitempty"`
	TimerID string       `json:"timerId,omitempty"`

	Reason      string `json:"reason,omitempty"`
	Owner       string `json:"owner,omitempty"`
	EvidenceRef string `json:"evidenceRef,omitempty"`
}

func Apply(st State, event Event) (State, error) {
	if event.Seq > 0 && st.LastLogSeq > 0 && event.Seq <= st.LastLogSeq {
		return State{}, fmt.Errorf("event seq %d must be greater than lastLogSeq %d", event.Seq, st.LastLogSeq)
	}
	next := Clone(st)
	normalizeState(&next)

	if err := applyEvent(&next, event); err != nil {
		return State{}, err
	}
	if event.Seq > 0 {
		next.LastLogSeq = event.Seq
	}
	if event.LogChecksum != "" {
		next.LogChecksum = event.LogChecksum
	}
	return next, nil
}

func ApplyAll(st State, events []Event) (State, error) {
	// This intentionally reuses Apply's clone-per-event semantics so replay has
	// the same immutability contract as single-event application. Long-log
	// executors can optimize this later behind the same reducer semantics.
	next := st
	for _, event := range events {
		applied, err := Apply(next, event)
		if err != nil {
			return State{}, err
		}
		next = applied
	}
	return next, nil
}

func applyEvent(st *State, event Event) error {
	switch event.Type {
	case EventRunInitialized:
		if st.RunID != "" || len(st.Nodes) > 0 {
			return fmt.Errorf("run is already initialized")
		}
		initialized := New(event.RunID, event.OriginalTemplateRef, event.CurrentTemplateRef, event.Nodes)
		initialized.Status = RunStatusRunning
		if event.RunStatus != "" {
			if !event.RunStatus.IsValid() {
				return fmt.Errorf("invalid run status %q", event.RunStatus)
			}
			initialized.Status = event.RunStatus
		}
		initialized.LastLogSeq = st.LastLogSeq
		initialized.LogChecksum = st.LogChecksum
		*st = initialized
		return nil
	case EventRunStatusSet:
		if event.RunStatus == "" {
			return fmt.Errorf("run_status_set requires runStatus")
		}
		if !event.RunStatus.IsValid() {
			return fmt.Errorf("invalid run status %q", event.RunStatus)
		}
		st.Status = event.RunStatus
		return nil
	case EventNodeAttemptStarted:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.Status == NodeStatusRunning || node.ActiveAttempt != nil {
			return fmt.Errorf("node %q already has an active attempt", event.NodeID)
		}
		attempt := event.Attempt
		if attempt <= 0 {
			attempt = node.Attempt + 1
		}
		if attempt <= node.Attempt {
			return fmt.Errorf("attempt %d for node %q must be greater than current attempt %d", attempt, event.NodeID, node.Attempt)
		}
		node.Status = NodeStatusRunning
		node.Attempt = attempt
		actor := normalizeActor(event.Actor)
		node.Assignee = event.Assignee
		if node.Assignee == "" {
			node.Assignee = string(actor)
		}
		node.ActiveAttempt = &AttemptState{
			Attempt:   attempt,
			Actor:     actor,
			CommandID: event.CommandID,
			StartedAt: event.At,
		}
		st.Nodes[event.NodeID] = node
		return nil
	case EventNodeAttemptSettled:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.ActiveAttempt == nil {
			return fmt.Errorf("node %q has no active attempt", event.NodeID)
		}
		if event.Outcome == "" && event.NodeStatus == "" {
			return fmt.Errorf("node_attempt_settled requires outcome or nodeStatus")
		}
		node.ActiveAttempt.SettledAt = event.At
		node.ActiveAttempt.Outcome = event.Outcome
		status := event.NodeStatus
		if status != "" && !status.IsValid() {
			return fmt.Errorf("invalid node status %q", status)
		}
		if status == "" {
			status = NodeStatusCompleted
			if !isPassOutcome(event.Outcome) {
				status = NodeStatusFailed
			}
		}
		node.Status = status
		st.Nodes[event.NodeID] = node
		return nil
	case EventDecisionRecorded:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.Status == NodeStatusCompleted && (node.ChosenEdge != "" || len(node.Decisions) > 0) {
			return fmt.Errorf("decision node %q is already completed", event.NodeID)
		}
		if node.Type == "" {
			node.Type = model.NodeTypeDecision
		}
		decision := DecisionRecord{}
		if event.Decision != nil {
			decision = *event.Decision
		}
		if decision.Actor == "" {
			decision.Actor = normalizeActor(event.Actor)
		} else {
			decision.Actor = normalizeActor(decision.Actor)
		}
		if decision.Timestamp.IsZero() {
			decision.Timestamp = event.At
		}
		if decision.Verdict == "" {
			decision.Verdict = event.Outcome
		}
		if decision.EvidenceRef == "" {
			decision.EvidenceRef = event.EvidenceRef
		}
		node.Decisions = append(node.Decisions, decision)
		node.ChosenEdge = event.ChosenEdge
		if node.ChosenEdge == "" {
			node.ChosenEdge = decision.Verdict
		}
		node.Status = NodeStatusCompleted
		st.Nodes[event.NodeID] = node
		return nil
	case EventNodeBlocked:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		node.Status = NodeStatusBlocked
		node.BlockedReason = event.Reason
		node.BlockedOwner = event.Owner
		st.Nodes[event.NodeID] = node
		return nil
	case EventNodeUnblocked:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		node.BlockedReason = ""
		node.BlockedOwner = ""
		node.Status = event.NodeStatus
		if node.Status != "" && !node.Status.IsValid() {
			return fmt.Errorf("invalid node status %q", node.Status)
		}
		if node.Status == "" || node.Status == NodeStatusBlocked {
			node.Status = NodeStatusReady
		}
		st.Nodes[event.NodeID] = node
		return nil
	case EventWaitCreated:
		wait := WaitRecord{}
		if event.Wait != nil {
			wait = *event.Wait
		}
		if wait.ID == "" {
			wait.ID = event.WaitID
		}
		if wait.NodeID == "" {
			wait.NodeID = event.NodeID
		}
		if wait.Status == "" {
			wait.Status = WaitStatusPending
		}
		if !wait.Status.IsValid() {
			return fmt.Errorf("invalid wait status %q", wait.Status)
		}
		if !wait.Kind.IsValid() {
			return fmt.Errorf("invalid wait kind %q", wait.Kind)
		}
		if wait.CreatedAt.IsZero() {
			wait.CreatedAt = event.At
		}
		if wait.ID == "" || wait.NodeID == "" {
			return fmt.Errorf("wait_created requires wait id and node id")
		}
		st.Waits[wait.ID] = wait
		node, err := getNode(st, wait.NodeID)
		if err != nil {
			return err
		}
		node.Status = waitingStatus(wait.Kind)
		if wait.Assignee != "" {
			node.Assignee = wait.Assignee
		}
		st.Nodes[wait.NodeID] = node
		return nil
	case EventWaitSatisfied:
		wait, ok := st.Waits[event.WaitID]
		if !ok {
			return fmt.Errorf("wait %q is not declared", event.WaitID)
		}
		wait.Status = WaitStatusSatisfied
		wait.SatisfiedAt = event.At
		st.Waits[event.WaitID] = wait
		node, err := getNode(st, wait.NodeID)
		if err != nil {
			return err
		}
		node.Status = event.NodeStatus
		if node.Status != "" && !node.Status.IsValid() {
			return fmt.Errorf("invalid node status %q", node.Status)
		}
		if node.Status == "" {
			node.Status = NodeStatusReady
		}
		st.Nodes[wait.NodeID] = node
		return nil
	case EventTimerCreated:
		timer := TimerRecord{}
		if event.Timer != nil {
			timer = *event.Timer
		}
		if timer.ID == "" {
			timer.ID = event.TimerID
		}
		if timer.NodeID == "" {
			timer.NodeID = event.NodeID
		}
		if timer.Status == "" {
			timer.Status = WaitStatusPending
		}
		if !timer.Status.IsValid() {
			return fmt.Errorf("invalid timer status %q", timer.Status)
		}
		if timer.CreatedAt.IsZero() {
			timer.CreatedAt = event.At
		}
		if timer.ID == "" || timer.NodeID == "" {
			return fmt.Errorf("timer_created requires timer id and node id")
		}
		st.Timers[timer.ID] = timer
		node, err := getNode(st, timer.NodeID)
		if err != nil {
			return err
		}
		node.Status = NodeStatusWaitingTimer
		st.Nodes[timer.NodeID] = node
		return nil
	case EventTimerSatisfied:
		timer, ok := st.Timers[event.TimerID]
		if !ok {
			return fmt.Errorf("timer %q is not declared", event.TimerID)
		}
		timer.Status = WaitStatusSatisfied
		timer.SatisfiedAt = event.At
		st.Timers[event.TimerID] = timer
		node, err := getNode(st, timer.NodeID)
		if err != nil {
			return err
		}
		node.Status = event.NodeStatus
		if node.Status != "" && !node.Status.IsValid() {
			return fmt.Errorf("invalid node status %q", node.Status)
		}
		if node.Status == "" {
			node.Status = NodeStatusReady
		}
		st.Nodes[timer.NodeID] = node
		return nil
	case EventCommandIssued:
		if event.Command == nil {
			return fmt.Errorf("command_issued requires command")
		}
		command := *event.Command
		if command.Status == "" {
			command.Status = CommandStatusIssued
		}
		if !command.Status.IsValid() {
			return fmt.Errorf("invalid command status %q", command.Status)
		}
		if command.CreatedAt.IsZero() {
			command.CreatedAt = event.At
		}
		if command.ID == "" {
			return fmt.Errorf("command_issued requires command id")
		}
		st.OutstandingCommands[command.ID] = command
		if command.NodeID != "" {
			node, err := getNode(st, command.NodeID)
			if err != nil {
				return err
			}
			if node.ActiveAttempt != nil && node.ActiveAttempt.CommandID == "" {
				node.ActiveAttempt.CommandID = command.ID
				st.Nodes[command.NodeID] = node
			}
		}
		return nil
	case EventCommandObserved:
		command, ok := st.OutstandingCommands[event.CommandID]
		if !ok {
			return fmt.Errorf("command %q is not outstanding", event.CommandID)
		}
		command.Status = CommandStatusObserved
		if event.ExternalRef != "" {
			command.ExternalRef = event.ExternalRef
		}
		st.OutstandingCommands[event.CommandID] = command
		return nil
	case EventAdminRepairRecorded:
		record := AdminRecord{
			Actor:       normalizeActor(event.Actor),
			Reason:      event.Reason,
			EvidenceRef: event.EvidenceRef,
			Timestamp:   event.At,
		}
		st.AdminRecords = append(st.AdminRecords, record)
		if event.RunStatus != "" {
			if !event.RunStatus.IsValid() {
				return fmt.Errorf("invalid run status %q", event.RunStatus)
			}
			st.Status = event.RunStatus
		}
		return nil
	case EventTemplateDivergenceMarked:
		divergence := TemplateDivergence{}
		if event.TemplateDivergence != nil {
			divergence = *event.TemplateDivergence
		}
		if !divergence.Diverged {
			divergence.Diverged = true
		}
		if divergence.At.IsZero() {
			divergence.At = event.At
		}
		if divergence.Actor == "" {
			divergence.Actor = normalizeActor(event.Actor)
		} else {
			divergence.Actor = normalizeActor(divergence.Actor)
		}
		if divergence.Reason == "" {
			divergence.Reason = event.Reason
		}
		st.TemplateDivergence = &divergence
		if event.CurrentTemplateRef != "" {
			st.CurrentTemplateRef = event.CurrentTemplateRef
		}
		return nil
	default:
		return fmt.Errorf("unsupported process state event type %q", event.Type)
	}
}

func getNode(st *State, nodeID string) (NodeState, error) {
	if strings.TrimSpace(nodeID) == "" {
		return NodeState{}, fmt.Errorf("node id is required")
	}
	node, ok := st.Nodes[nodeID]
	if !ok {
		return NodeState{}, fmt.Errorf("node %q is not declared in state", nodeID)
	}
	return node, nil
}

func normalizeActor(actor ActorRef) ActorRef {
	return ActorRef(strings.TrimSpace(string(actor)))
}

func isPassOutcome(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "pass", "passed", "success", "succeeded", "ok", "completed":
		return true
	default:
		return false
	}
}

func waitingStatus(kind WaitKind) NodeStatus {
	switch kind {
	case WaitKindHuman:
		return NodeStatusWaitingHuman
	case WaitKindAgent:
		return NodeStatusWaitingAgent
	case WaitKindProgram:
		return NodeStatusWaitingProgram
	case WaitKindTimer:
		return NodeStatusWaitingTimer
	default:
		return NodeStatusWaitingSignal
	}
}
