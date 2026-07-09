package model

import "fmt"

// Compound task nodes expand into explicit child stages. The stage vocabulary
// follows the design doc: <id>.plan -> <id>.do -> <id>.test.<step> ->
// <id>.review -> <id>.done, with an optional <id>.plan.approval gate between
// plan and do when the plan step declares approval: human.
type StageKind string

const (
	StagePlan         StageKind = "plan"
	StagePlanApproval StageKind = "plan.approval"
	StageDo           StageKind = "do"
	StageTest         StageKind = "test"
	StageReview       StageKind = "review"
	StageDone         StageKind = "done"
)

func (k StageKind) IsValid() bool {
	switch k {
	case StagePlan, StagePlanApproval, StageDo, StageTest, StageReview, StageDone:
		return true
	default:
		return false
	}
}

// IsGateStage reports whether the stage renders a verdict over prior work
// rather than producing the work itself.
func (k StageKind) IsGateStage() bool {
	switch k {
	case StagePlanApproval, StageTest, StageReview:
		return true
	default:
		return false
	}
}

const (
	PlanApprovalHuman = "human"
	PlanApprovalAuto  = "auto"
)

const (
	RetryModeFeedbackSameSession = "feedback-same-session"
	RetryModeFreshAttempt        = "fresh-attempt"
	// DefaultRetryMode is used when retry.onFail is unset: a fresh attempt is
	// the conservative choice because it never trusts a possibly-poisoned
	// performer context.
	DefaultRetryMode = RetryModeFreshAttempt
)

// RetryMode resolves the on-fail retry mode policy axis for a retry policy.
func RetryMode(retry *RetryPolicy) string {
	if retry == nil || retry.OnFail == "" {
		return DefaultRetryMode
	}
	return retry.OnFail
}

// RetryBudget resolves a retry policy's declared budget, default 1. For work
// stages it bounds attempts; for gates it bounds failed verdicts in a loop
// window. Planner and verify invariants must agree on this rule, so it lives
// here as the single source of truth.
func RetryBudget(retry *RetryPolicy) int {
	if retry != nil && retry.MaxAttempts > 0 {
		return retry.MaxAttempts
	}
	return 1
}

// StageSpec describes one derived child of a compound task node. ChildID is
// the fully qualified state node id (parent id + "." + stage path).
type StageSpec struct {
	ChildID   string
	Stage     StageKind
	StepID    string
	Performer *Performer
	Retry     *RetryPolicy
}

// IsCompound reports whether a task node declares compound stages and
// therefore expands into child stage nodes at activation.
func (n Node) IsCompound() bool {
	return n.Type == NodeTypeTask && (n.Plan != nil || len(n.Checks) > 0 || n.Review != nil)
}

// ExpandNode derives the ordered child stages of a compound task node. The
// derivation is a pure function of the template so the reducer-recorded
// expansion can always be re-checked against the pinned template.
func ExpandNode(nodeID string, node Node) []StageSpec {
	if !node.IsCompound() {
		return nil
	}
	var specs []StageSpec
	if node.Plan != nil {
		plan := *node.Plan
		specs = append(specs, StageSpec{
			ChildID:   stageChildID(nodeID, StagePlan, ""),
			Stage:     StagePlan,
			Performer: &plan.Performer,
			Retry:     plan.Retry,
		})
		if plan.Approval == PlanApprovalHuman {
			specs = append(specs, StageSpec{
				ChildID: stageChildID(nodeID, StagePlanApproval, ""),
				Stage:   StagePlanApproval,
				Performer: &Performer{
					Kind: PerformerHuman,
					Ask:  fmt.Sprintf("Approve the plan for node %q?", nodeID),
				},
				Retry: plan.ApprovalRetry,
			})
		}
	}
	specs = append(specs, StageSpec{
		ChildID:   stageChildID(nodeID, StageDo, ""),
		Stage:     StageDo,
		Performer: node.Performer,
		Retry:     node.Retry,
	})
	for _, check := range node.Checks {
		step := check
		specs = append(specs, StageSpec{
			ChildID:   stageChildID(nodeID, StageTest, step.ID),
			Stage:     StageTest,
			StepID:    step.ID,
			Performer: &step.Performer,
			Retry:     step.Retry,
		})
	}
	if node.Review != nil {
		review := *node.Review
		specs = append(specs, StageSpec{
			ChildID:   stageChildID(nodeID, StageReview, ""),
			Stage:     StageReview,
			Performer: &review.Performer,
			Retry:     review.Retry,
		})
	}
	specs = append(specs, StageSpec{
		ChildID: stageChildID(nodeID, StageDone, ""),
		Stage:   StageDone,
	})
	return specs
}

func stageChildID(nodeID string, stage StageKind, stepID string) string {
	if stage == StageTest {
		return nodeID + "." + string(StageTest) + "." + stepID
	}
	return nodeID + "." + string(stage)
}
