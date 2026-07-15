package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// ExecutionTransition is a sealed exact successor. Its bytes can only be
// created by path-v1 planner/reducer constructors; callers cannot inject an
// aggregate into the persistence boundary.
type ExecutionTransition struct {
	pre       CheckpointBinding
	post      CheckpointBinding
	postBytes []byte
	kind      string
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
	if _, err := VerifyExclusiveInput(ctx, currentBytes, templateSource); err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	pre := CurrentCheckpointBinding(current)
	postBytes := bytes.Clone(transition.postBytes)
	post, err := DecodeCheckpointV7(postBytes)
	if err != nil {
		return pre, nil, nil, err
	}
	if _, err := VerifyExclusiveInput(ctx, postBytes, templateSource); err != nil {
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
	return pre, postBytes, post, nil
}

// ExclusiveAttemptPlan is a sealed exact perform_attempt_v1 command plus the
// adapter request authority derived from the exact current template.
type ExclusiveAttemptPlan struct {
	command   CommandRecord
	nodeID    string
	performer *model.Performer
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

func PlanExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID, attempt uint64) (*ExclusiveAttemptPlan, error) {
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
	for _, command := range view.Commands {
		if command.State.Active() {
			return nil, fmt.Errorf("%w: active command %q must recover before planning", ErrMutationInconsistent, command.ID)
		}
	}
	source, ok := view.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return nil, fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return nil, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
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
	payload, err := performCommandPayload(view, reservation.NodeID, source, attempt)
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
	return &ExclusiveAttemptPlan{command: command, nodeID: reservation.NodeID, performer: cloneExclusivePerformer(node.Performer)}, nil
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
	replanned, err := PlanExclusiveAttempt(ctx, input, source.ID, plan.command.Identity.Attempt)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(replanned.command, plan.command) || replanned.nodeID != plan.nodeID {
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
	return newExecutionTransition(input.checkpoint, next, "claim_attempt")
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

func performCommandPayload(view AggregateView, nodeID string, source PathRecord, attempt uint64) ([]byte, error) {
	return json.Marshal(performAttemptPayload{
		TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash, NodeID: nodeID,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: attempt,
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
