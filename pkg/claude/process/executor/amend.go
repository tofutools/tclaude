package executor

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// ErrAmendmentStale refuses an amendment proposed against a run that has
// moved on since: its frontier may not be the one the proposer saw.
var ErrAmendmentStale = errors.New("process run moved on since the amendment was proposed")

// AmendInput applies one stored amendment to the run.
type AmendInput struct {
	AmendmentID int64
	// BaseStateVersion is the run state the amendment was proposed against.
	BaseStateVersion int64
	Template         *model.Template
	Note             string
}

// AmendedPayload is the run_amended evidence.
type AmendedPayload struct {
	Amendment int64               `json:"amendment"`
	Summary   engine.AmendSummary `json:"summary"`
	Note      string              `json:"note,omitempty"`
}

// CurrentTemplate is the run's template as it stands: its pinned snapshot,
// or its last applied amendment.
func CurrentTemplate(runID string) (*model.Template, map[string]string, error) {
	record, err := db.GetProcessRun(runID)
	if err != nil {
		return nil, nil, err
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return nil, nil, fmt.Errorf("%w: decode template snapshot: %v", ErrInvalidRun, err)
	}
	var params map[string]string
	if err := record.DecodeParams(&params); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRun, err)
	}
	return &tmpl, params, nil
}

// PlanAmendment checks an amended template against the run as it is now,
// without changing anything: the new checkpoint and what changed, or why the
// run cannot take it.
func PlanAmendment(run *Run, amended *model.Template) (engine.Checkpoint, *engine.Definition, engine.AmendSummary, error) {
	if run == nil || run.definition == nil || amended == nil {
		return engine.Checkpoint{}, nil, engine.AmendSummary{}, ErrInvalidRun
	}
	current, params, err := CurrentTemplate(run.id)
	if err != nil {
		return engine.Checkpoint{}, nil, engine.AmendSummary{}, err
	}
	definition, err := engine.Prepare(amended, params)
	if err != nil {
		return engine.Checkpoint{}, nil, engine.AmendSummary{}, fmt.Errorf("%w: %v", engine.ErrAmendmentRefused, err)
	}
	next, summary, err := engine.Amend(run.checkpoint, run.definition, current, amended, definition)
	return next, definition, summary, err
}

// Amend applies a stored amendment: the run's template becomes its copy, and
// its checkpoint carries over, in one transaction. A run that moved on since
// the amendment was proposed refuses it with ErrAmendmentStale.
func Amend(run *Run, actor string, input AmendInput) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	if run.stateVersion != input.BaseStateVersion {
		return fmt.Errorf("%w: it was proposed at state %d and the run is at %d; re-propose it against the run as it is",
			ErrAmendmentStale, input.BaseStateVersion, run.stateVersion)
	}
	next, definition, summary, err := PlanAmendment(run, input.Template)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(input.Template)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode process checkpoint: %w", err)
	}
	events := []db.ProcessRunEvent{event("run_amended", nil, actor, AmendedPayload{
		Amendment: input.AmendmentID, Summary: summary, Note: input.Note,
	})}
	version, err := db.TransitionProcessRun(run.id, db.ProcessRunTransition{
		ExpectedStateVersion: run.stateVersion,
		Status:               string(next.Status),
		CheckpointJSON:       encoded,
		Events:               events,
		Amendment:            &db.ProcessRunAmendmentApply{ID: input.AmendmentID, By: actor, TemplateJSON: snapshot},
	})
	if err != nil {
		return err
	}
	waited := waitsOnPerson(run.checkpoint)
	run.stateVersion = version
	run.checkpoint = next
	run.definition = definition
	// An amendment can change, remove or add what the run waits on a person
	// for, and a run parked on a person may make no other commit soon.
	if waited || waitsOnPerson(next) {
		fireWaitingHook(run.id)
	}
	return nil
}
