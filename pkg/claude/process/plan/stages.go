package plan

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

// DefaultBlockedOwner is who owns a poisoned (budget-exhausted) node until a
// decision node or repair flow reassigns it. Deliberately a plain default for
// now; making it configurable is a later, explicit decision.
const DefaultBlockedOwner = "human:operator"

// StageTransitionKind enumerates what follows a settled stage child. Both the
// planner (state -> commands) and the manual-advance CLI (verdict -> events)
// map from the same transition so their gate semantics cannot drift.
type StageTransitionKind string

const (
	// TransitionActivateChild readies the next stage child in the chain.
	TransitionActivateChild StageTransitionKind = "activate_child"
	// TransitionRetryChild re-readies the same child for another attempt.
	TransitionRetryChild StageTransitionKind = "retry_child"
	// TransitionCompleteParent completes the done marker and the parent.
	TransitionCompleteParent StageTransitionKind = "complete_parent"
	// TransitionFeedbackLoop routes a failed gate's payload back to its work
	// stage and re-enters the gate span: the target stage re-readies with
	// pending feedback, the span's gates reset to pending, and cross-kind
	// gates in the span reset their fail counters.
	TransitionFeedbackLoop StageTransitionKind = "feedback_loop"
	// TransitionPoison blocks the child and its parent with reason and owner.
	TransitionPoison StageTransitionKind = "poison"
)

type StageTransition struct {
	Kind        StageTransitionKind
	NextChildID string
	DoneChildID string
	Reason      string
	Owner       string

	// Feedback-loop fields.
	TargetStageID string
	ResetGates    []string
	ResetCounters []string
	Feedback      string
	EvidenceRef   string
}

// StageSettle describes a stage child's settle for transition purposes.
// FailCount must INCLUDE the settling failure (the reducer increments the
// gate's counter in the same event, so planner callers pass the recorded
// count and CLI callers pass count+1).
type StageSettle struct {
	ChildID     string
	Outcome     string
	Attempt     int
	FailCount   int
	Feedback    string
	EvidenceRef string
}

// CompoundSpecs derives the stage specs for a state node's parent template
// node and checks that the recorded expansion still matches the template.
func CompoundSpecs(tmpl *model.Template, parentID string, parent state.NodeState) ([]model.StageSpec, error) {
	templateNode, ok := tmpl.Nodes[parentID]
	if !ok {
		return nil, fmt.Errorf("compound parent %q is not in template", parentID)
	}
	specs := model.ExpandNode(parentID, templateNode)
	if len(specs) == 0 {
		return nil, fmt.Errorf("node %q records an expansion but template node is not compound", parentID)
	}
	if len(parent.Children) != len(specs) {
		return nil, fmt.Errorf("node %q records %d children but template derives %d; run verify", parentID, len(parent.Children), len(specs))
	}
	for i, spec := range specs {
		if parent.Children[i] != spec.ChildID {
			return nil, fmt.Errorf("node %q child %q does not match template-derived child %q; run verify", parentID, parent.Children[i], spec.ChildID)
		}
	}
	return specs, nil
}

// ExpansionInits converts derived stage specs into the child node inits
// recorded by a node_expanded event.
func ExpansionInits(parentID string, specs []model.StageSpec) []state.NodeInit {
	inits := make([]state.NodeInit, 0, len(specs))
	for _, spec := range specs {
		inits = append(inits, state.NodeInit{
			ID:     spec.ChildID,
			Parent: parentID,
			Stage:  spec.Stage,
			StepID: spec.StepID,
		})
	}
	return inits
}

// StageMaxAttempts is the per-settle attempt budget of a stage child. Gate
// settles are always terminal for their loop window (a failed gate never
// re-readies itself — the feedback loop re-enters it via pending), so a gate
// settle carries maxAttempts 1; the gate's declared retry budget bounds its
// failed VERDICTS and is enforced by NextAfterStage against FailCount.
func StageMaxAttempts(spec model.StageSpec) int {
	if spec.Stage.IsGateStage() {
		return 1
	}
	return maxAttempts(spec.Retry)
}

// GateBudget is the number of failed verdicts a gate may produce in its loop
// window before it poisons the node.
func GateBudget(spec model.StageSpec) int {
	return maxAttempts(spec.Retry)
}

// NextAfterStage decides what follows a stage child settling with the given
// effective outcome. Gate failures within budget route feedback back to their
// work stage (bounded additionally by the work stage's own attempt budget);
// exhausted budgets poison the node (blocked, never an auto-failed run).
func NextAfterStage(parentID string, children []string, specs []model.StageSpec, nodes map[string]state.NodeState, settle StageSettle) (StageTransition, error) {
	index := -1
	for i, id := range children {
		if id == settle.ChildID {
			index = i
			break
		}
	}
	if index < 0 || index >= len(specs) {
		return StageTransition{}, fmt.Errorf("stage child %q is not part of node %q expansion", settle.ChildID, parentID)
	}
	spec := specs[index]
	if spec.Stage == model.StageDone {
		return StageTransition{}, fmt.Errorf("done stage %q settles automatically and cannot be advanced", settle.ChildID)
	}
	if state.IsPassOutcome(settle.Outcome) {
		next := index + 1
		if next >= len(children) {
			return StageTransition{}, fmt.Errorf("stage child %q has no successor stage", settle.ChildID)
		}
		if specs[next].Stage == model.StageDone {
			return StageTransition{Kind: TransitionCompleteParent, DoneChildID: children[next]}, nil
		}
		return StageTransition{Kind: TransitionActivateChild, NextChildID: children[next]}, nil
	}
	if spec.Stage.IsGateStage() {
		return gateFailTransition(children, specs, nodes, settle, index)
	}
	budget := maxAttempts(spec.Retry)
	if settle.Attempt < budget {
		return StageTransition{Kind: TransitionRetryChild}, nil
	}
	return StageTransition{
		Kind:   TransitionPoison,
		Reason: fmt.Sprintf("stage %q failed and exhausted its budget of %d attempts", settle.ChildID, budget),
		Owner:  DefaultBlockedOwner,
	}, nil
}

func gateFailTransition(children []string, specs []model.StageSpec, nodes map[string]state.NodeState, settle StageSettle, index int) (StageTransition, error) {
	spec := specs[index]
	budget := GateBudget(spec)
	if settle.FailCount >= budget {
		return StageTransition{
			Kind:   TransitionPoison,
			Reason: fmt.Sprintf("gate %q exhausted its budget of %d failed verdicts", settle.ChildID, budget),
			Owner:  DefaultBlockedOwner,
		}, nil
	}
	targetIndex := feedbackTargetIndex(specs, index)
	if targetIndex < 0 {
		return StageTransition{}, fmt.Errorf("gate %q has no feedback target stage", settle.ChildID)
	}
	targetID := children[targetIndex]
	target, ok := nodes[targetID]
	if !ok {
		return StageTransition{}, fmt.Errorf("feedback target %q is not in state", targetID)
	}
	workBudget := maxAttempts(specs[targetIndex].Retry)
	if target.Attempt >= workBudget {
		return StageTransition{
			Kind:   TransitionPoison,
			Reason: fmt.Sprintf("gate %q failed but stage %q has exhausted its budget of %d attempts", settle.ChildID, targetID, workBudget),
			Owner:  DefaultBlockedOwner,
		}, nil
	}
	transition := StageTransition{
		Kind:          TransitionFeedbackLoop,
		TargetStageID: targetID,
		Reason:        fmt.Sprintf("gate %q failed on attempt %d; feedback re-enters %q", settle.ChildID, settle.Attempt, targetID),
		Feedback:      settle.Feedback,
		EvidenceRef:   settle.EvidenceRef,
	}
	// The gate span between the work stage and the failing gate re-enters:
	// the work change invalidates every verdict in the span. Cross-KIND gates
	// in the span additionally reset their fail counters (review-triggered
	// rework restarts the testing window; same-kind re-runs share a window).
	for i := targetIndex + 1; i <= index; i++ {
		if !specs[i].Stage.IsGateStage() {
			continue
		}
		transition.ResetGates = append(transition.ResetGates, children[i])
		if i < index && specs[i].Stage != spec.Stage {
			transition.ResetCounters = append(transition.ResetCounters, children[i])
		}
	}
	return transition, nil
}

// feedbackTargetIndex resolves which earlier stage a gate's feedback re-enters:
// the plan approval gate re-enters the plan stage, test and review gates
// re-enter the do stage.
func feedbackTargetIndex(specs []model.StageSpec, gateIndex int) int {
	want := model.StageDo
	if specs[gateIndex].Stage == model.StagePlanApproval {
		want = model.StagePlan
	}
	for i := gateIndex - 1; i >= 0; i-- {
		if specs[i].Stage == want {
			return i
		}
	}
	return -1
}

// ShortCircuitHash reports whether a re-entering gate may declare "evidence
// unchanged, previous verdict stands": it has a prior verdict, and the work
// stage's settled evidence hash matches the hash the prior verdict evaluated.
// It returns that hash when eligible.
func ShortCircuitHash(children []string, specs []model.StageSpec, nodes map[string]state.NodeState, gateID string) (string, bool) {
	index := -1
	for i, id := range children {
		if id == gateID {
			index = i
			break
		}
	}
	if index < 0 || index >= len(specs) || !specs[index].Stage.IsGateStage() {
		return "", false
	}
	gate, ok := nodes[gateID]
	if !ok || len(gate.Decisions) == 0 || gate.LastEvidenceHash == "" {
		return "", false
	}
	targetIndex := feedbackTargetIndex(specs, index)
	if targetIndex < 0 {
		return "", false
	}
	target, ok := nodes[children[targetIndex]]
	if !ok || target.ActiveAttempt == nil || target.ActiveAttempt.SettledAt.IsZero() {
		return "", false
	}
	hash := target.ActiveAttempt.EvidenceHash
	if hash == "" || hash != gate.LastEvidenceHash {
		return "", false
	}
	return hash, true
}

// WorkEvidenceHash resolves the settled evidence hash of a gate's feedback
// target stage — the hash the gate's verdict is about.
func WorkEvidenceHash(children []string, specs []model.StageSpec, nodes map[string]state.NodeState, gateID string) string {
	index := -1
	for i, id := range children {
		if id == gateID {
			index = i
			break
		}
	}
	if index < 0 || index >= len(specs) {
		return ""
	}
	targetIndex := feedbackTargetIndex(specs, index)
	if targetIndex < 0 {
		return ""
	}
	target, ok := nodes[children[targetIndex]]
	if !ok || target.ActiveAttempt == nil {
		return ""
	}
	return target.ActiveAttempt.EvidenceHash
}

// EffectiveStageOutcome normalizes a settled stage child's status and recorded
// outcome into the verdict NextAfterStage expects: a child whose status is
// failed is a failure even when its recorded outcome claims pass (the
// claimed-done-without-evidence flip).
func EffectiveStageOutcome(node state.NodeState) string {
	if node.Status == state.NodeStatusFailed {
		return "fail"
	}
	if node.ActiveAttempt != nil && strings.TrimSpace(node.ActiveAttempt.Outcome) != "" {
		return strings.ToLower(strings.TrimSpace(node.ActiveAttempt.Outcome))
	}
	return "pass"
}
