// Package worklist derives explicit process obligations from durable run
// snapshots. It does not persist state or perform transitions.
package worklist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type Kind string

const (
	KindHumanWait       Kind = "human-wait"
	KindDecisionNeeded  Kind = "decision-needed"
	KindReviewNeeded    Kind = "review-needed"
	KindBlocked         Kind = "blocked"
	KindAgentObligation Kind = "agent-obligation"
	KindWaiting         Kind = "waiting"
)

type Nudge struct {
	LastContactAt    time.Time `json:"lastContactAt,omitzero"`
	NextContactAt    time.Time `json:"nextContactAt,omitzero"`
	BudgetUsed       int       `json:"budgetUsed"`
	BudgetMax        int       `json:"budgetMax"`
	EscalationTarget string    `json:"escalationTarget"`
	Paused           bool      `json:"paused"`
}

type Links struct {
	RunID        string   `json:"runId"`
	NodeID       string   `json:"nodeId"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

type ActionTarget struct {
	CommandID string `json:"-"`
	Blocked   bool   `json:"-"`
}

type Item struct {
	ID       string           `json:"id"`
	Run      string           `json:"run"`
	Node     string           `json:"node"`
	Attempt  int              `json:"attempt"`
	Kind     Kind             `json:"kind"`
	Assignee string           `json:"assignee"`
	Status   state.WaitStatus `json:"status"`
	// CreatedAt/DueAt come straight off the durable records; ChangedAt is the
	// item's last state change (resolution when resolved, else creation) and
	// drives the dashboard's bounded "Recently changed" view. All omitzero so
	// pre-v6 blocked records remain honest instead of fabricating history.
	CreatedAt        time.Time    `json:"createdAt,omitzero"`
	DueAt            time.Time    `json:"dueAt,omitzero"`
	ChangedAt        time.Time    `json:"changedAt,omitzero"`
	Nudge            *Nudge       `json:"nudge,omitempty"`
	Summary          string       `json:"summary"`
	Detached         bool         `json:"detached,omitempty"`
	DetachmentCount  int          `json:"detachmentCount,omitempty"`
	AvailableActions []string     `json:"availableActions,omitempty"`
	Links            Links        `json:"links"`
	Target           ActionTarget `json:"-"`
}

type Filter struct {
	Assignee string
	Kind     Kind
	Run      string
	Status   state.WaitStatus
}

// CommandAssigneeLookup resolves the live agent assigned to a schema-7
// external command. Nil is valid for store-only callers that do not have the
// daemon's agent registry available.
type CommandAssigneeLookup func(context.Context, string) (string, error)

// Derive projects snapshots into a stable, deterministically ordered worklist.
func Derive(snapshots []store.Snapshot) []Item {
	items := make([]Item, 0)
	for _, snapshot := range snapshots {
		if snapshot.State == nil {
			continue
		}
		items = append(items, obligationItems(snapshot)...)
		items = append(items, blockedItems(snapshot)...)
	}
	slices.SortFunc(items, func(a, b Item) int {
		return strings.Compare(a.ID, b.ID)
	})
	return items
}

// DerivePathV1 projects one verified schema-7 checkpoint into the same stable
// worklist contract used for legacy snapshots. The checkpoint is the only
// execution authority; template source is used solely for immutable node and
// performer metadata.
func DerivePathV1(ctx context.Context, snapshot store.PathV1RunSnapshot, lookup CommandAssigneeLookup) ([]Item, error) {
	if _, err := pathv1.VerifyExecutionInput(ctx, snapshot.CheckpointJSON, snapshot.TemplateSource); err != nil {
		return nil, err
	}
	parsed, err := model.ParseExactSource(snapshot.TemplateSource)
	if err != nil || parsed.Diagnostics.HasErrors() {
		return nil, fmt.Errorf("schema-7 worklist template is invalid")
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0)
	coveredEffects := make(map[string]struct{})
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != pathv1.CommandPerformAttempt {
			continue
		}
		status := state.WaitStatusPending
		if command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled {
			status = state.WaitStatusSatisfied
		} else if !command.State.Active() {
			continue
		}
		work, contextErr := pathV1WorkContext(&aggregate.Routing, parsed.Template, command.Identity.SourceActivationID)
		if contextErr != nil {
			return nil, contextErr
		}
		node := work.node
		if node.Type == model.NodeTypeWait {
			item, effectID, buildErr := buildPathV1WaitItem(snapshot.Run.ID, aggregate, work, command, status)
			if buildErr != nil {
				return nil, buildErr
			}
			coveredEffects[effectID] = struct{}{}
			items = append(items, item)
			continue
		}
		if node.Performer == nil || (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) {
			continue
		}
		performer := model.InterpolatePerformer(*node.Performer, snapshot.Run.Params)
		item, buildErr := buildPathV1Item(ctx, snapshot.Run.ID, parsed.Template, work, &performer, command, status, lookup)
		if buildErr != nil {
			return nil, buildErr
		}
		items = append(items, item)
	}
	for _, effect := range aggregate.SideEffects {
		if _, covered := coveredEffects[effect.ID]; covered {
			continue
		}
		if effect.Kind != pathv1.SideEffectObligation && effect.Kind != pathv1.SideEffectBlock {
			continue
		}
		work, contextErr := pathV1WorkContext(&aggregate.Routing, parsed.Template, effect.ActivationID)
		if contextErr != nil {
			return nil, contextErr
		}
		item := buildPathV1SideEffectItem(snapshot.Run.ID, work, effect)
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
	return items, nil
}

type pathV1WorkContextRecord struct {
	activation      pathv1.ActivationRecord
	reservation     pathv1.ActivationReservation
	node            model.Node
	detachmentCount int
}

func pathV1WorkContext(routing *pathv1.RoutingState, tmpl *model.Template, activationID string) (pathV1WorkContextRecord, error) {
	if routing == nil || tmpl == nil {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist routing authority is absent")
	}
	activation, ok := routing.Activations[activationID]
	if !ok {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist activation is absent")
	}
	reservation, ok := routing.Reservations[activation.ReservationID]
	if !ok {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist reservation is absent")
	}
	node, ok := tmpl.Nodes[reservation.NodeID]
	if !ok {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist node is absent from the exact template")
	}
	output, ok := routing.Paths[activation.OutputPathID]
	if !ok && activation.OutputPathID != "" {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist activation output is absent")
	}
	detachments, err := pathv1.VerifyDetachmentSet(routing, output.DetachmentSetID)
	if err != nil {
		return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist detachment set: %w", err)
	}
	detachmentIDs := make(map[pathv1.DetachmentID]struct{}, len(detachments))
	for _, detachmentID := range detachments {
		detachmentIDs[detachmentID] = struct{}{}
	}
	// A live parallel-any loser has not emitted a detached sink yet, so its
	// output path may not carry a materialized detachment set. Its immutable
	// candidate lineage still binds it to the reservation-relative detachment.
	for _, frame := range output.CandidateLineage {
		key, identityErr := pathv1.DetachmentKeyIdentity(frame.ReservationID, frame.CandidateID)
		if identityErr != nil {
			return pathV1WorkContextRecord{}, fmt.Errorf("schema-7 worklist detachment key: %w", identityErr)
		}
		if detachment, exists := routing.Detachments[key]; exists {
			detachmentIDs[detachment.ID] = struct{}{}
		}
	}
	return pathV1WorkContextRecord{activation: activation, reservation: reservation, node: node, detachmentCount: len(detachmentIDs)}, nil
}

func buildPathV1Item(ctx context.Context, runID string, tmpl *model.Template, work pathV1WorkContextRecord, performer *model.Performer, command pathv1.CommandRecord, status state.WaitStatus, lookup CommandAssigneeLookup) (Item, error) {
	if performer == nil || tmpl == nil || len(command.ID) < 24 {
		return Item{}, fmt.Errorf("schema-7 worklist command authority is invalid")
	}
	nodeID := work.reservation.NodeID
	node := tmpl.Nodes[nodeID]
	commandID := "cmd_" + command.ID[:24]
	kind := KindAgentObligation
	assignee := ""
	summary := strings.TrimSpace(performer.Prompt)
	actions := []string{"pass", "fail", "ask-changes"}
	if performer.Kind == model.PerformerHuman {
		kind = KindHumanWait
		assignee = strings.TrimSpace(performer.Assignee)
		if assignee == "" {
			assignee = strings.TrimSpace(performer.Profile)
		}
		if assignee == "" {
			assignee = "human:operator"
		} else if !strings.HasPrefix(assignee, "human:") && !strings.HasPrefix(assignee, "role:") {
			assignee = "human:" + assignee
		}
		summary = strings.TrimSpace(performer.Ask)
		if summary == "" {
			summary = strings.TrimSpace(performer.Prompt)
		}
		actions = []string{"approve", "reject", "ask-changes"}
		if node.Type == model.NodeTypeDecision {
			kind = KindDecisionNeeded
			actions = make([]string, 0, len(node.Next))
			for outcome := range node.Next {
				actions = append(actions, outcome)
			}
			slices.Sort(actions)
		} else if len(performer.Choices) > 0 {
			actions = append([]string(nil), performer.Choices...)
		}
	} else if lookup != nil {
		resolved, lookupErr := lookup(ctx, commandID)
		if lookupErr != nil {
			return Item{}, lookupErr
		}
		assignee = strings.TrimSpace(resolved)
	}
	attemptNumber := int(command.Identity.Attempt)
	return Item{
		ID: stableID(runID, nodeID, commandID, attemptNumber), Run: runID, Node: nodeID, Attempt: attemptNumber,
		Kind: kind, Assignee: assignee, Status: status, Summary: summary,
		Detached: work.detachmentCount > 0, DetachmentCount: work.detachmentCount,
		AvailableActions: actions, Links: Links{RunID: runID, NodeID: nodeID},
		Target: ActionTarget{CommandID: commandID},
	}, nil
}

type pathV1WaitPayload struct {
	TemplateRef        string `json:"templateRef"`
	TemplateSourceHash string `json:"templateSourceHash"`
	NodeID             string `json:"nodeId"`
	SourceActivationID string `json:"sourceActivationId"`
	SourceGeneration   uint64 `json:"sourceGeneration"`
	Attempt            uint64 `json:"attempt"`
	WaitKind           string `json:"waitKind"`
	Signal             string `json:"signal,omitempty"`
	ScheduledAt        string `json:"scheduledAt,omitempty"`
	DueAt              string `json:"dueAt,omitempty"`
}

func buildPathV1WaitItem(runID string, aggregate pathv1.AggregateCheckpoint, work pathV1WorkContextRecord, command pathv1.CommandRecord, commandStatus state.WaitStatus) (Item, string, error) {
	var payload pathV1WaitPayload
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Item{}, "", fmt.Errorf("schema-7 worklist wait payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Item{}, "", fmt.Errorf("schema-7 worklist wait payload has trailing data")
	}
	if payload.TemplateRef != aggregate.TemplateRef || payload.TemplateSourceHash != aggregate.TemplateSourceHash ||
		payload.NodeID != work.reservation.NodeID || payload.SourceActivationID != work.activation.ID ||
		payload.SourceGeneration != work.activation.Ref.Generation || payload.Attempt != command.Identity.Attempt {
		return Item{}, "", fmt.Errorf("schema-7 worklist wait payload binding is invalid")
	}
	effectID, err := pathv1.WaitIdentity(runID, work.activation.ID, command.Identity.Attempt, payload.WaitKind)
	if err != nil {
		return Item{}, "", err
	}
	effect, ok := aggregate.SideEffects[effectID]
	if !ok || effect.Kind != pathv1.SideEffectWait {
		return Item{}, "", fmt.Errorf("schema-7 worklist wait side effect is absent")
	}
	status := pathV1EffectWaitStatus(effect.State)
	if status == "" || status != commandStatus {
		return Item{}, "", fmt.Errorf("schema-7 worklist wait state is inconsistent")
	}
	var summary string
	var dueAt time.Time
	switch payload.WaitKind {
	case "signal":
		if payload.Signal == "" || work.node.Wait == nil || payload.Signal != strings.TrimSpace(work.node.Wait.Signal) {
			return Item{}, "", fmt.Errorf("schema-7 worklist signal wait is inconsistent")
		}
		summary = "Waiting for signal " + payload.Signal
	case "duration", "until":
		dueAt, err = time.Parse(time.RFC3339Nano, payload.DueAt)
		if err != nil || dueAt.IsZero() {
			return Item{}, "", fmt.Errorf("schema-7 worklist timer due time is invalid")
		}
		summary = "Waiting until " + dueAt.UTC().Format(time.RFC3339)
	default:
		return Item{}, "", fmt.Errorf("schema-7 worklist wait kind is unsupported")
	}
	return Item{
		ID:  stableID(runID, work.reservation.NodeID, effectID, int(command.Identity.Attempt)),
		Run: runID, Node: work.reservation.NodeID, Attempt: int(command.Identity.Attempt), Kind: KindWaiting,
		Status: status, DueAt: dueAt, Summary: summary,
		Detached: work.detachmentCount > 0, DetachmentCount: work.detachmentCount,
		Links: Links{RunID: runID, NodeID: work.reservation.NodeID},
	}, effectID, nil
}

func buildPathV1SideEffectItem(runID string, work pathV1WorkContextRecord, effect pathv1.SideEffectIdentity) Item {
	status := pathV1EffectWaitStatus(effect.State)
	kind := KindHumanWait
	summary := "Waiting for " + effect.WaitKind
	attempt := int(effect.Attempt)
	assignee := effect.Assignee
	if effect.Kind == pathv1.SideEffectBlock {
		kind = KindBlocked
		attempt = int(effect.BlockedAttempt)
		assignee = ""
		summary = "Blocked at checkpoint; resolution detail unavailable"
		if effect.State == "blocked" {
			status = state.WaitStatusPending
		} else {
			status = state.WaitStatusSatisfied
		}
	}
	return Item{
		ID: stableID(runID, work.reservation.NodeID, effect.ID, attempt), Run: runID,
		Node: work.reservation.NodeID, Attempt: attempt, Kind: kind, Assignee: assignee,
		Status: status, Summary: summary,
		Detached: work.detachmentCount > 0, DetachmentCount: work.detachmentCount,
		Links: Links{RunID: runID, NodeID: work.reservation.NodeID},
	}
}

func pathV1EffectWaitStatus(value string) state.WaitStatus {
	switch value {
	case "pending", "blocked":
		return state.WaitStatusPending
	case "satisfied", "resolved_retry", "resolved_skip", "resolved_cancel":
		return state.WaitStatusSatisfied
	case "canceled":
		return state.WaitStatusCanceled
	default:
		return ""
	}
}

// ApplyFilter applies exact-match REST/CLI filters without changing order.
func ApplyFilter(items []Item, filter Filter) []Item {
	filtered := make([]Item, 0, len(items))
	for _, item := range items {
		if filter.Assignee != "" && item.Assignee != filter.Assignee ||
			filter.Kind != "" && item.Kind != filter.Kind ||
			filter.Run != "" && item.Run != filter.Run ||
			filter.Status != "" && item.Status != filter.Status {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func Find(items []Item, id string) (Item, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Item{}, false
}

func obligationItems(snapshot store.Snapshot) []Item {
	ids := make([]string, 0, len(snapshot.State.Obligations))
	for id := range snapshot.State.Obligations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		obligation := snapshot.State.Obligations[id]
		node := snapshot.State.Nodes[obligation.NodeID]
		kind := obligationKind(obligation, node)
		if kind == "" {
			continue
		}
		changedAt := obligation.CreatedAt
		if !obligation.ResolvedAt.IsZero() {
			changedAt = obligation.ResolvedAt
		}
		item := Item{
			ID:  stableID(snapshot.Run.ID, obligation.NodeID, id, obligation.Attempt),
			Run: snapshot.Run.ID, Node: obligation.NodeID, Attempt: obligation.Attempt,
			Kind: kind, Assignee: obligation.Assignee, Status: obligation.Status,
			CreatedAt: obligation.CreatedAt, DueAt: obligation.DueAt, ChangedAt: changedAt,
			Summary: obligation.Summary, AvailableActions: append([]string(nil), obligation.AvailableActions...),
			Links:  Links{RunID: snapshot.Run.ID, NodeID: obligation.NodeID, EvidenceRefs: evidenceRefs(obligation.EvidenceRef)},
			Target: ActionTarget{CommandID: obligation.CommandID},
		}
		if contact, ok := snapshot.State.Contacts[obligation.CommandID]; ok {
			item.Nudge = &Nudge{
				LastContactAt: contact.LastContactedAt, NextContactAt: contact.NextContactAt,
				BudgetUsed: contact.Used, BudgetMax: contact.Budget,
				EscalationTarget: contact.EscalationTarget, Paused: contact.Paused,
			}
		}
		items = append(items, item)
	}
	return items
}

func obligationKind(obligation state.ObligationRecord, node state.NodeState) Kind {
	switch obligation.Kind {
	case state.WaitKindAgent:
		return KindAgentObligation
	case state.WaitKindHuman:
		if node.Type == model.NodeTypeDecision {
			return KindDecisionNeeded
		}
		if node.Stage.IsGateStage() {
			return KindReviewNeeded
		}
		return KindHumanWait
	default:
		return ""
	}
}

func blockedItems(snapshot store.Snapshot) []Item {
	nodeIDs := make([]string, 0, len(snapshot.State.Nodes))
	for nodeID := range snapshot.State.Nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	slices.Sort(nodeIDs)
	var items []Item
	for _, nodeID := range nodeIDs {
		node := snapshot.State.Nodes[nodeID]
		// Expanded parents mirror their blocked child. The child is the
		// generation-bound action target and therefore the canonical item.
		if node.Parent == "" && len(node.Children) > 0 {
			continue
		}
		if node.Status != state.NodeStatusBlocked && node.BlockResolution == nil {
			continue
		}
		attempt := node.BlockedAttempt
		if attempt <= 0 {
			attempt = node.Attempt
		}
		status := state.WaitStatusPending
		assignee, summary := node.BlockedOwner, node.BlockedReason
		refs := nodeEvidenceRefs(node)
		createdAt := node.BlockedAt
		changedAt := createdAt
		if node.BlockResolution != nil && node.Status != state.NodeStatusBlocked {
			status = state.WaitStatusSatisfied
			assignee = string(node.BlockResolution.Actor)
			summary = node.BlockResolution.Reason
			refs = appendEvidenceRef(refs, node.BlockResolution.EvidenceRef)
			changedAt = node.BlockResolution.Timestamp
		}
		item := Item{
			ID:  stableID(snapshot.Run.ID, nodeID, "blocked", attempt),
			Run: snapshot.Run.ID, Node: nodeID, Attempt: attempt,
			Kind: KindBlocked, Assignee: assignee, Status: status, Summary: summary,
			CreatedAt: createdAt, ChangedAt: changedAt,
			AvailableActions: []string{string(state.BlockDecisionRetry), string(state.BlockDecisionSkip), string(state.BlockDecisionCancel)},
			Links:            Links{RunID: snapshot.Run.ID, NodeID: nodeID, EvidenceRefs: refs},
			Target:           ActionTarget{Blocked: true},
		}
		if contact, ok := blockedContact(snapshot.State, nodeID, attempt); ok {
			item.DueAt = contact.NextContactAt
			item.Nudge = &Nudge{
				LastContactAt: contact.LastContactedAt, NextContactAt: contact.NextContactAt,
				BudgetUsed: contact.Used, BudgetMax: contact.Budget,
				EscalationTarget: contact.EscalationTarget, Paused: contact.Paused,
			}
		}
		items = append(items, item)
	}
	return items
}

func blockedContact(st *state.State, nodeID string, attempt int) (state.ContactState, bool) {
	commandIDs := make([]string, 0, len(st.Contacts))
	for commandID := range st.Contacts {
		commandIDs = append(commandIDs, commandID)
	}
	slices.Sort(commandIDs)
	for _, commandID := range commandIDs {
		contact := st.Contacts[commandID]
		command, ok := st.OutstandingCommands[commandID]
		serviceableStatus := command.Status == state.CommandStatusIssued || command.Status == state.CommandStatusObserved
		if ok && command.Kind == state.CommandKindBlockNode && serviceableStatus && command.NodeID == nodeID && command.Attempt == attempt {
			return contact, true
		}
	}
	return state.ContactState{}, false
}

func stableID(runID, nodeID, slot string, attempt int) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + nodeID + "\x00" + slot + "\x00" + strconv.Itoa(attempt)))
	return "wi_" + hex.EncodeToString(sum[:12])
}

func evidenceRefs(ref string) []string {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	return []string{ref}
}

func nodeEvidenceRefs(node state.NodeState) []string {
	var refs []string
	if node.ActiveAttempt != nil {
		refs = appendEvidenceRef(refs, node.ActiveAttempt.EvidenceRef)
	}
	for _, decision := range node.Decisions {
		refs = appendEvidenceRef(refs, decision.EvidenceRef)
	}
	if node.BlockResolution != nil {
		refs = appendEvidenceRef(refs, node.BlockResolution.EvidenceRef)
	}
	return refs
}

func appendEvidenceRef(refs []string, ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" || slices.Contains(refs, ref) {
		return refs
	}
	return append(refs, ref)
}
