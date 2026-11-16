package executor

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// MarkBlockInput parks an agent step that is out on a person, with why.
// Attempt is the attempt the person saw out, so a mark formed against a step
// that has since moved on is refused.
type MarkBlockInput struct {
	NodeID  string
	Attempt int
	Reason  string
}

// CancelRun gives the whole run up. Work still out drains: agent steps are
// withdrawn by the daemon, programs are stopped, and the run ends canceled
// once the last of them is accounted for.
func CancelRun(run *Run, actor, note string) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if err := validateSteerText(note, MaxResolutionNoteBytes, false); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{Kind: engine.TransitionRunCanceled})
	if err != nil {
		return err
	}
	payload := struct {
		Note string `json:"note,omitempty"`
	}{Note: note}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{event("run_canceled", nil, actor, payload)}, nil, false)
	return persistEvents(run, next, events)
}

// MarkBlocked parks an agent step that is out. Its command is withdrawn, and
// the branch then waits on the ordinary retry/skip/cancel resolution.
func MarkBlocked(run *Run, actor string, input MarkBlockInput) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if input.NodeID == "" || len(input.NodeID) > MaxDecisionNodeIDBytes || !utf8.ValidString(input.NodeID) {
		return fmt.Errorf("%w: node id must be 1..%d bytes of UTF-8", ErrInvalidResolutionInput, MaxDecisionNodeIDBytes)
	}
	if input.Attempt < 1 {
		return fmt.Errorf("%w: attempt must be the positive attempt the step is out at", ErrInvalidResolutionInput)
	}
	if err := validateSteerText(input.Reason, engine.MaxBlockedReasonBytes, true); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionBlockMarked,
		Mark: &engine.BlockMark{NodeID: input.NodeID, Attempt: input.Attempt, Reason: input.Reason},
	})
	if err != nil {
		return err
	}
	payload := struct {
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
		Reason  string `json:"reason"`
	}{NodeID: input.NodeID, Attempt: input.Attempt, Reason: input.Reason}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{eventForNode("block_marked", input.NodeID, actor, payload)}, nil, false)
	return persistEvents(run, next, events)
}

// validateSteerText bounds a person's note or reason: UTF-8, line breaks
// and tabs allowed, no other control characters.
func validateSteerText(text string, limit int, required bool) error {
	if required && text == "" {
		return fmt.Errorf("%w: a reason is required", ErrInvalidResolutionInput)
	}
	if len(text) > limit || !utf8.ValidString(text) {
		return fmt.Errorf("%w: text must be at most %d bytes of UTF-8", ErrInvalidResolutionInput, limit)
	}
	for _, value := range text {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return fmt.Errorf("%w: text cannot contain control characters other than line breaks and tabs", ErrInvalidResolutionInput)
		}
	}
	return nil
}
