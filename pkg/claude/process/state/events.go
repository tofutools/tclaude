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
	EventRunPaused                EventType = "run_paused"
	EventRunResumed               EventType = "run_resumed"
	EventNodeStatusSet            EventType = "node_status_set"
	EventNodeExpanded             EventType = "node_expanded"
	EventNodeAttemptStarted       EventType = "node_attempt_started"
	EventNodeAttemptSettled       EventType = "node_attempt_settled"
	EventFeedbackRecorded         EventType = "feedback_recorded"
	EventGateLoopReset            EventType = "gate_loop_reset"
	EventGateShortCircuited       EventType = "gate_short_circuited"
	EventDecisionRecorded         EventType = "decision_recorded"
	EventNodeBlocked              EventType = "node_blocked"
	EventNodeUnblocked            EventType = "node_unblocked"
	EventBlockResolutionRecorded  EventType = "block_resolution_recorded"
	EventWaitCreated              EventType = "wait_created"
	EventWaitSatisfied            EventType = "wait_satisfied"
	EventTimerCreated             EventType = "timer_created"
	EventTimerSatisfied           EventType = "timer_satisfied"
	EventCommandIssued            EventType = "command_issued"
	EventCommandDispatched        EventType = "command_dispatched"
	EventCommandObserved          EventType = "command_observed"
	EventObligationCreated        EventType = "obligation_created"
	EventObligationResolved       EventType = "obligation_resolved"
	EventContactScheduled         EventType = "contact_scheduled"
	EventContactUpdated           EventType = "contact_updated"
	EventAdminRepairRecorded      EventType = "admin_repair_recorded"
	EventAdminProgramsAllowed     EventType = "admin_programs_allowed"
	EventTemplateDivergenceMarked EventType = "template_divergence_marked"
)

type Event struct {
	Type EventType `json:"type"`
	Seq  int64     `json:"seq,omitempty"`
	At   time.Time `json:"at,omitempty"`

	LogChecksum string `json:"logChecksum,omitempty"`

	RunID               string              `json:"runId,omitempty"`
	RunStatus           RunStatus           `json:"runStatus,omitempty"`
	Pause               *PauseState         `json:"pause,omitempty"`
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

	Command      *OutstandingCommand `json:"command,omitempty"`
	ExternalRef  string              `json:"externalRef,omitempty"`
	Obligation   *ObligationRecord   `json:"obligation,omitempty"`
	ObligationID string              `json:"obligationId,omitempty"`
	Contact      *ContactState       `json:"contact,omitempty"`

	Decision   *DecisionRecord `json:"decision,omitempty"`
	ChosenEdge string          `json:"chosenEdge,omitempty"`

	Wait   *WaitRecord `json:"wait,omitempty"`
	WaitID string      `json:"waitId,omitempty"`

	Timer   *TimerRecord `json:"timer,omitempty"`
	TimerID string       `json:"timerId,omitempty"`

	Reason         string           `json:"reason,omitempty"`
	Owner          string           `json:"owner,omitempty"`
	PoisonedNodeID string           `json:"poisonedNodeId,omitempty"`
	EvidenceRef    string           `json:"evidenceRef,omitempty"`
	Resolution     *BlockResolution `json:"resolution,omitempty"`

	// Gate feedback-loop fields. EvidenceHash is the hash of THIS settle's
	// evidence; WorkEvidenceHash is the work-evidence hash a gate verdict
	// evaluated; FromNodeID names the gate a feedback payload came from;
	// Gates/ResetCounters drive gate_loop_reset re-entry.
	Feedback         string   `json:"feedback,omitempty"`
	EvidenceHash     string   `json:"evidenceHash,omitempty"`
	WorkEvidenceHash string   `json:"workEvidenceHash,omitempty"`
	FromNodeID       string   `json:"fromNodeId,omitempty"`
	Gates            []string `json:"gates,omitempty"`
	ResetCounters    []string `json:"resetCounters,omitempty"`
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
	// Reducer transitions should preserve core invariants by construction:
	// a nil error means the resulting checkpoint is suitable for verify.
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
		if event.RunStatus != RunStatusPaused {
			st.Pause = nil
		}
		return nil
	case EventRunPaused:
		if event.Pause == nil {
			return fmt.Errorf("run_paused requires pause")
		}
		if !event.Pause.Kind.IsValid() {
			return fmt.Errorf("invalid pause kind %q", event.Pause.Kind)
		}
		if strings.TrimSpace(event.Pause.Reason) == "" {
			return fmt.Errorf("run_paused requires reason")
		}
		if event.Pause.Kind == PauseKindRateLimited && event.Pause.Until.IsZero() {
			return fmt.Errorf("rate-limited run pause requires until")
		}
		if strings.TrimSpace(event.Pause.CommandID) == "" {
			return fmt.Errorf("run_paused requires command id")
		}
		if _, ok := st.OutstandingCommands[event.Pause.CommandID]; !ok {
			return fmt.Errorf("run_paused command %q is not outstanding", event.Pause.CommandID)
		}
		if event.Pause.Kind == PauseKindNeedsReconcile && !ValidateActorRef(event.Pause.Owner) {
			return fmt.Errorf("needs-reconcile run pause requires a valid owner")
		}
		pause := *event.Pause
		promoteSchema(st, 2)
		st.Status = RunStatusPaused
		st.Pause = &pause
		return nil
	case EventRunResumed:
		if st.Status != RunStatusPaused || st.Pause == nil {
			return fmt.Errorf("run_resumed requires an engine-paused run")
		}
		st.Status = RunStatusRunning
		st.Pause = nil
		return nil
	case EventNodeStatusSet:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if event.NodeStatus == "" {
			return fmt.Errorf("node_status_set requires nodeStatus")
		}
		if !event.NodeStatus.IsValid() {
			return fmt.Errorf("invalid node status %q", event.NodeStatus)
		}
		switch event.NodeStatus {
		case NodeStatusReady, NodeStatusCompleted:
		default:
			return fmt.Errorf("node_status_set cannot set status %q", event.NodeStatus)
		}
		if node.Parent != "" {
			if err := requirePriorStagesCompleted(st, event.NodeID, node); err != nil {
				return err
			}
		}
		node.Status = event.NodeStatus
		if node.Type == model.NodeTypeDecision && event.NodeStatus == NodeStatusReady && event.Attempt > 0 {
			if strings.TrimSpace(event.PoisonedNodeID) == "" {
				return fmt.Errorf("generation-bound decision activation requires poisonedNodeId")
			}
			node.Attempt = event.Attempt
			node.PoisonedNodeID = event.PoisonedNodeID
		}
		st.Nodes[event.NodeID] = node
		// Completing the done marker IS completing the compound parent: one
		// event, so no checkpoint ever shows a completed done stage under a
		// still-running parent (the invariant treats that shape as forgery).
		if node.Parent != "" && node.Stage == model.StageDone && event.NodeStatus == NodeStatusCompleted {
			parent, err := getNode(st, node.Parent)
			if err != nil {
				return err
			}
			parent.Status = NodeStatusCompleted
			st.Nodes[node.Parent] = parent
		}
		return nil
	case EventNodeExpanded:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if len(node.Children) > 0 {
			return fmt.Errorf("node %q is already expanded", event.NodeID)
		}
		if node.Parent != "" {
			return fmt.Errorf("stage child %q cannot expand", event.NodeID)
		}
		if node.Status != NodeStatusReady {
			return fmt.Errorf("node %q is %s; only ready nodes can expand", event.NodeID, node.Status)
		}
		if len(event.Nodes) < 2 {
			return fmt.Errorf("node_expanded requires at least one work stage and a done stage")
		}
		children := make([]string, 0, len(event.Nodes))
		for i, child := range event.Nodes {
			if !strings.HasPrefix(child.ID, event.NodeID+".") {
				return fmt.Errorf("expanded child id %q must be prefixed with %q", child.ID, event.NodeID+".")
			}
			if _, exists := st.Nodes[child.ID]; exists {
				return fmt.Errorf("expanded child %q is already declared", child.ID)
			}
			if !child.Stage.IsValid() {
				return fmt.Errorf("expanded child %q has invalid stage %q", child.ID, child.Stage)
			}
			if (child.Stage == model.StageTest) != (child.StepID != "") {
				return fmt.Errorf("expanded child %q stage %q and step id %q are inconsistent", child.ID, child.Stage, child.StepID)
			}
			if child.Parent != "" && child.Parent != event.NodeID {
				return fmt.Errorf("expanded child %q parent %q must be %q", child.ID, child.Parent, event.NodeID)
			}
			if isLast := i == len(event.Nodes)-1; (child.Stage == model.StageDone) != isLast {
				return fmt.Errorf("expanded child %q: exactly the last child must be the done stage", child.ID)
			}
			status := NodeStatusPending
			if i == 0 {
				status = NodeStatusReady
			}
			st.Nodes[child.ID] = NodeState{
				Status: status,
				Parent: event.NodeID,
				Stage:  child.Stage,
				StepID: child.StepID,
			}
			children = append(children, child.ID)
		}
		node.Status = NodeStatusRunning
		node.Children = children
		st.Nodes[event.NodeID] = node
		return nil
	case EventNodeAttemptStarted:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.Parent != "" && node.Stage == model.StageDone {
			// The done marker settles automatically with its parent; an attempt
			// on it could complete the done stage without the parent and forge
			// the state expanded_parent_running_after_done exists to catch.
			return fmt.Errorf("done stage %q settles automatically and cannot start attempts", event.NodeID)
		}
		if node.Status == NodeStatusRunning || (node.ActiveAttempt != nil && node.ActiveAttempt.SettledAt.IsZero() && node.ActiveAttempt.Outcome == "") {
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
		// The attempt consumes any pending gate feedback: the planner already
		// threaded the payload onto this attempt's command, and a stale marker
		// would leak into the loop's NEXT window.
		node.PendingFeedback = nil
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
		if event.Actor != "" {
			node.ActiveAttempt.Actor = normalizeActor(event.Actor)
		}
		if event.EvidenceRef != "" {
			node.ActiveAttempt.EvidenceRef = event.EvidenceRef
		}
		if event.EvidenceHash != "" {
			node.ActiveAttempt.EvidenceHash = event.EvidenceHash
		}
		if event.Feedback != "" {
			node.ActiveAttempt.Feedback = event.Feedback
		}
		status := event.NodeStatus
		if status != "" && !status.IsValid() {
			return fmt.Errorf("invalid node status %q", status)
		}
		if status != "" && !canSetNodeStatusDirectly(status) {
			return fmt.Errorf("node_attempt_settled cannot set status %q", status)
		}
		if status == "" {
			status = NodeStatusCompleted
			if !IsPassOutcome(event.Outcome) {
				status = NodeStatusFailed
			}
		}
		// Claimed done is not done: an expanded stage child may only settle as
		// completed when an evidence ref backs the claim; otherwise it flips to
		// failed (design doc section 4).
		if node.Parent != "" && status == NodeStatusCompleted && strings.TrimSpace(node.ActiveAttempt.EvidenceRef) == "" {
			status = NodeStatusFailed
		}
		// Gate verdicts are uniform decision records (design doc section 2),
		// and gates track their loop window: failed verdicts consume the gate
		// budget, and the work-evidence hash the verdict evaluated powers the
		// evidence-unchanged short-circuit.
		if node.Parent != "" && node.Stage.IsGateStage() {
			verdict := "fail"
			if status == NodeStatusCompleted {
				verdict = "pass"
			}
			node.Decisions = append(node.Decisions, DecisionRecord{
				Actor:       node.ActiveAttempt.Actor,
				Verdict:     verdict,
				EvidenceRef: node.ActiveAttempt.EvidenceRef,
				Timestamp:   event.At,
			})
			if status == NodeStatusFailed {
				node.FailCount++
			}
			// Assign unconditionally: LastEvidenceHash must describe what the
			// LATEST verdict evaluated. Preserving a stale hash across a
			// hashless settle would let a later re-entry short-circuit against
			// a verdict that evaluated different, unhashed work (empty is
			// ineligible for short-circuiting).
			node.LastEvidenceHash = event.WorkEvidenceHash
		}
		node.Status = status
		st.Nodes[event.NodeID] = node
		return nil
	case EventFeedbackRecorded:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.Parent == "" || (node.Stage != model.StagePlan && node.Stage != model.StageDo) {
			return fmt.Errorf("feedback_recorded target %q must be a plan or do stage child", event.NodeID)
		}
		from, err := getNode(st, event.FromNodeID)
		if err != nil {
			return err
		}
		if from.Parent != node.Parent || !from.Stage.IsGateStage() {
			return fmt.Errorf("feedback_recorded source %q must be a sibling gate of %q", event.FromNodeID, event.NodeID)
		}
		node.PendingFeedback = &FeedbackRef{
			FromNodeID:  event.FromNodeID,
			FromAttempt: from.Attempt,
			Feedback:    event.Feedback,
			EvidenceRef: event.EvidenceRef,
			At:          event.At,
		}
		st.Nodes[event.NodeID] = node
		return nil
	case EventGateLoopReset:
		parent, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if len(parent.Children) == 0 {
			return fmt.Errorf("gate_loop_reset parent %q is not expanded", event.NodeID)
		}
		if len(event.Gates) == 0 {
			return fmt.Errorf("gate_loop_reset requires gates to reset")
		}
		for _, gateID := range event.Gates {
			gate, err := getNode(st, gateID)
			if err != nil {
				return err
			}
			if gate.Parent != event.NodeID || !gate.Stage.IsGateStage() {
				return fmt.Errorf("gate_loop_reset %q is not a gate child of %q", gateID, event.NodeID)
			}
			switch gate.Status {
			case NodeStatusPending, NodeStatusCompleted, NodeStatusFailed:
			default:
				return fmt.Errorf("gate_loop_reset %q is %s; only settled or pending gates re-enter", gateID, gate.Status)
			}
			gate.Status = NodeStatusPending
			st.Nodes[gateID] = gate
		}
		for _, gateID := range event.ResetCounters {
			gate, err := getNode(st, gateID)
			if err != nil {
				return err
			}
			if gate.Parent != event.NodeID || !gate.Stage.IsGateStage() {
				return fmt.Errorf("gate_loop_reset counter %q is not a gate child of %q", gateID, event.NodeID)
			}
			gate.FailCount = 0
			st.Nodes[gateID] = gate
		}
		return nil
	case EventGateShortCircuited:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.Parent == "" || !node.Stage.IsGateStage() {
			return fmt.Errorf("gate_short_circuited node %q is not a gate stage child", event.NodeID)
		}
		if node.Status != NodeStatusReady && node.Status != NodeStatusPending {
			return fmt.Errorf("gate_short_circuited node %q is %s; only a re-entering gate can short-circuit", event.NodeID, node.Status)
		}
		if len(node.Decisions) == 0 {
			return fmt.Errorf("gate_short_circuited node %q has no prior verdict to stand", event.NodeID)
		}
		prior := node.Decisions[len(node.Decisions)-1]
		if event.EvidenceHash == "" || node.LastEvidenceHash == "" || event.EvidenceHash != node.LastEvidenceHash {
			return fmt.Errorf("gate_short_circuited node %q evidence hash does not match the prior verdict's", event.NodeID)
		}
		actor := normalizeActor(event.Actor)
		if !strings.HasPrefix(string(actor), "engine:") {
			return fmt.Errorf("gate_short_circuited requires an engine actor, got %q", actor)
		}
		node.Decisions = append(node.Decisions, DecisionRecord{
			Actor:       actor,
			Verdict:     prior.Verdict,
			EvidenceRef: prior.EvidenceRef,
			Timestamp:   event.At,
		})
		node.Status = NodeStatusCompleted
		if !IsPassOutcome(prior.Verdict) {
			node.Status = NodeStatusFailed
			node.FailCount++
		}
		st.Nodes[event.NodeID] = node
		return nil
	case EventDecisionRecorded:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		if node.ChosenEdge != "" || len(node.Decisions) > 0 {
			return fmt.Errorf("decision node %q is already decided", event.NodeID)
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
		if node.Attempt > 0 && node.PoisonedNodeID != "" && strings.TrimSpace(decision.EvidenceRef) == "" {
			return fmt.Errorf("poison escalation decision %q requires an evidence reference", event.NodeID)
		}
		if event.ChosenEdge != "" && decision.Verdict != "" && event.ChosenEdge != decision.Verdict {
			return fmt.Errorf("decision node %q chosenEdge %q must match verdict %q", event.NodeID, event.ChosenEdge, decision.Verdict)
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
		if strings.TrimSpace(event.Reason) == "" || strings.TrimSpace(event.Owner) == "" {
			return fmt.Errorf("node_blocked requires reason and owner")
		}
		if event.At.IsZero() {
			return fmt.Errorf("node_blocked requires timestamp")
		}
		blockedAttempt := event.Attempt
		if blockedAttempt <= 0 {
			blockedAttempt = node.Attempt
		}
		// The resolution is a generation tombstone. A delayed block for that
		// attempt (or an older one) is idempotent success, not permission to
		// undo an explicit retry/skip/cancel decision.
		blockedNodeID := strings.TrimSpace(event.FromNodeID)
		if blockedNodeID == "" {
			blockedNodeID = event.NodeID
		}
		if node.BlockResolution != nil && node.BlockResolution.NodeID == blockedNodeID && blockedAttempt <= node.BlockResolution.BlockedAttempt {
			return nil
		}
		if node.Status == NodeStatusBlocked && node.BlockedNodeID == blockedNodeID && node.BlockedAttempt == blockedAttempt {
			return nil
		}
		node.Status = NodeStatusBlocked
		node.BlockedReason = event.Reason
		node.BlockedOwner = event.Owner
		node.BlockedAt = event.At.UTC()
		node.BlockedAtUnavailable = false
		node.BlockedAttempt = blockedAttempt
		node.BlockedNodeID = blockedNodeID
		node.BlockResolution = nil
		promoteSchema(st, StateSchemaVersion)
		st.Nodes[event.NodeID] = node
		return nil
	case EventNodeUnblocked:
		node, err := getNode(st, event.NodeID)
		if err != nil {
			return err
		}
		resolution, err := normalizedBlockResolution(event.Resolution, event.At)
		if err != nil {
			return fmt.Errorf("node_unblocked: %w", err)
		}
		if node.BlockResolution != nil {
			if *node.BlockResolution == resolution {
				pauseResolvedBlockContacts(st, resolution.NodeID)
				return nil
			}
			return fmt.Errorf("node %q was already unblocked with a different resolution", event.NodeID)
		}
		if node.Status != NodeStatusBlocked {
			return fmt.Errorf("node %q is %s; only blocked nodes can be unblocked", event.NodeID, node.Status)
		}
		// A zero generation is accepted for state replayed from pre-v5 block
		// events, which did not carry the child's attempt on the parent mirror.
		if node.BlockedAttempt > 0 && node.BlockedAttempt != resolution.BlockedAttempt {
			return fmt.Errorf("node %q blocked attempt %d does not match resolution attempt %d", event.NodeID, node.BlockedAttempt, resolution.BlockedAttempt)
		}
		if node.BlockedNodeID != "" && node.BlockedNodeID != resolution.NodeID {
			return fmt.Errorf("node %q mirrors blocked child %q, not resolution child %q", event.NodeID, node.BlockedNodeID, resolution.NodeID)
		}
		expectedStatus := NodeStatusRunning
		if node.Parent != "" || len(node.Children) == 0 {
			expectedStatus = NodeStatusSkipped
			if resolution.Decision == BlockDecisionRetry {
				expectedStatus = NodeStatusReady
			}
		} else if !nodeListsChild(node, resolution.NodeID) {
			return fmt.Errorf("node %q does not mirror resolution child %q", event.NodeID, resolution.NodeID)
		}
		if event.NodeStatus != expectedStatus {
			return fmt.Errorf("node_unblocked decision %q requires nodeStatus %q for node %q", resolution.Decision, expectedStatus, event.NodeID)
		}
		node.BlockedReason = ""
		node.BlockedOwner = ""
		node.Status = event.NodeStatus
		if node.Status != "" && !node.Status.IsValid() {
			return fmt.Errorf("invalid node status %q", node.Status)
		}
		if node.Status == "" || node.Status == NodeStatusBlocked {
			return fmt.Errorf("node_unblocked requires a non-blocked nodeStatus")
		}
		node.BlockedAttempt = resolution.BlockedAttempt
		node.BlockedNodeID = resolution.NodeID
		node.BlockResolution = &resolution
		pauseResolvedBlockContacts(st, resolution.NodeID)
		if resolution.Decision == BlockDecisionRetry {
			// A retry is an explicit fresh budget window. In particular, a gate
			// must not immediately short-circuit against the same evidence and
			// recreate the poison without invoking its performer.
			node.FailCount = 0
			node.LastEvidenceHash = ""
		}
		blockSchemaVersion := 5
		if !node.BlockedAt.IsZero() {
			blockSchemaVersion = StateSchemaVersion
		}
		promoteSchema(st, blockSchemaVersion)
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
		if strings.TrimSpace(command.ID) == "" {
			return fmt.Errorf("command_issued requires command id")
		}
		if !command.Kind.IsValid() {
			return fmt.Errorf("invalid command kind %q", command.Kind)
		}
		if command.Status == "" {
			command.Status = CommandStatusIssued
		}
		if !command.Status.IsValid() {
			return fmt.Errorf("invalid command status %q", command.Status)
		}
		if command.Status != CommandStatusIssued {
			return fmt.Errorf("command_issued requires issued status")
		}
		if command.Attempt < 0 {
			return fmt.Errorf("command_issued attempt must be non-negative")
		}
		if command.CreatedAt.IsZero() {
			command.CreatedAt = event.At
		}
		if existing, exists := st.OutstandingCommands[command.ID]; exists && commandIsActive(existing) {
			return fmt.Errorf("command %q is already outstanding", command.ID)
		}
		if command.NodeID != "" {
			node, err := getNode(st, command.NodeID)
			if err != nil {
				return err
			}
			if command.Kind == CommandKindStartAttempt && node.ActiveAttempt != nil && node.ActiveAttempt.CommandID == "" {
				node.ActiveAttempt.CommandID = command.ID
				st.Nodes[command.NodeID] = node
			}
		}
		st.OutstandingCommands[command.ID] = command
		return nil
	case EventCommandObserved:
		if strings.TrimSpace(event.CommandID) == "" {
			return fmt.Errorf("command_observed requires command id")
		}
		command, ok := st.OutstandingCommands[event.CommandID]
		if !ok {
			return fmt.Errorf("command %q is not outstanding", event.CommandID)
		}
		if command.Status != CommandStatusIssued {
			return fmt.Errorf("command %q is %s and cannot be observed", event.CommandID, command.Status)
		}
		command.Status = CommandStatusObserved
		if event.ExternalRef != "" {
			command.ExternalRef = event.ExternalRef
		}
		if event.Actor != "" {
			command.Actor = normalizeActor(event.Actor)
		}
		if event.Outcome != "" {
			command.Verdict = event.Outcome
		}
		if event.EvidenceRef != "" {
			command.EvidenceRef = event.EvidenceRef
		}
		if event.EvidenceHash != "" {
			command.EvidenceHash = event.EvidenceHash
		}
		if event.Feedback != "" {
			command.Feedback = event.Feedback
		}
		st.OutstandingCommands[event.CommandID] = command
		return nil
	case EventCommandDispatched:
		if strings.TrimSpace(event.CommandID) == "" || strings.TrimSpace(event.ExternalRef) == "" {
			return fmt.Errorf("command_dispatched requires command id and external ref")
		}
		command, ok := st.OutstandingCommands[event.CommandID]
		if !ok {
			return fmt.Errorf("command %q is not outstanding", event.CommandID)
		}
		if command.Status != CommandStatusIssued {
			return fmt.Errorf("command %q is %s and cannot be dispatched", event.CommandID, command.Status)
		}
		if command.ExternalRef != "" && command.ExternalRef != event.ExternalRef {
			return fmt.Errorf("command %q is already dispatched as %q", event.CommandID, command.ExternalRef)
		}
		command.ExternalRef = event.ExternalRef
		st.OutstandingCommands[event.CommandID] = command
		return nil
	case EventObligationCreated:
		if event.Obligation == nil {
			return fmt.Errorf("obligation_created requires obligation")
		}
		obligation := *event.Obligation
		if obligation.ID == "" || obligation.RunID == "" || obligation.NodeID == "" || obligation.CommandID == "" {
			return fmt.Errorf("obligation_created requires id, run, node, and command")
		}
		if obligation.RunID != st.RunID {
			return fmt.Errorf("obligation %q run %q does not match state run %q", obligation.ID, obligation.RunID, st.RunID)
		}
		if !obligation.Kind.IsValid() || !obligation.Status.IsValid() {
			return fmt.Errorf("obligation %q has invalid kind or status", obligation.ID)
		}
		if strings.TrimSpace(obligation.Assignee) == "" || strings.TrimSpace(obligation.Summary) == "" {
			return fmt.Errorf("obligation %q requires assignee and summary", obligation.ID)
		}
		if _, ok := st.OutstandingCommands[obligation.CommandID]; !ok {
			return fmt.Errorf("obligation %q command %q is not outstanding", obligation.ID, obligation.CommandID)
		}
		if obligation.CreatedAt.IsZero() {
			obligation.CreatedAt = event.At
		}
		obligation.AvailableActions = append([]string(nil), obligation.AvailableActions...)
		promoteSchema(st, 4)
		st.Obligations[obligation.ID] = obligation
		node, err := getNode(st, obligation.NodeID)
		if err != nil {
			return err
		}
		node.Assignee = obligation.Assignee
		node.Status = waitingStatus(obligation.Kind)
		st.Nodes[obligation.NodeID] = node
		return nil
	case EventObligationResolved:
		id := event.ObligationID
		if id == "" && event.Obligation != nil {
			id = event.Obligation.ID
		}
		obligation, ok := st.Obligations[id]
		if !ok {
			return fmt.Errorf("obligation %q is not declared", id)
		}
		obligation.Status = WaitStatusSatisfied
		promoteSchema(st, 4)
		obligation.ResolvedAt = event.At
		if event.EvidenceRef != "" {
			obligation.EvidenceRef = event.EvidenceRef
		}
		st.Obligations[id] = obligation
		node, err := getNode(st, obligation.NodeID)
		if err != nil {
			return err
		}
		if node.ActiveAttempt != nil && node.ActiveAttempt.SettledAt.IsZero() {
			node.Status = NodeStatusRunning
			st.Nodes[obligation.NodeID] = node
		}
		return nil
	case EventContactScheduled, EventContactUpdated:
		if event.Contact == nil {
			return fmt.Errorf("%s requires contact", event.Type)
		}
		contact := *event.Contact
		if contact.CommandID == "" || !contact.Kind.IsValid() || strings.TrimSpace(contact.Cadence) == "" || contact.Budget <= 0 || strings.TrimSpace(contact.EscalationTarget) == "" {
			return fmt.Errorf("contact for command %q is incomplete", contact.CommandID)
		}
		command, ok := st.OutstandingCommands[contact.CommandID]
		if !ok {
			return fmt.Errorf("contact command %q is not outstanding", contact.CommandID)
		}
		if command.Kind == CommandKindBlockNode {
			kind, typed := ContactKindForOwner(contact.Assignee)
			if !typed || kind != WaitKindHuman || contact.Kind != WaitKindHuman {
				return fmt.Errorf("blocked contact command %q requires a human/role owner", contact.CommandID)
			}
		}
		if contact.Used < 0 || contact.Used > contact.Budget {
			return fmt.Errorf("contact command %q budget usage %d/%d is invalid", contact.CommandID, contact.Used, contact.Budget)
		}
		promoteSchema(st, 4)
		st.Contacts[contact.CommandID] = contact
		return nil
	case EventAdminRepairRecorded, EventAdminProgramsAllowed, EventBlockResolutionRecorded:
		var resolution *BlockResolution
		if event.Type == EventBlockResolutionRecorded {
			normalized, err := normalizedBlockResolution(event.Resolution, event.At)
			if err != nil {
				return fmt.Errorf("block_resolution_recorded: %w", err)
			}
			resolution = &normalized
		}
		record := AdminRecord{
			Type:        event.Type,
			Actor:       normalizeActor(event.Actor),
			Reason:      event.Reason,
			EvidenceRef: event.EvidenceRef,
			Timestamp:   event.At,
			Resolution:  resolution,
		}
		if resolution != nil {
			record.Actor = resolution.Actor
			record.Reason = resolution.Reason
			record.EvidenceRef = resolution.EvidenceRef
			record.Timestamp = resolution.Timestamp
		}
		st.AdminRecords = append(st.AdminRecords, record)
		if event.RunStatus != "" {
			if !event.RunStatus.IsValid() {
				return fmt.Errorf("invalid run status %q", event.RunStatus)
			}
			st.Status = event.RunStatus
			if event.RunStatus != RunStatusPaused {
				st.Pause = nil
			}
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

func pauseResolvedBlockContacts(st *State, blockedNodeID string) {
	for commandID, contact := range st.Contacts {
		command, ok := st.OutstandingCommands[commandID]
		if !ok || command.Kind != CommandKindBlockNode || command.NodeID != blockedNodeID {
			continue
		}
		contact.Paused = true
		contact.PauseReason = "block resolved"
		contact.NextContactAt = time.Time{}
		st.Contacts[commandID] = contact
	}
}

func promoteSchema(st *State, version int) {
	if st.StateSchemaVersion < 6 && version >= 6 {
		for nodeID, node := range st.Nodes {
			if node.BlockedAt.IsZero() && (node.Status == NodeStatusBlocked || node.BlockResolution != nil) {
				node.BlockedAtUnavailable = true
				st.Nodes[nodeID] = node
			}
		}
	}
	if st.StateSchemaVersion < version {
		st.StateSchemaVersion = version
	}
}

// requirePriorStagesCompleted enforces the stage chain on activation: a stage
// child may only be set ready or completed once every earlier sibling has
// completed. This holds for retry loops too, which re-activate a stage whose
// earlier siblings already completed.
func requirePriorStagesCompleted(st *State, childID string, child NodeState) error {
	parent, ok := st.Nodes[child.Parent]
	if !ok {
		return fmt.Errorf("stage child %q references undeclared parent %q", childID, child.Parent)
	}
	for _, siblingID := range parent.Children {
		if siblingID == childID {
			return nil
		}
		sibling, ok := st.Nodes[siblingID]
		if !ok || (sibling.Status != NodeStatusCompleted && sibling.Status != NodeStatusSkipped) {
			return fmt.Errorf("stage child %q cannot activate before earlier stage %q completes", childID, siblingID)
		}
	}
	return fmt.Errorf("stage child %q is not listed in parent %q children", childID, child.Parent)
}

func normalizedBlockResolution(input *BlockResolution, at time.Time) (BlockResolution, error) {
	if input == nil {
		return BlockResolution{}, fmt.Errorf("block resolution is required")
	}
	resolution := *input
	resolution.NodeID = strings.TrimSpace(resolution.NodeID)
	resolution.Actor = normalizeActor(resolution.Actor)
	resolution.Reason = strings.TrimSpace(resolution.Reason)
	resolution.EvidenceRef = strings.TrimSpace(resolution.EvidenceRef)
	if resolution.Timestamp.IsZero() {
		resolution.Timestamp = at
	}
	if resolution.Timestamp.IsZero() {
		return BlockResolution{}, fmt.Errorf("block resolution requires timestamp")
	}
	if resolution.NodeID == "" || resolution.BlockedAttempt <= 0 {
		return BlockResolution{}, fmt.Errorf("block resolution requires node id and blocked attempt")
	}
	if !resolution.Decision.IsValid() {
		return BlockResolution{}, fmt.Errorf("invalid block decision %q", resolution.Decision)
	}
	if !ValidateActorRef(resolution.Actor) || IsEngineActor(resolution.Actor) {
		return BlockResolution{}, fmt.Errorf("block resolution requires a non-engine actor")
	}
	if resolution.Reason == "" || resolution.EvidenceRef == "" {
		return BlockResolution{}, fmt.Errorf("block resolution requires reason and evidence ref")
	}
	return resolution, nil
}

func nodeListsChild(node NodeState, childID string) bool {
	for _, candidate := range node.Children {
		if candidate == childID {
			return true
		}
	}
	return false
}

func commandIsActive(command OutstandingCommand) bool {
	return command.Status == CommandStatusIssued || command.Status == CommandStatusObserved
}

func canSetNodeStatusDirectly(status NodeStatus) bool {
	switch status {
	case NodeStatusReady, NodeStatusCompleted, NodeStatusFailed:
		return true
	default:
		return false
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

func IsPassOutcome(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "pass", "passed", "success", "succeeded", "ok", "done", "completed":
		return true
	default:
		return false
	}
}

func IsFailOutcome(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "fail", "failed", "failure", "error", "cancel", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func SettleNodeStatus(outcome string, attempt int, retry *model.RetryPolicy) NodeStatus {
	if IsPassOutcome(outcome) {
		return NodeStatusCompleted
	}
	maxAttempts := 1
	if retry != nil && retry.MaxAttempts > 0 {
		maxAttempts = retry.MaxAttempts
	}
	if attempt > 0 && attempt < maxAttempts {
		return NodeStatusReady
	}
	return NodeStatusFailed
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
