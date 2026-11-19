package executor

import (
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// How a wait step ended, as its wait_finished evidence says.
const (
	// WaitElapsed: its deadline passed.
	WaitElapsed = "elapsed"
	// WaitSkipped: someone with process.runs.manage ended it early.
	WaitSkipped = "skipped"
	// WaitFailed: its deadline could not be worked out when it started.
	WaitFailed = "failed"
	// WaitWithdrawn: the run was given up while it waited.
	WaitWithdrawn = "withdrawn"
)

// ErrInvalidWaitFinish refuses a wait finish that names no outstanding wait,
// or that is not one of the ways a wait ends.
var ErrInvalidWaitFinish = errors.New("invalid wait finish")

// WaitFinish ends one outstanding wait step's timer. The daemon owns the
// clock: it says when the wait started and was due, and how it ended.
type WaitFinish struct {
	CommandID string
	NodeID    string
	Attempt   int
	// How is WaitElapsed, WaitSkipped, WaitFailed or WaitWithdrawn.
	How string
	// Reason is why a failed wait failed. It names the step's problem.
	Reason    string
	StartedAt time.Time
	DueAt     time.Time
}

// waitFinishedEvidence is the wait_finished event's payload: how the wait
// ended, and its window, so a finished step can say "waited 3 days".
type waitFinishedEvidence struct {
	Attempt   int        `json:"attempt"`
	How       string     `json:"how"`
	Reason    string     `json:"reason,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
}

// FinishWait settles one outstanding wait command through the observation
// transition every task uses: elapsed and skipped succeed along the step's
// route; failed and withdrawn fail the step, which (a wait has no retries)
// fails the run naming the step. A finish for a wait no longer outstanding,
// say one a cancel or a send-back already withdrew, is refused as
// engine.ErrStaleObservation without touching state.
func FinishWait(run *Run, actor string, finish WaitFinish) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	command, ok := findCommand(run.checkpoint, finish.CommandID, finish.NodeID)
	if !ok || command.Kind != engine.CommandWait || command.Attempt != finish.Attempt {
		return fmt.Errorf("%w: no outstanding wait matches %s attempt %d", engine.ErrStaleObservation, finish.NodeID, finish.Attempt)
	}
	observation := engine.ProgramObservation{CommandID: command.ID, NodeID: command.NodeID}
	switch finish.How {
	case WaitElapsed, WaitSkipped:
		observation.Outcome = engine.ProgramSucceeded
		// A wait for a criterion reads when its timer started and was due.
		observation.Wait = &engine.WaitObservation{How: finish.How, StartedAt: finish.StartedAt.UTC(), DueAt: finish.DueAt.UTC()}
	case WaitFailed, WaitWithdrawn:
		observation.Outcome, observation.ExitCode = engine.ProgramFailed, 1
		observation.Error = truncateWaitReason(finish.Reason)
		if observation.Error == "" {
			observation.Error = "the wait ended without finishing"
		}
	default:
		return fmt.Errorf("%w: unknown way for a wait to end %q", ErrInvalidWaitFinish, finish.How)
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionProgramObserved, Observation: &observation,
	})
	if err != nil {
		return err
	}
	evidence := waitFinishedEvidence{Attempt: command.Attempt, How: finish.How, Reason: observation.Error}
	if !finish.StartedAt.IsZero() {
		at := finish.StartedAt.UTC()
		evidence.StartedAt = &at
	}
	if !finish.DueAt.IsZero() {
		at := finish.DueAt.UTC()
		evidence.DueAt = &at
	}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{event("wait_finished", &command, actor, evidence)}, nil, false)
	return persistEvents(run, next, events)
}

// truncateWaitReason bounds a failure reason like any step failure's.
func truncateWaitReason(reason string) string {
	if len(reason) <= engine.MaxBlockedReasonBytes {
		return reason
	}
	cut := engine.MaxBlockedReasonBytes
	for cut > 0 && reason[cut]&0xC0 == 0x80 {
		cut--
	}
	return reason[:cut]
}
