package executor

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// MaxAgentReportNoteBytes bounds the free-text note an agent performer sends
// with its report. It is evidence and, on failure, the parked branch's reason,
// so it stays at the same human-prose scale as decision evidence.
const MaxAgentReportNoteBytes = 4096

// AgentReport is one authenticated performer's report for exactly one agent
// command. The daemon has already proven the caller is the agent this command
// was dispatched to; the executor's job is to bind the report to the exact
// outstanding command — run, node, and attempt — and refuse anything else.
type AgentReport struct {
	CommandID string
	NodeID    string
	Attempt   int
	Outcome   engine.ProgramOutcome
	Note      string
	// Verdict is an agent decider's chosen outcome; required on a decider's
	// successful report and refused on any other.
	Verdict string
	// Performer is the stable agent id that reported, recorded as evidence.
	Performer string
}

// RecordAgentReport durably applies one performer's report to its outstanding
// agent command. It goes through the same observation transition a program
// does, so retry budgets, parking, and fail-fast behave identically; the only
// difference is the evidence row, which names the performer.
//
// A report for a command that is no longer outstanding — a duplicate, an older
// attempt, or a node a person already settled — is refused as
// engine.ErrStaleObservation without touching state.
func RecordAgentReport(run *Run, actor string, report AgentReport) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if err := validateAgentReport(report); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	observation, err := agentObservation(run, report)
	if err != nil {
		return err
	}
	command, _ := findCommand(run.checkpoint, report.CommandID, report.NodeID)
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionProgramObserved, Observation: &observation,
	})
	if err != nil {
		return err
	}
	payload := struct {
		Observation engine.ProgramObservation `json:"observation"`
		Attempt     int                       `json:"attempt"`
		Performer   string                    `json:"performer,omitempty"`
		Note        string                    `json:"note,omitempty"`
	}{Observation: observation, Attempt: command.Attempt, Performer: report.Performer, Note: report.Note}
	// A failed agent check or review sends its compound's work back, exactly
	// as a failed program gate does.
	causal := append([]db.ProcessRunEvent{event("agent_reported", &command, actor, payload)},
		causedStageReset(run.definition, command.NodeID, run.checkpoint, next)...)
	if observation.Verdict != "" {
		// The note is the decider's reasoning, and the next round's performer
		// is handed it exactly as a person's evidence.
		decided, err := verdictEvidence(run.definition, run.checkpoint, next, actor,
			decisionEvidencePayload{Verdict: report.Verdict, Evidence: report.Note, Attempt: command.Attempt}, command.NodeID)
		if err != nil {
			return err
		}
		causal = append(causal, decided...)
	}
	events := commitEvidence(run.definition, run.checkpoint, next, causal, nil, false)
	return persistEvents(run, next, events)
}

// CheckAgentReport reports whether RecordAgentReport would accept a report,
// without recording anything. The daemon stores a report durably before it
// applies it, and a stored report the engine then refuses could never be
// applied; an agent decider's verdict is the one part of a report the engine
// can refuse on its own terms (an outcome its loop has no rounds left for).
// While the command is outstanding nothing else can change that answer.
func CheckAgentReport(run *Run, report AgentReport) error {
	if err := validateAgentReport(report); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	observation, err := agentObservation(run, report)
	if err != nil {
		return err
	}
	_, err = engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionProgramObserved, Observation: &observation,
	})
	return err
}

// agentObservation binds a report to its outstanding command and builds the
// observation the engine applies.
func agentObservation(run *Run, report AgentReport) (engine.ProgramObservation, error) {
	command, ok := findCommand(run.checkpoint, report.CommandID, report.NodeID)
	if !ok || command.Kind != engine.CommandAgent || command.Attempt != report.Attempt {
		return engine.ProgramObservation{}, fmt.Errorf("%w: report does not match an outstanding agent command", engine.ErrStaleObservation)
	}
	switch decider := len(command.Verdicts) > 0; {
	case decider && report.Outcome == engine.ProgramSucceeded && report.Verdict == "":
		return engine.ProgramObservation{}, fmt.Errorf("%w: a decider reports its verdict, one of %s",
			ErrInvalidAgentReport, strings.Join(command.Verdicts, ", "))
	case (!decider || report.Outcome != engine.ProgramSucceeded) && report.Verdict != "":
		return engine.ProgramObservation{}, fmt.Errorf("%w: only a decider's successful report carries a verdict", ErrInvalidAgentReport)
	}
	observation := engine.ProgramObservation{
		CommandID: command.ID, NodeID: command.NodeID, Outcome: report.Outcome, Verdict: report.Verdict,
	}
	if report.Outcome == engine.ProgramFailed {
		observation.ExitCode = 1
		observation.Error = report.Note
		if observation.Error == "" {
			observation.Error = "agent performer reported failure"
		}
	}
	return observation, nil
}

func validateAgentReport(report AgentReport) error {
	if report.CommandID == "" || report.NodeID == "" || report.Attempt < 1 {
		return fmt.Errorf("%w: a report must name its command, node, and positive attempt", ErrInvalidAgentReport)
	}
	if report.Outcome != engine.ProgramSucceeded && report.Outcome != engine.ProgramFailed {
		return fmt.Errorf("%w: outcome must be %s or %s", ErrInvalidAgentReport, engine.ProgramSucceeded, engine.ProgramFailed)
	}
	return ValidateAgentReportNote(report.Note)
}

// ValidateAgentReportNote checks a performer's report note: bounded UTF-8 with
// no control characters other than line breaks and tabs.
func ValidateAgentReportNote(note string) error {
	if len(note) > MaxAgentReportNoteBytes || !utf8.ValidString(note) {
		return fmt.Errorf("%w: note must be at most %d bytes of UTF-8", ErrInvalidAgentReport, MaxAgentReportNoteBytes)
	}
	for _, value := range note {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return fmt.Errorf("%w: note cannot contain control characters other than line breaks and tabs", ErrInvalidAgentReport)
		}
	}
	return nil
}
