package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

// Execution transition kinds are persisted by the schema-8 owner-runtime
// protocol. Keep this set closed: new planner/reducer transitions must be
// explicitly admitted before they can execute there.
const (
	TransitionClaimWait                = "claim_wait"
	TransitionObserveWait              = "observe_wait"
	TransitionClaimAttempt             = "claim_attempt"
	TransitionObserveAttempt           = "observe_attempt"
	TransitionRouteObservation         = "route_observation"
	TransitionClaimCompletion          = "claim_completion"
	TransitionObserveCompletion        = "observe_completion"
	TransitionParallelSplit            = "parallel_split"
	TransitionParallelAll              = "parallel_all"
	TransitionParallelAny              = "parallel_any"
	TransitionParallelRoute            = "parallel_route"
	TransitionParallelExclusiveArrival = "parallel_exclusive_arrival"
	TransitionParallelEnd              = "parallel_end"
	TransitionParallelPropagation      = "parallel_propagation"
	TransitionParallelPropagationSeed  = "parallel_propagation_seed"
	TransitionParallelTerminalClosure  = "parallel_terminal_closure"
	TransitionParallelDetachedSink     = "parallel_detached_sink"
	TransitionParallelDetachmentIntern = "parallel_detachment_intern"
	TransitionScheduleContact          = "schedule_contact"
	TransitionMarkContactDue           = "mark_contact_due"
	TransitionNudgeContact             = "nudge_contact"
	TransitionEscalateContact          = "escalate_contact"
	TransitionPauseContact             = "pause_contact"
	TransitionLatchContactHuman        = "latch_contact_human"
	TransitionClearContactHumanLatch   = "clear_contact_human_latch"
	TransitionRecoverContact           = "recover_contact"
	TransitionAuditedSettlement        = "audited_settlement"
)

// ExecutionTransition is a sealed exact successor. Its bytes can only be
// created by path-v1 planner/reducer constructors; callers cannot inject an
// aggregate into the persistence boundary.
type ExecutionTransition struct {
	pre        CheckpointBinding
	post       CheckpointBinding
	postBytes  []byte
	kind       string
	resolution *BlockResolution
}

// AuditedResolution returns the immutable operator authority carried by an
// audited-settlement transition. Other transition kinds return false.
func (t *ExecutionTransition) AuditedResolution() (BlockResolution, bool) {
	if t == nil || t.kind != TransitionAuditedSettlement || t.resolution == nil {
		return BlockResolution{}, false
	}
	return *t.resolution, true
}

func (t *ExecutionTransition) PreBinding() CheckpointBinding {
	if t == nil {
		return CheckpointBinding{}
	}
	return t.pre
}

func (t *ExecutionTransition) PostBinding() CheckpointBinding {
	if t == nil {
		return CheckpointBinding{}
	}
	return t.post
}

func (t *ExecutionTransition) Kind() string {
	if t == nil {
		return ""
	}
	return t.kind
}

func newExecutionTransition(checkpoint, next *CheckpointV7, kind string) (*ExecutionTransition, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	if err := ValidateCheckpointV7(next); err != nil {
		return nil, err
	}
	pre := CurrentCheckpointBinding(checkpoint)
	post := CurrentCheckpointBinding(next)
	if next.Execution == nil || next.Execution.PreviousDigest != pre.Digest || post.Generation != pre.Generation+1 || post.Digest == pre.Digest || kind == "" {
		return nil, fmt.Errorf("%w: invalid sealed execution successor", ErrMutationInvalid)
	}
	postBytes, err := EncodeCheckpointV7(next)
	if err != nil {
		return nil, err
	}
	return &ExecutionTransition{pre: pre, post: post, postBytes: bytes.Clone(postBytes), kind: kind}, nil
}

// ValidateExecutionTransitionForAppend revalidates a sealed transition against
// the exact coherently read checkpoint and template source. Detached bytes and
// checkpoint values are returned so the store never installs caller aliases.
func ValidateExecutionTransitionForAppend(ctx context.Context, currentBytes, templateSource []byte, transition *ExecutionTransition) (CheckpointBinding, []byte, *CheckpointV7, error) {
	if err := ctx.Err(); err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	if transition == nil || transition.kind == "" || len(transition.postBytes) == 0 {
		return CheckpointBinding{}, nil, nil, fmt.Errorf("%w: sealed execution transition is required", ErrMutationInvalid)
	}
	current, err := DecodeCheckpointV7(currentBytes)
	if err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	if _, err := VerifyExecutionInput(ctx, currentBytes, templateSource); err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	pre := CurrentCheckpointBinding(current)
	postBytes := bytes.Clone(transition.postBytes)
	post, err := DecodeCheckpointV7(postBytes)
	if err != nil {
		return pre, nil, nil, err
	}
	if _, err := VerifyExecutionInput(ctx, postBytes, templateSource); err != nil {
		return CurrentCheckpointBinding(current), nil, nil, err
	}
	if bytes.Equal(currentBytes, postBytes) {
		if pre != transition.post {
			return pre, nil, nil, fmt.Errorf("%w: exact transition bytes have a different post-binding", ErrMutationInconsistent)
		}
		return transition.pre, postBytes, post, nil
	}
	if pre != transition.pre {
		return pre, nil, nil, fmt.Errorf("%w: transition pre-binding differs from current checkpoint", ErrMutationInconsistent)
	}
	currentInitialize, currentInitializeDigest, err := CanonicalInitializationAnchor(current)
	if err != nil {
		return pre, nil, nil, err
	}
	postInitialize, postInitializeDigest, err := CanonicalInitializationAnchor(post)
	if err != nil {
		return pre, nil, nil, err
	}
	if !bytes.Equal(postInitialize, currentInitialize) || postInitializeDigest != currentInitializeDigest ||
		post.Execution == nil || post.Execution.Revision != CheckpointRevision(current)+1 ||
		post.Execution.PreviousDigest != pre.Digest || CurrentCheckpointBinding(post) != transition.post {
		return pre, nil, nil, fmt.Errorf("%w: transition post-state is not the exact next checkpoint", ErrMutationInvalid)
	}
	if !post.Execution.LogAdvanced && (post.Execution.LastLogSeq != CurrentLastLogSeq(current) || post.Execution.LogChecksum != CurrentLogChecksum(current)) {
		return pre, nil, nil, fmt.Errorf("%w: log-preserving transition changed completion anchors", ErrMutationInvalid)
	}
	if post.Execution.LogAdvanced {
		currentLogSeq := CurrentLastLogSeq(current)
		if post.Execution.LastLogSeq <= currentLogSeq || post.Execution.LastLogSeq-currentLogSeq > uint64(MaxRoutingLogEntries) {
			return pre, nil, nil, fmt.Errorf("%w: log-advancing transition has invalid logical sequence delta", ErrMutationInvalid)
		}
	}
	return pre, postBytes, post, nil
}

// ExclusiveAttemptPlan is a sealed exact perform_attempt_v1 command plus the
// adapter request authority derived from the exact current template.
type ExclusiveAttemptPlan struct {
	command   CommandRecord
	nodeID    string
	performer *model.Performer
	params    map[string]string
}

// ExclusiveWaitPlan is the sealed durable lifecycle for one live wait-node
// activation. Timer schedules bind both their scheduling instant and due
// instant; signal waits bind the exact signal from the immutable template.
// The command remains a perform_attempt_v1 because settlement/routing already
// treats task, decision, and wait activation attempts uniformly.
type ExclusiveWaitPlan struct {
	command     CommandRecord
	nodeID      string
	waitKind    string
	signal      string
	scheduledAt time.Time
	dueAt       time.Time
}

type exclusiveWaitPayload struct {
	TemplateRef        string       `json:"templateRef"`
	TemplateSourceHash string       `json:"templateSourceHash"`
	NodeID             string       `json:"nodeId"`
	SourceActivationID ActivationID `json:"sourceActivationId"`
	SourceGeneration   uint64       `json:"sourceGeneration"`
	Attempt            uint64       `json:"attempt"`
	WaitKind           string       `json:"waitKind"`
	Signal             string       `json:"signal,omitempty"`
	ScheduledAt        string       `json:"scheduledAt,omitempty"`
	DueAt              string       `json:"dueAt,omitempty"`
}

func (p *ExclusiveWaitPlan) Command() CommandRecord {
	if p == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(p.command)
}

func (p *ExclusiveWaitPlan) NodeID() string {
	if p == nil {
		return ""
	}
	return p.nodeID
}

func (p *ExclusiveWaitPlan) WaitKind() string {
	if p == nil {
		return ""
	}
	return p.waitKind
}

func (p *ExclusiveWaitPlan) Signal() string {
	if p == nil {
		return ""
	}
	return p.signal
}

func (p *ExclusiveWaitPlan) DueAt() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.dueAt
}

// PlanExclusiveWait creates the exact durable wait claim. now is authority
// only for duration waits; until and signal semantics come solely from the
// immutable exact template.
func PlanExclusiveWait(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID, now time.Time) (*ExclusiveWaitPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil || sourcePathID == "" {
		return nil, fmt.Errorf("%w: complete sealed wait input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	source, ok := aggregate.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return nil, fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := aggregate.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return nil, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	for _, command := range aggregate.Commands {
		otherWait := parallelActiveWaitCommand(input, aggregate, command) && command.Identity.SourceActivationID != source.SourceActivation.ID
		if command.State.Active() && !otherWait {
			return nil, fmt.Errorf("%w: active command %q must recover before planning", ErrMutationInconsistent, command.ID)
		}
	}
	reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
	if !ok || reservation.State != ReservationActivated {
		return nil, fmt.Errorf("%w: source reservation is not activated", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Type != model.NodeTypeWait || node.Wait == nil {
		return nil, fmt.Errorf("%w: node %q is not a wait", ErrExclusiveUnsupported, reservation.NodeID)
	}
	waitKind := exactWaitKind(node.Wait)
	if waitKind != "signal" && waitKind != "duration" && waitKind != "until" {
		return nil, fmt.Errorf("%w: wait node %q has no supported wait authority", ErrExclusiveUnsupported, reservation.NodeID)
	}
	plan := &ExclusiveWaitPlan{nodeID: reservation.NodeID, waitKind: waitKind}
	if waitKind == "signal" {
		plan.signal = strings.TrimSpace(node.Wait.Signal)
		if plan.signal == "" {
			return nil, fmt.Errorf("%w: signal wait is empty", ErrExclusiveInputInvalid)
		}
	} else {
		plan.scheduledAt = now.UTC()
		if plan.scheduledAt.IsZero() {
			return nil, fmt.Errorf("%w: timer scheduling instant is required", ErrExclusiveInputInvalid)
		}
		if waitKind == "until" {
			plan.dueAt, err = model.ParseRFC3339(strings.TrimSpace(node.Wait.Until))
		} else {
			var duration time.Duration
			duration, err = time.ParseDuration(strings.TrimSpace(node.Wait.Duration))
			if err == nil && duration <= 0 {
				err = fmt.Errorf("duration must be positive")
			}
			if err == nil {
				plan.dueAt = plan.scheduledAt.Add(duration)
			}
		}
		if err != nil || plan.dueAt.IsZero() {
			return nil, fmt.Errorf("%w: invalid %s wait schedule", ErrExclusiveInputInvalid, waitKind)
		}
		plan.dueAt = plan.dueAt.UTC()
	}
	payload := exclusiveWaitPayload{
		TemplateRef: aggregate.TemplateRef, TemplateSourceHash: aggregate.TemplateSourceHash,
		NodeID: reservation.NodeID, SourceActivationID: activation.ID,
		SourceGeneration: activation.Ref.Generation, Attempt: 1,
		WaitKind: waitKind, Signal: plan.signal,
	}
	if !plan.scheduledAt.IsZero() {
		payload.ScheduledAt = plan.scheduledAt.Format(time.RFC3339Nano)
		payload.DueAt = plan.dueAt.Format(time.RFC3339Nano)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	identity := CommandIdentity{
		RunID: aggregate.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: activation.ID, SourceGeneration: activation.Ref.Generation,
		Attempt: 1, PlanDigest: payloadDigest(payloadJSON),
	}
	plan.command, err = commandWithState(identity, payloadJSON, CommandIssued)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// ClaimExclusiveWait atomically records the exact pending wait and its
// schedule before any timer or external signal may satisfy it.
func ClaimExclusiveWait(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveWaitPlan) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || plan == nil {
		return nil, fmt.Errorf("%w: sealed input and wait plan are required", ErrMutationInvalid)
	}
	validated, err := validateExclusiveWaitPlan(input, plan.command)
	if err != nil {
		return nil, err
	}
	if validated.nodeID != plan.nodeID || validated.waitKind != plan.waitKind || validated.signal != plan.signal ||
		!validated.scheduledAt.Equal(plan.scheduledAt) || !validated.dueAt.Equal(plan.dueAt) {
		return nil, fmt.Errorf("%w: supplied wait differs from its exact command", ErrMutationInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	if _, exists := aggregate.Commands[plan.command.ID]; exists {
		return nil, fmt.Errorf("%w: wait command already exists", ErrMutationInconsistent)
	}
	effect := SideEffectIdentity{
		Kind: SideEffectWait, RunID: aggregate.RunID,
		ActivationID: plan.command.Identity.SourceActivationID, Attempt: 1,
		WaitKind: plan.waitKind, State: "pending",
	}
	effect.ID, err = WaitIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.WaitKind)
	if err != nil {
		return nil, err
	}
	aggregate.Commands[plan.command.ID] = cloneCommandRecord(plan.command)
	aggregate.SideEffects[effect.ID] = effect
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionClaimWait)
}

// RecoverExclusiveWaits returns every exact pending wait claim in stable
// command order. Verified parallel input may own sibling waits concurrently;
// non-parallel execution retains the strict single-wait invariant.
func RecoverExclusiveWaits(ctx context.Context, input *VerifiedExclusiveInput) ([]*ExclusiveWaitPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	found := make([]*ExclusiveWaitPlan, 0, 1)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandPerformAttempt || !command.State.Active() {
			continue
		}
		plan, planErr := validateExclusiveWaitPlan(input, command)
		if planErr != nil {
			// An active task/decision attempt is not a wait claim.
			var payload exclusiveWaitPayload
			if decodeExactPayload(command.Payload, &payload) != nil || payload.WaitKind == "" {
				continue
			}
			return nil, planErr
		}
		found = append(found, plan)
	}
	if len(found) > 1 && input.parallel == nil {
		return nil, fmt.Errorf("%w: multiple pending exclusive waits", ErrMutationInconsistent)
	}
	slices.SortFunc(found, func(a, b *ExclusiveWaitPlan) int {
		return strings.Compare(a.command.ID, b.command.ID)
	})
	for index := 1; index < len(found); index++ {
		if found[index-1].command.Identity.SourceActivationID == found[index].command.Identity.SourceActivationID {
			return nil, fmt.Errorf("%w: multiple pending waits own activation %q", ErrMutationInconsistent, found[index].command.Identity.SourceActivationID)
		}
	}
	return found, nil
}

// RecoverExclusiveWait returns the first exact pending wait claim, if any.
// Parallel callers that need to reason about every sibling use
// RecoverExclusiveWaits.
func RecoverExclusiveWait(ctx context.Context, input *VerifiedExclusiveInput) (*ExclusiveWaitPlan, bool, error) {
	waits, err := RecoverExclusiveWaits(ctx, input)
	if err != nil {
		return nil, false, err
	}
	if len(waits) == 0 {
		return nil, false, nil
	}
	return waits[0], true, nil
}

// ObserveExclusiveWait settles a previously claimed wait. Timer callers must
// first compare DueAt with their clock; signal callers must match Signal.
func ObserveExclusiveWait(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveWaitPlan, actor, evidenceRef string) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waits, err := RecoverExclusiveWaits(ctx, input)
	if err != nil {
		return nil, err
	}
	var current *ExclusiveWaitPlan
	for _, candidate := range waits {
		if plan != nil && candidate.command.ID == plan.command.ID {
			current = candidate
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("%w: exact pending wait claim is absent", ErrMutationInconsistent)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	command := aggregate.Commands[current.command.ID]
	command.State = CommandObserved
	aggregate.Commands[command.ID] = command
	completeContactsForSettledCommand(&aggregate, command.ID, false, int64(CurrentLastLogSeq(input.checkpoint))+1)
	activation := aggregate.Routing.Activations[command.Identity.SourceActivationID]
	source := aggregate.Routing.Paths[activation.OutputPathID]
	node := input.template.Nodes[current.nodeID]
	observation := ExclusiveObservation{
		SourcePathID: source.ID, Attempt: 1, Outcome: "satisfied",
		Actor: strings.TrimSpace(actor), EvidenceRef: strings.TrimSpace(evidenceRef),
	}
	perform, settle, effect, err := observedAttemptCommands(aggregate.View(), current.nodeID, node, source, observation, false)
	if err != nil {
		return nil, err
	}
	aggregate.Commands[perform.ID] = perform
	aggregate.Commands[settle.ID] = settle
	aggregate.SideEffects[effect.ID] = effect
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionObserveWait)
}

// ExactExclusiveSignalObserved recognizes only an already-settled replay of
// the same node, signal, and actor authority. It lets an API retry after an
// ambiguous commit succeed without allowing a different caller to inherit the
// first observation.
func ExactExclusiveSignalObserved(ctx context.Context, input *VerifiedExclusiveInput, nodeID, signal, actor string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if input == nil || input.checkpoint == nil {
		return false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return false, err
	}
	wantedSignal := strings.TrimSpace(signal)
	wantedActor := strings.TrimSpace(actor)
	authorityDrift := false
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandPerformAttempt || (command.State != CommandObserved && command.State != CommandReconciled) {
			continue
		}
		plan, planErr := validateExclusiveWaitPlan(input, command)
		if planErr != nil || plan.waitKind != "signal" || plan.nodeID != nodeID || plan.signal != wantedSignal {
			continue
		}
		effectID, effectErr := WaitIdentity(aggregate.RunID, command.Identity.SourceActivationID, 1, "signal")
		effect, ok := aggregate.SideEffects[effectID]
		if effectErr != nil || !ok || effect.Kind != SideEffectWait || effect.State != "satisfied" {
			return false, fmt.Errorf("%w: observed signal lacks its satisfied wait effect", ErrMutationInconsistent)
		}
		foundSettlement := false
		for _, settle := range aggregate.Commands {
			if settle.Identity.Kind != CommandSettleAttempt || settle.Identity.InputDigest != command.ID {
				continue
			}
			foundSettlement = true
			var payload settleAttemptObservationPayload
			if err := decodeExactPayload(settle.Payload, &payload); err != nil {
				return false, fmt.Errorf("%w: observed signal settlement is invalid", ErrMutationInvalid)
			}
			if payload.ResultCode != "satisfied" || payload.Actor != wantedActor || payload.EvidenceRef != "signal:"+wantedSignal {
				authorityDrift = true
				continue
			}
			return true, nil
		}
		if !foundSettlement {
			return false, fmt.Errorf("%w: observed signal settlement is absent", ErrMutationInconsistent)
		}
	}
	if authorityDrift {
		return false, fmt.Errorf("%w: signal replay authority differs from the recorded observation", ErrMutationInvalid)
	}
	return false, nil
}

func validateExclusiveWaitPlan(input *VerifiedExclusiveInput, command CommandRecord) (*ExclusiveWaitPlan, error) {
	if input == nil || input.checkpoint == nil || input.template == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	if err := ValidateCommand(command); err != nil || command.Identity.Kind != CommandPerformAttempt || command.Identity.Attempt != 1 {
		return nil, fmt.Errorf("%w: invalid wait command", ErrMutationInvalid)
	}
	var payload exclusiveWaitPayload
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		return nil, fmt.Errorf("%w: wait payload: %v", ErrMutationInvalid, err)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	activation, ok := aggregate.Routing.Activations[payload.SourceActivationID]
	if !ok || activation.Ref.Generation != payload.SourceGeneration || command.Identity.SourceActivationID != activation.ID ||
		command.Identity.SourceGeneration != activation.Ref.Generation || payload.Attempt != 1 ||
		payload.TemplateRef != aggregate.TemplateRef || payload.TemplateSourceHash != aggregate.TemplateSourceHash {
		return nil, fmt.Errorf("%w: wait command binding mismatch", ErrMutationInvalid)
	}
	reservation := aggregate.Routing.Reservations[activation.ReservationID]
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Type != model.NodeTypeWait || node.Wait == nil || payload.NodeID != reservation.NodeID || payload.WaitKind != exactWaitKind(node.Wait) {
		return nil, fmt.Errorf("%w: wait command differs from exact template", ErrMutationInvalid)
	}
	plan := &ExclusiveWaitPlan{command: cloneCommandRecord(command), nodeID: payload.NodeID, waitKind: payload.WaitKind, signal: payload.Signal}
	switch payload.WaitKind {
	case "signal":
		if payload.Signal != strings.TrimSpace(node.Wait.Signal) || payload.Signal == "" || payload.ScheduledAt != "" || payload.DueAt != "" {
			return nil, fmt.Errorf("%w: signal wait payload mismatch", ErrMutationInvalid)
		}
	case "duration", "until":
		if payload.Signal != "" {
			return nil, fmt.Errorf("%w: timer wait carries a signal", ErrMutationInvalid)
		}
		plan.scheduledAt, err = time.Parse(time.RFC3339Nano, payload.ScheduledAt)
		if err == nil {
			plan.dueAt, err = time.Parse(time.RFC3339Nano, payload.DueAt)
		}
		if err != nil || plan.scheduledAt.IsZero() || plan.dueAt.IsZero() {
			return nil, fmt.Errorf("%w: timer wait schedule is invalid", ErrMutationInvalid)
		}
		if payload.WaitKind == "until" {
			var exact time.Time
			exact, err = model.ParseRFC3339(strings.TrimSpace(node.Wait.Until))
			if err != nil || !plan.dueAt.Equal(exact) {
				return nil, fmt.Errorf("%w: until wait due time mismatch", ErrMutationInvalid)
			}
		} else {
			var duration time.Duration
			duration, err = time.ParseDuration(strings.TrimSpace(node.Wait.Duration))
			if err != nil || duration <= 0 || !plan.dueAt.Equal(plan.scheduledAt.Add(duration)) {
				return nil, fmt.Errorf("%w: duration wait due time mismatch", ErrMutationInvalid)
			}
		}
	default:
		return nil, fmt.Errorf("%w: unsupported wait kind %q", ErrExclusiveUnsupported, payload.WaitKind)
	}
	return plan, nil
}

func (p *ExclusiveAttemptPlan) Command() CommandRecord {
	if p == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(p.command)
}

func (p *ExclusiveAttemptPlan) NodeID() string {
	if p == nil {
		return ""
	}
	return p.nodeID
}

func (p *ExclusiveAttemptPlan) Performer() *model.Performer {
	if p == nil || p.performer == nil {
		return nil
	}
	return cloneExclusivePerformer(p.performer)
}

func (p *ExclusiveAttemptPlan) Params() map[string]string {
	if p == nil {
		return nil
	}
	return cloneExclusiveParams(p.params)
}

func PlanExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID, attempt uint64, params map[string]string) (*ExclusiveAttemptPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil || sourcePathID == "" || attempt == 0 {
		return nil, fmt.Errorf("%w: complete sealed attempt input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	source, ok := view.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return nil, fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return nil, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	for _, command := range view.Commands {
		otherWait := parallelActiveWaitCommand(input, aggregate, command) && command.Identity.SourceActivationID != source.SourceActivation.ID
		if command.State.Active() && !otherWait {
			return nil, fmt.Errorf("%w: active command %q must recover before planning", ErrMutationInconsistent, command.ID)
		}
	}
	reservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || reservation.State != ReservationActivated {
		return nil, fmt.Errorf("%w: source reservation is not activated", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok {
		return nil, fmt.Errorf("%w: source node is absent from exact template", ErrExclusiveInputInvalid)
	}
	if (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) || node.Performer == nil || node.IsCompound() {
		return nil, fmt.Errorf("%w: node %q of type %q has no direct performer dispatch path", ErrExclusiveUnsupported, reservation.NodeID, node.Type)
	}
	wantAttempt := uint64(1)
	for _, command := range view.Commands {
		if command.Identity.Kind == CommandPerformAttempt && command.Identity.SourceActivationID == source.SourceActivation.ID && command.Identity.Attempt >= wantAttempt {
			if command.Identity.Attempt == math.MaxUint64 {
				return nil, &OverBudgetError{Limit: "attempt", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
			}
			wantAttempt = command.Identity.Attempt + 1
		}
	}
	if attempt != wantAttempt {
		return nil, fmt.Errorf("%w: attempt %d is not exact next attempt %d", ErrMutationInvalid, attempt, wantAttempt)
	}
	performer := model.InterpolatePerformer(*node.Performer, params)
	payload, err := performCommandPayload(view, reservation.NodeID, source, attempt, &performer, params)
	if err != nil {
		return nil, err
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: attempt, PlanDigest: payloadDigest(payload),
	}
	command, err := commandWithState(identity, payload, CommandIssued)
	if err != nil {
		return nil, err
	}
	return &ExclusiveAttemptPlan{command: command, nodeID: reservation.NodeID, performer: cloneExclusivePerformer(&performer), params: cloneExclusiveParams(params)}, nil
}

func parallelActiveWaitCommand(input *VerifiedExclusiveInput, aggregate AggregateCheckpoint, command CommandRecord) bool {
	if input == nil || input.parallel == nil || command.Identity.Kind != CommandPerformAttempt {
		return false
	}
	activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]
	if !ok {
		return false
	}
	reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
	return ok && input.template.Nodes[reservation.NodeID].Type == model.NodeTypeWait
}

// ClaimExclusiveAttempt creates the exact durable self-only claim transition.
// It does not invoke an adapter and is safe to compute inside a coherent read.
func ClaimExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveAttemptPlan) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || plan == nil {
		return nil, fmt.Errorf("%w: sealed input and attempt plan are required", ErrMutationInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	activation := view.Routing.Activations[plan.command.Identity.SourceActivationID]
	if activation.ID == "" || activation.Ref.Generation != plan.command.Identity.SourceGeneration {
		return nil, fmt.Errorf("%w: attempt plan activation is not current", ErrMutationInvalid)
	}
	source := view.Routing.Paths[activation.OutputPathID]
	replanned, err := PlanExclusiveAttempt(ctx, input, source.ID, plan.command.Identity.Attempt, plan.params)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(replanned.command, plan.command) || replanned.nodeID != plan.nodeID || !canonicalEqual(replanned.performer, plan.performer) || !canonicalEqual(replanned.params, plan.params) {
		return nil, fmt.Errorf("%w: supplied attempt differs from deterministic plan", ErrMutationInvalid)
	}
	aggregate.Commands[plan.command.ID] = cloneCommandRecord(plan.command)
	effect := SideEffectIdentity{Kind: SideEffectAttempt, RunID: aggregate.RunID, ActivationID: activation.ID, Attempt: plan.command.Identity.Attempt, State: "claimed"}
	effect.ID, err = AttemptIdentity(effect.RunID, effect.ActivationID, effect.Attempt)
	if err != nil {
		return nil, err
	}
	aggregate.SideEffects[effect.ID] = effect
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionClaimAttempt)
}

// RecoverExclusiveAttempt returns the sole durable active performer request.
// It never converts a claimed command back into new-perform authority.
func RecoverExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput) (*ExclusiveAttemptPlan, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil {
		return nil, false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, false, err
	}
	var command CommandRecord
	for _, candidate := range aggregate.Commands {
		if !candidate.State.Active() || candidate.Identity.Kind != CommandPerformAttempt {
			continue
		}
		activation, activationOK := aggregate.Routing.Activations[candidate.Identity.SourceActivationID]
		reservation, reservationOK := aggregate.Routing.Reservations[activation.ReservationID]
		if activationOK && reservationOK && input.template.Nodes[reservation.NodeID].Type == model.NodeTypeWait {
			continue
		}
		if command.ID != "" {
			return nil, false, fmt.Errorf("%w: multiple active performer commands", ErrMutationInconsistent)
		}
		command = cloneCommandRecord(candidate)
	}
	if command.ID == "" {
		return nil, false, nil
	}
	var payload performAttemptPayload
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		return nil, false, fmt.Errorf("%w: claimed performer payload: %v", ErrMutationInvalid, err)
	}
	view := aggregate.View()
	activation, ok := view.Routing.Activations[command.Identity.SourceActivationID]
	if !ok || activation.Ref.Generation != command.Identity.SourceGeneration || activation.OutputPathID == "" {
		return nil, false, fmt.Errorf("%w: claimed performer activation is unavailable", ErrMutationInconsistent)
	}
	source, ok := view.Routing.Paths[activation.OutputPathID]
	if !ok || source.State != PathLive {
		return nil, false, fmt.Errorf("%w: claimed performer source path is not live", ErrMutationInconsistent)
	}
	reservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || reservation.NodeID != payload.NodeID {
		return nil, false, fmt.Errorf("%w: claimed performer node binding differs", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Performer == nil || (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) || node.IsCompound() {
		return nil, false, fmt.Errorf("%w: claimed node has no direct performer path", ErrExclusiveUnsupported)
	}
	wantPerformer := model.InterpolatePerformer(*node.Performer, payload.Params)
	if payload.TemplateRef != view.TemplateRef || payload.TemplateSourceHash != view.TemplateSourceHash ||
		payload.SourceActivationID != string(activation.ID) || payload.SourceGeneration != activation.Ref.Generation ||
		payload.Attempt != command.Identity.Attempt || !canonicalEqual(payload.Performer, &wantPerformer) {
		return nil, false, fmt.Errorf("%w: claimed performer request differs from exact template-bound payload", ErrMutationInvalid)
	}
	effectID, err := AttemptIdentity(view.RunID, activation.ID, command.Identity.Attempt)
	if err != nil {
		return nil, false, err
	}
	effect, ok := aggregate.SideEffects[effectID]
	if !ok || !ActiveSideEffect(effect) {
		return nil, false, fmt.Errorf("%w: claimed performer lacks active attempt evidence", ErrMutationInconsistent)
	}
	return &ExclusiveAttemptPlan{
		command: command, nodeID: reservation.NodeID, performer: cloneExclusivePerformer(payload.Performer), params: cloneExclusiveParams(payload.Params),
	}, true, nil
}

// ObserveExclusiveAttempt settles exactly one durable claim. recovered marks
// an observation obtained through adapter reconciliation after an ambiguous
// perform acknowledgement; it never authorizes another Perform call.
func ObserveExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveAttemptPlan, observation ExclusiveObservation, recovered bool) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, found, err := RecoverExclusiveAttempt(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found || plan == nil || !exactExclusiveCommand(current.command, plan.command) || !canonicalEqual(current.performer, plan.performer) || !canonicalEqual(current.params, plan.params) {
		return nil, fmt.Errorf("%w: observation does not settle the exact durable claim", ErrMutationInvalid)
	}
	actor := legacy.ActorRef(strings.TrimSpace(observation.Actor))
	if strings.TrimSpace(observation.Outcome) == "" || !legacy.ValidateActorRef(actor) || legacy.IsEngineActor(actor) {
		return nil, fmt.Errorf("%w: performer observation requires outcome and actor provenance", ErrMutationInvalid)
	}
	observation.Actor = string(actor)
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	activation := view.Routing.Activations[plan.command.Identity.SourceActivationID]
	source := view.Routing.Paths[activation.OutputPathID]
	node := input.template.Nodes[plan.nodeID]
	normalizedOutcome, _, terminalUnhandledFailure, err := normalizeExclusiveObservationResult(node, observation.Outcome, plan.command.Identity.Attempt, observation.ResolutionDigest, input.parallel != nil)
	if err != nil {
		return nil, err
	}
	observation.Outcome = normalizedOutcome
	outcome := observation.Outcome
	observation.SourcePathID = source.ID
	observation.Attempt = plan.command.Identity.Attempt
	disposition, err := classifyExclusiveObservation(view, input.template, observation, input.parallel != nil)
	if err != nil {
		return nil, err
	}
	if disposition == ExclusiveRouteReady && !terminalUnhandledFailure {
		outgoing, err := exactOutgoingEdges(view.TemplateRef, plan.nodeID, node.Next)
		if err != nil {
			return nil, err
		}
		if _, err := resolveExclusiveEdge(node, observation.Outcome, outgoing); err != nil {
			return nil, err
		}
	}
	observedPerform := cloneCommandRecord(plan.command)
	observedPerform.State = CommandObserved
	if recovered {
		observedPerform.State = CommandReconciled
	}
	if err := ValidateCommand(observedPerform); err != nil {
		return nil, err
	}
	aggregate.Commands[observedPerform.ID] = observedPerform
	settlePayloadValue := settleAttemptObservationPayload{
		TemplateRef: aggregate.TemplateRef, SourceCommandID: observedPerform.ID,
		SourceActivationID: activation.ID, SourceGeneration: activation.Ref.Generation,
		Attempt: plan.command.Identity.Attempt, ResultCode: outcome,
		Actor: observation.Actor, EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash,
		ResolutionDigest: observation.ResolutionDigest, ExternalRef: observation.ExternalRef, Feedback: observation.Feedback,
	}
	if terminalUnhandledFailure {
		settlePayloadValue.ReasonCode = "performer_failed"
	}
	settlePayload, err := json.Marshal(settlePayloadValue)
	if err != nil {
		return nil, err
	}
	settleIdentity := CommandIdentity{
		RunID: aggregate.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: activation.ID, SourceGeneration: activation.Ref.Generation,
		Attempt: plan.command.Identity.Attempt, InputDigest: observedPerform.ID,
		PlanDigest: payloadDigest(settlePayload), ResultCode: outcome,
	}
	settle, err := observedCommand(settleIdentity, settlePayload)
	if err != nil {
		return nil, err
	}
	aggregate.Commands[settle.ID] = settle
	effectID, err := AttemptIdentity(aggregate.RunID, activation.ID, plan.command.Identity.Attempt)
	if err != nil {
		return nil, err
	}
	effect := aggregate.SideEffects[effectID]
	effect.State = "observed"
	if terminalUnhandledFailure {
		effect.State = "failed"
	}
	aggregate.SideEffects[effectID] = effect
	completeContactsForSettledCommand(&aggregate, observedPerform.ID, false, int64(CurrentLastLogSeq(input.checkpoint))+1)
	if _, _, _, err := observedAttemptCommands(aggregate.View(), plan.nodeID, input.template.Nodes[plan.nodeID], source, observation, terminalUnhandledFailure); err != nil {
		return nil, err
	}
	if terminalUnhandledFailure {
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		failed := aggregate.Routing.Paths[source.ID]
		failed, _, err = inheritPathDetachments(&aggregate.Routing, failed)
		if err != nil {
			return nil, err
		}
		failed.State = PathFailed
		failed.UpdatedSeq = eventSeq
		receiptID, receiptErr := DispositionReceiptIdentity(failed.ID, PathLive, PathFailed, "performer_failed", settle.ID, "", uint64(eventSeq))
		if receiptErr != nil {
			return nil, receiptErr
		}
		failed.Disposition = &DispositionReceipt{ID: receiptID, PathID: failed.ID, FromState: PathLive, ToState: PathFailed, ReasonCode: "performer_failed", CommandID: settle.ID, EventSeq: eventSeq}
		causeID, causeErr := CauseIdentity(failed.ID, TerminalFailed, "performer_failed", failed.SourceActivation.ID, settle.ID, "", uint64(eventSeq))
		if causeErr != nil {
			return nil, causeErr
		}
		failed.TerminalCauseID = causeID
		aggregate.Routing.Paths[failed.ID] = failed
		aggregate.Routing.CauseRecords[causeID] = CauseRecord{ID: causeID, SourcePathID: failed.ID, TerminalKind: TerminalFailed, DispositionReason: "performer_failed", SourceActivationID: failed.SourceActivation.ID, SourceCommandID: settle.ID, EventSeq: eventSeq}
		causeDigest, causeErr := CauseSetIdentity([]CauseID{causeID})
		if causeErr != nil {
			return nil, causeErr
		}
		aggregate.Routing.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []CauseID{causeID}}
	}
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionObserveAttempt)
}

// ExactExclusiveAttemptObserved recognizes an already-settled task or
// decision observation. It returns the exact durable perform command so the
// API layer can additionally bind its public command alias. Any observation
// drift is rejected rather than inheriting authority from the first caller.
func ExactExclusiveAttemptObserved(ctx context.Context, input *VerifiedExclusiveInput, nodeID, commandIDPrefix string, observation ExclusiveObservation) (CommandRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return CommandRecord{}, false, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil {
		return CommandRecord{}, false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	actor := legacy.ActorRef(strings.TrimSpace(observation.Actor))
	if strings.TrimSpace(observation.Outcome) == "" || !legacy.ValidateActorRef(actor) || legacy.IsEngineActor(actor) {
		return CommandRecord{}, false, fmt.Errorf("%w: performer observation requires outcome and actor provenance", ErrMutationInvalid)
	}
	node, ok := input.template.Nodes[nodeID]
	if !ok || (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) {
		return CommandRecord{}, false, fmt.Errorf("%w: node %q has no direct performer path", ErrExclusiveUnsupported, nodeID)
	}
	if _, err := canonicalExclusiveOutcome(node, normalizeExclusiveTaskAction(node, observation.Outcome)); err != nil {
		return CommandRecord{}, false, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return CommandRecord{}, false, err
	}
	var matched CommandRecord
	for _, perform := range aggregate.Commands {
		if perform.Identity.Kind != CommandPerformAttempt || (perform.State != CommandObserved && perform.State != CommandReconciled) {
			continue
		}
		if commandIDPrefix != "" && !strings.HasPrefix(perform.ID, commandIDPrefix) {
			continue
		}
		var request performAttemptPayload
		if decodeExactPayload(perform.Payload, &request) != nil || request.NodeID != nodeID {
			continue
		}
		outcome, reasonCode, _, normalizeErr := normalizeExclusiveObservationResult(node, observation.Outcome, perform.Identity.Attempt, observation.ResolutionDigest, input.parallel != nil)
		if normalizeErr != nil {
			return CommandRecord{}, false, normalizeErr
		}
		wanted := settleAttemptObservationPayload{
			TemplateRef: aggregate.TemplateRef, ResultCode: outcome, ReasonCode: reasonCode, Actor: string(actor),
			EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash,
			ResolutionDigest: observation.ResolutionDigest, ExternalRef: observation.ExternalRef,
			Feedback: observation.Feedback,
		}
		for _, settle := range aggregate.Commands {
			if settle.Identity.Kind != CommandSettleAttempt || settle.Identity.InputDigest != perform.ID {
				continue
			}
			var payload settleAttemptObservationPayload
			if err := decodeExactPayload(settle.Payload, &payload); err != nil {
				return CommandRecord{}, false, fmt.Errorf("%w: observed performer settlement is invalid", ErrMutationInvalid)
			}
			candidate := wanted
			candidate.SourceCommandID = perform.ID
			candidate.SourceActivationID = perform.Identity.SourceActivationID
			candidate.SourceGeneration = perform.Identity.SourceGeneration
			candidate.Attempt = perform.Identity.Attempt
			if payload != candidate && commandIDPrefix != "" {
				return CommandRecord{}, false, fmt.Errorf("%w: performer replay authority differs from the recorded observation", ErrMutationInvalid)
			}
			if payload != candidate {
				continue
			}
			if matched.ID != "" && matched.ID != perform.ID {
				return CommandRecord{}, false, fmt.Errorf("%w: performer replay is ambiguous across attempts", ErrMutationInconsistent)
			}
			matched = cloneCommandRecord(perform)
		}
	}
	return matched, matched.ID != "", nil
}

func normalizeExclusiveObservationResult(node model.Node, outcome string, attempt uint64, resolutionDigest string, parallel bool) (result, reason string, terminalUnhandledFailure bool, err error) {
	result, err = canonicalExclusiveOutcome(node, normalizeExclusiveTaskAction(node, outcome))
	if err != nil {
		return "", "", false, err
	}
	terminalUnhandledFailure = exclusiveTaskUnhandledFailureTerminal(node, result, attempt, resolutionDigest, parallel)
	if terminalUnhandledFailure {
		return "failed", "performer_failed", true, nil
	}
	return result, "", false, nil
}

// PendingExclusiveObservation returns the next routable observed settlement
// that still owns a live source path. Parallel branches are ordered by exact
// source and settlement identity and folded one per checkpoint transition;
// non-parallel execution retains the strict single-pending invariant.
// Retry-pending settlements remain durable provenance but do not prevent
// planning their exact next attempt.
func PendingExclusiveObservation(ctx context.Context, input *VerifiedExclusiveInput) (ExclusiveObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return ExclusiveObservation{}, false, err
	}
	if input == nil || input.checkpoint == nil {
		return ExclusiveObservation{}, false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return ExclusiveObservation{}, false, err
	}
	used := make(map[string]bool)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind == CommandRoutePaths {
			used[command.Identity.InputDigest] = true
		}
	}
	type pendingCandidate struct {
		observation  ExclusiveObservation
		settlementID string
	}
	pending := make([]pendingCandidate, 0, 1)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandSettleAttempt || (command.State != CommandObserved && command.State != CommandReconciled) || used[command.ID] {
			continue
		}
		activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]
		if !ok || activation.Ref.Generation != command.Identity.SourceGeneration {
			continue
		}
		source, ok := aggregate.Routing.Paths[activation.OutputPathID]
		if !ok || source.State != PathLive {
			continue
		}
		var payload settleAttemptObservationPayload
		if err := decodeExactPayload(command.Payload, &payload); err != nil {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: pending settlement payload: %v", ErrMutationInvalid, err)
		}
		candidate := ExclusiveObservation{
			SourcePathID: source.ID, Attempt: command.Identity.Attempt, Outcome: payload.ResultCode,
			Actor: payload.Actor, EvidenceRef: payload.EvidenceRef, EvidenceHash: payload.EvidenceHash,
			ResolutionDigest: payload.ResolutionDigest, ExternalRef: payload.ExternalRef, Feedback: payload.Feedback,
		}
		if candidate.ResolutionDigest == "" {
			reservation := aggregate.Routing.Reservations[activation.ReservationID]
			for recordID, resolution := range aggregate.AdminResolutions {
				if resolution.NodeID != reservation.NodeID || resolution.BlockedAttempt != candidate.Attempt {
					continue
				}
				digest, digestErr := ValidateBlockResolution(resolution)
				if digestErr != nil || aggregate.AdminRecords[recordID].ResolutionDigest != digest || candidate.ResolutionDigest != "" {
					return ExclusiveObservation{}, false, fmt.Errorf("%w: audited settlement generation is invalid or ambiguous", ErrMutationInconsistent)
				}
				candidate.ResolutionDigest = digest
			}
		}
		disposition, err := classifyExclusiveObservation(aggregate.View(), input.template, candidate, input.parallel != nil)
		if err != nil {
			return ExclusiveObservation{}, false, err
		}
		if disposition == ExclusiveRetryPending || disposition == ExclusiveResolvedRetry {
			continue
		}
		if disposition != ExclusiveRouteReady {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: pending observation disposition %q requires explicit integration", ErrExclusiveUnsupported, disposition)
		}
		pending = append(pending, pendingCandidate{observation: candidate, settlementID: command.ID})
	}
	if len(pending) == 0 {
		return ExclusiveObservation{}, false, nil
	}
	if len(pending) > 1 && input.parallel == nil {
		return ExclusiveObservation{}, false, fmt.Errorf("%w: multiple pending exclusive observations", ErrMutationInconsistent)
	}
	slices.SortFunc(pending, func(a, b pendingCandidate) int {
		if order := strings.Compare(a.observation.SourcePathID, b.observation.SourcePathID); order != 0 {
			return order
		}
		if a.observation.Attempt < b.observation.Attempt {
			return -1
		}
		if a.observation.Attempt > b.observation.Attempt {
			return 1
		}
		return strings.Compare(a.settlementID, b.settlementID)
	})
	for index := 1; index < len(pending); index++ {
		if pending[index-1].observation.SourcePathID == pending[index].observation.SourcePathID {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: multiple pending observations own source path %q", ErrMutationInconsistent, pending[index].observation.SourcePathID)
		}
	}
	return pending[0].observation, true, nil
}

// AdvanceExclusiveRoute deterministically derives the sole pending durable
// observation from the verified checkpoint, then folds it through route,
// local DPE, activation, and a directly reached end node. Callers cannot
// supply observation authority to this persistence-authorizing constructor.
func AdvanceExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	observation, found, err := PendingExclusiveObservation(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no pending durable observation is routable", ErrExclusiveNotRoutable)
	}
	return advanceExclusiveRoute(ctx, input, observation)
}

func advanceExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (*ExecutionTransition, error) {
	sequence, err := PlanExclusiveRouteSequence(ctx, input, observation)
	if err != nil {
		return nil, err
	}
	commands := sequence.Commands()
	projection := &ExclusiveProjection{
		aggregate: sequence.final, binding: input.binding,
		command: commands[len(commands)-1], dispose: ReplayApplied,
	}
	lastLogSeq, err := aggregateLogicalLastSeq(projection.aggregate)
	if err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7To(input.checkpoint, projection.aggregate, CurrentRunStatus(input.checkpoint), lastLogSeq)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionRouteObservation)
}

func aggregateLogicalLastSeq(aggregate AggregateCheckpoint) (uint64, error) {
	var maximum int64
	add := func(sequence int64) error {
		if sequence < 0 {
			return fmt.Errorf("%w: negative aggregate event sequence", ErrMutationInconsistent)
		}
		if sequence > maximum {
			maximum = sequence
		}
		return nil
	}
	for _, path := range aggregate.Routing.Paths {
		for _, sequence := range []int64{path.CreatedSeq, path.UpdatedSeq, path.ArrivedSeq} {
			if err := add(sequence); err != nil {
				return 0, err
			}
		}
		if path.Disposition != nil {
			if err := add(path.Disposition.EventSeq); err != nil {
				return 0, err
			}
		}
		if path.DetachedSink != nil {
			if err := add(path.DetachedSink.EventSeq); err != nil {
				return 0, err
			}
		}
	}
	for _, scope := range aggregate.Routing.Scopes {
		if err := add(scope.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, reservation := range aggregate.Routing.Reservations {
		if err := add(reservation.EventSeq); err != nil {
			return 0, err
		}
		if reservation.CloseReceipt != nil {
			if err := add(reservation.CloseReceipt.EventSeq); err != nil {
				return 0, err
			}
		}
	}
	for _, activation := range aggregate.Routing.Activations {
		if err := add(activation.EventSeq); err != nil {
			return 0, err
		}
		if err := add(activation.Receipt.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, cause := range aggregate.Routing.CauseRecords {
		if err := add(cause.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, closure := range aggregate.Routing.CandidateClosures {
		if err := add(closure.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, detachment := range aggregate.Routing.Detachments {
		if err := add(detachment.EventSeq); err != nil {
			return 0, err
		}
		if err := add(detachment.ActivatedSeq); err != nil {
			return 0, err
		}
	}
	for _, propagation := range aggregate.Routing.Propagation {
		if err := add(propagation.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, admin := range aggregate.AdminRecords {
		if err := add(admin.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, contact := range aggregate.Contacts {
		if err := add(contact.EventSeq); err != nil {
			return 0, err
		}
	}
	return uint64(maximum), nil
}

// AdvanceExclusiveStart routes an instantaneous start node without creating
// adapter dispatch authority.
func AdvanceExclusiveStart(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID) (*ExecutionTransition, error) {
	if input == nil || input.checkpoint == nil || input.template == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	source, ok := aggregate.Routing.Paths[sourcePathID]
	activation := aggregate.Routing.Activations[source.SourceActivation.ID]
	reservation := aggregate.Routing.Reservations[activation.ReservationID]
	node, nodeOK := input.template.Nodes[reservation.NodeID]
	if !ok || source.State != PathLive || !nodeOK || node.Type != model.NodeTypeStart {
		return nil, fmt.Errorf("%w: source is not a live instantaneous start", ErrExclusiveUnsupported)
	}
	return advanceExclusiveRoute(ctx, input, ExclusiveObservation{SourcePathID: sourcePathID, Attempt: 1, Outcome: "pass"})
}

func completionReplayView(checkpoint *CheckpointV7, aggregate AggregateCheckpoint, selfCommandID string) (CompletionReplayView, error) {
	checkpointJSON, err := CompletionCheckpointJSON(checkpoint, selfCommandID)
	if err != nil {
		return CompletionReplayView{}, err
	}
	view := CompletionReplayView{
		Aggregate: aggregate.View(), CheckpointJSON: checkpointJSON,
		RunStatus: CurrentRunStatus(checkpoint), LastLogSeq: CurrentLastLogSeq(checkpoint), LogChecksum: CurrentLogChecksum(checkpoint),
		Checkpoint: CurrentCheckpointBinding(checkpoint),
	}
	basis, err := computeCompletionBasis(view, CompletionBasis{
		BasisRunStatus: view.RunStatus, BasisLastLogSeq: view.LastLogSeq, BasisLogChecksum: view.LogChecksum,
	}, selfCommandID)
	if err != nil {
		return CompletionReplayView{}, err
	}
	view.Checkpoint = completionBasisCheckpoint(basis)
	return view, nil
}

// ClaimExclusiveCompletion durably installs the sole completion self-command
// against the exact current checkpoint basis.
func ClaimExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view, err := completionReplayView(input.checkpoint, aggregate, "")
	if err != nil {
		return nil, err
	}
	recovery, err := RecoverCompleteRun(view)
	if err != nil || recovery.Phase != CompletionReadyToClaim {
		return nil, fmt.Errorf("%w: completion is not ready to claim: %v", ErrMutationInconsistent, err)
	}
	aggregate.Commands[recovery.Command.ID] = cloneCommandRecord(recovery.Command)
	next, err := advanceCheckpointV7PreservingLog(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionClaimCompletion)
}

// ObserveExclusiveCompletion atomically marks the completion self-command and
// the run status from the same validated completion basis.
func ObserveExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	var self string
	for id, command := range aggregate.Commands {
		if command.Identity.Kind == CommandCompleteRun && command.State.Active() {
			if self != "" {
				return nil, fmt.Errorf("%w: multiple active completion commands", ErrMutationInconsistent)
			}
			self = id
		}
	}
	if self == "" {
		return nil, fmt.Errorf("%w: active completion command is absent", ErrMutationInconsistent)
	}
	view, err := completionReplayView(input.checkpoint, aggregate, self)
	if err != nil {
		return nil, err
	}
	recovery, err := RecoverCompleteRun(view)
	if err != nil || recovery.Phase != CompletionReadyToObserve {
		return nil, fmt.Errorf("%w: completion is not ready to observe: %v", ErrMutationInconsistent, err)
	}
	command := aggregate.Commands[self]
	command.State = CommandObserved
	aggregate.Commands[self] = command
	completeContactsForSettledCommand(&aggregate, self, false, int64(CurrentLastLogSeq(input.checkpoint))+1)
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, recovery.Result)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionObserveCompletion)
}

func cloneExclusivePerformer(performer *model.Performer) *model.Performer {
	if performer == nil {
		return nil
	}
	clone := *performer
	clone.Choices = append([]string(nil), performer.Choices...)
	clone.Args = append([]string(nil), performer.Args...)
	if performer.ChoiceOutcomes != nil {
		clone.ChoiceOutcomes = make(map[string]string, len(performer.ChoiceOutcomes))
		for choice, outcome := range performer.ChoiceOutcomes {
			clone.ChoiceOutcomes[choice] = outcome
		}
	}
	if performer.Contact != nil {
		contact := *performer.Contact
		clone.Contact = &contact
	}
	return &clone
}

func cloneExclusiveParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	clone := make(map[string]string, len(params))
	for key, value := range params {
		clone[key] = value
	}
	return clone
}

func performCommandPayload(view AggregateView, nodeID string, source PathRecord, attempt uint64, performer *model.Performer, params map[string]string) ([]byte, error) {
	return json.Marshal(performAttemptPayload{
		TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash, NodeID: nodeID,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: attempt,
		Performer: cloneExclusivePerformer(performer), Params: cloneExclusiveParams(params),
	})
}

func commandWithState(identity CommandIdentity, payload []byte, state CommandState) (CommandRecord, error) {
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, err
	}
	command.State = state
	if err := ValidateCommand(command); err != nil {
		return CommandRecord{}, err
	}
	return command, nil
}
