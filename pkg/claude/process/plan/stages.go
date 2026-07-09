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
	// TransitionPoison blocks the child and its parent with reason and owner.
	TransitionPoison StageTransitionKind = "poison"
)

type StageTransition struct {
	Kind        StageTransitionKind
	NextChildID string
	DoneChildID string
	Reason      string
	Owner       string
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

// NextAfterStage decides what follows a stage child settling with the given
// effective outcome on the given attempt. In this phase gates carry no
// feedback loops yet: a failed gate exhausts its budget immediately and
// poisons the node (blocked, never auto-failed run).
func NextAfterStage(parentID string, children []string, specs []model.StageSpec, childID, outcome string, attempt int) (StageTransition, error) {
	index := -1
	for i, id := range children {
		if id == childID {
			index = i
			break
		}
	}
	if index < 0 || index >= len(specs) {
		return StageTransition{}, fmt.Errorf("stage child %q is not part of node %q expansion", childID, parentID)
	}
	spec := specs[index]
	if spec.Stage == model.StageDone {
		return StageTransition{}, fmt.Errorf("done stage %q settles automatically and cannot be advanced", childID)
	}
	if state.IsPassOutcome(outcome) {
		next := index + 1
		if next >= len(children) {
			return StageTransition{}, fmt.Errorf("stage child %q has no successor stage", childID)
		}
		if specs[next].Stage == model.StageDone {
			return StageTransition{Kind: TransitionCompleteParent, DoneChildID: children[next]}, nil
		}
		return StageTransition{Kind: TransitionActivateChild, NextChildID: children[next]}, nil
	}
	if spec.Stage.IsGateStage() {
		return StageTransition{
			Kind:   TransitionPoison,
			Reason: fmt.Sprintf("gate %q failed on attempt %d", childID, attempt),
			Owner:  DefaultBlockedOwner,
		}, nil
	}
	budget := maxAttempts(spec.Retry)
	if attempt < budget {
		return StageTransition{Kind: TransitionRetryChild}, nil
	}
	return StageTransition{
		Kind:   TransitionPoison,
		Reason: fmt.Sprintf("stage %q failed and exhausted its budget of %d attempts", childID, budget),
		Owner:  DefaultBlockedOwner,
	}, nil
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
