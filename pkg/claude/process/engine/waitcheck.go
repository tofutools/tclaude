package engine

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// A wait for a criterion (model.WaitCheck) is one wait node that alternates
// between two commands until its criterion holds or it reaches a limit:
//
//	timer (CommandWait) → check (a program or an agent) → timer → check → …
//
// The first timer is due at once; the daemon's record of it is when the wait
// started, which the checkpoint keeps (WaitCheckState.StartedAt) so a within
// limit bounds every later timer. A rule check (when) has no check command:
// the engine evaluates the condition each time a timer comes due, and also as
// soon as something the run does makes it hold (TransitionWaitMet).
//
// The engine stays clock-free: the daemon says when each timer started and was
// due (ProgramObservation.Wait), and the engine compares those to the limit.

// TransitionWaitMet ends a rule-checked wait whose condition holds while it
// waits for its next check. Engine-owned, like an advance.
const TransitionWaitMet TransitionKind = "wait_met"

// Wait check phases: what the node's next (or outstanding) command is.
const (
	waitPhaseTimer = ""
	waitPhaseCheck = "check"
)

// WaitCheckState is where a wait for a criterion is: how many checks it has
// made, how many failed in a row, when it started, and the last answer.
type WaitCheckState struct {
	Checks int `json:"checks"`
	Failed int `json:"failed,omitempty"`
	// Phase is "" while a timer is (or is to be) out, "check" while a check is.
	Phase     string     `json:"phase,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// Last is the last check's answer: yes, no, or failed: <why>.
	Last string `json:"last,omitempty"`
}

// WaitObservation is the daemon's account of a wait timer that ended: how,
// when it started, and when it was due.
type WaitObservation struct {
	How       string    `json:"how"`
	StartedAt time.Time `json:"startedAt"`
	DueAt     time.Time `json:"dueAt"`
}

// How a wait timer ended, as WaitObservation.How says.
const (
	WaitHowElapsed = "elapsed"
	WaitHowSkipped = "skipped"
)

// preparedWaitCheck is a wait's criterion bound to its run.
type preparedWaitCheck struct {
	spec model.WaitCheck
	// rule is a when check, as a one-case rule whose outcome is "met".
	rule    *preparedRule
	program *ProgramCommand
	agent   *AgentCommand
	within  time.Duration
}

// prepareWaitCheck binds a wait node's criterion.
func prepareWaitCheck(definition *Definition, id string, check model.WaitCheck, scope *model.Template,
	params map[string]string, prefix string) (*preparedWaitCheck, error) {
	prepared := &preparedWaitCheck{spec: check}
	if check.Within != "" {
		within, err := model.ParseWaitDuration(check.Within)
		if err != nil {
			return nil, fmt.Errorf("%w: wait %q within: %v", ErrInvalidDefinition, id, err)
		}
		prepared.within = within
	}
	switch {
	case strings.TrimSpace(check.When) != "":
		rule, err := prepareRule(id, model.Performer{Kind: model.PerformerRule,
			Cases: []model.RuleCase{{When: check.When, Then: model.WaitOutcomeMet}}}, scope, params, prefix)
		if err != nil {
			return nil, err
		}
		prepared.rule = rule
	case check.Performer != nil && check.Performer.Kind == model.PerformerAgent:
		agent, err := bindAgent(id, *check.Performer, nil, params, definition.fallback)
		if err != nil {
			return nil, err
		}
		constrainAgent(agent, *check.Performer, prefix)
		prepared.agent = agent
	case check.Performer != nil:
		program, err := bindProgram(id, *check.Performer, params)
		if err != nil {
			return nil, err
		}
		if check.Timeout != "" {
			// A check's timeout is the check's, in the wait's own grammar.
			d, err := model.ParseWaitDuration(check.Timeout)
			if err != nil {
				return nil, fmt.Errorf("%w: wait %q timeout: %v", ErrInvalidDefinition, id, err)
			}
			program.Timeout = d.String()
		}
		prepared.program = &program
	default:
		return nil, fmt.Errorf("%w: wait %q has no criterion", ErrInvalidDefinition, id)
	}
	return prepared, nil
}

// IsWaitCheck reports whether nodeID is a wait for a criterion.
func (d *Definition) IsWaitCheck(nodeID string) bool {
	index, ok := d.index[nodeID]
	return ok && d.nodes[index].check != nil
}

// waitCheckAttempts is a check wait's attempt budget: every timer and every
// check is an attempt of the node, and its own limits, not attempts, bound it.
const waitCheckAttempts = math.MaxInt32

// waitCheckVerdicts are what an agent check answers.
var waitCheckVerdicts = []string{model.WaitCheckNo, model.WaitCheckYes}

// waitCheckKind is the kind of the command a check wait plans next.
func (node definitionNode) waitCheckKind(checkpoint Checkpoint) CommandKind {
	if checkpoint.WaitChecks[node.id].Phase != waitPhaseCheck {
		return CommandWait
	}
	if node.check.agent != nil {
		return CommandAgent
	}
	return CommandProgram
}

// fillWaitCheckCommand fills in the command a check wait plans next: its next
// timer, or its check.
func fillWaitCheckCommand(checkpoint Checkpoint, node definitionNode, command *Command) {
	state := checkpoint.WaitChecks[node.id]
	if state.Phase == waitPhaseCheck {
		if node.check.agent != nil {
			agent := *node.check.agent
			agent.DifferentFrom = slices.Clone(node.check.agent.DifferentFrom)
			agent.Captures = slices.Clone(node.check.agent.Captures)
			command.Agent = &agent
			command.Verdicts = slices.Clone(waitCheckVerdicts)
		} else {
			command.Program = cloneProgramCommand(*node.check.program)
		}
		return
	}
	wait := &WaitCommand{}
	if state.StartedAt == nil {
		// The first check is made straight away.
		wait.Now = true
	} else {
		wait.Duration = node.check.spec.Interval(state.Checks).String()
		if node.check.within > 0 {
			wait.Until = state.StartedAt.Add(node.check.within).UTC().Format(time.RFC3339)
		}
	}
	command.Wait = wait
}

// applyWaitCheckObservation settles one command of a check wait: a timer
// that came due, or a check's answer.
func applyWaitCheckObservation(next *Checkpoint, definition *Definition, index int, command Command, observation *ProgramObservation) error {
	node := definition.nodes[index]
	check := node.check
	state := next.WaitChecks[node.id]
	removeCommand(next, node.id)
	if command.Kind == CommandWait {
		if observation.Outcome == ProgramFailed {
			// Withdrawn, or the daemon could not time it.
			return failWaitCheck(next, definition, index, state, observation.Error)
		}
		if observation.Wait == nil {
			return fmt.Errorf("%w: a check wait's timer must say when it started and was due", ErrInvalidTransition)
		}
		if observation.Wait.How == WaitHowSkipped {
			state.Last = "skipped"
			return metWaitCheck(next, definition, index, state)
		}
		if state.StartedAt == nil {
			started := observation.Wait.StartedAt.UTC()
			state.StartedAt = &started
		} else if check.within > 0 && !observation.Wait.DueAt.Before(state.StartedAt.Add(check.within)) {
			return limitWaitCheck(next, definition, index, state, fmt.Sprintf("not met within %s (%d checks)", check.spec.Within, state.Checks))
		}
		if check.rule != nil {
			state.Checks++
			if waitRuleHolds(*next, node) {
				state.Last = model.WaitCheckYes
				return metWaitCheck(next, definition, index, state)
			}
			state.Last = model.WaitCheckNo
			return afterWaitCheck(next, definition, index, state)
		}
		state.Phase = waitPhaseCheck
		return readyWaitCheck(next, node, state)
	}
	// A check's answer.
	state.Checks++
	state.Phase = waitPhaseTimer
	yes, no := false, false
	switch {
	case command.Kind == CommandAgent && observation.Outcome == ProgramSucceeded:
		yes, no = observation.Verdict == model.WaitCheckYes, observation.Verdict == model.WaitCheckNo
	case command.Kind == CommandProgram && observation.Outcome == ProgramSucceeded:
		yes = true
	case command.Kind == CommandProgram && observation.ExitCode == 1:
		no = true
	}
	switch {
	case yes:
		state.Failed, state.Last = 0, model.WaitCheckYes
		return metWaitCheck(next, definition, index, state)
	case no:
		state.Failed, state.Last = 0, model.WaitCheckNo
	default:
		state.Failed++
		reason := strings.TrimSpace(observation.Error)
		if reason == "" {
			reason = "the check failed"
		}
		state.Last = "failed: " + truncateReason(reason)
		if state.Failed >= check.spec.MaxFailed() {
			return failWaitCheck(next, definition, index, state,
				fmt.Sprintf("wait '%s': %d checks in a row failed; the last: %s", node.id, state.Failed, reason))
		}
	}
	return afterWaitCheck(next, definition, index, state)
}

// afterWaitCheck goes on after a check that did not hold: to the next timer,
// or to the limit once the checks are used up.
func afterWaitCheck(next *Checkpoint, definition *Definition, index int, state WaitCheckState) error {
	node := definition.nodes[index]
	if max := node.check.spec.MaxChecks; max > 0 && state.Checks >= max {
		return limitWaitCheck(next, definition, index, state, fmt.Sprintf("not met after %d checks", state.Checks))
	}
	state.Phase = waitPhaseTimer
	return readyWaitCheck(next, node, state)
}

// readyWaitCheck readies the node for its next command.
func readyWaitCheck(next *Checkpoint, node definitionNode, state WaitCheckState) error {
	putWaitCheckState(next, node.id, state)
	next.Nodes[node.id] = NodeReady
	return nil
}

// metWaitCheck ends the wait along its met route.
func metWaitCheck(next *Checkpoint, definition *Definition, index int, state WaitCheckState) error {
	node := definition.nodes[index]
	putWaitCheckState(next, node.id, state)
	outcome := soleOutcome(definition, node)
	if node.check.spec.TimesOut() {
		outcome = model.WaitOutcomeMet
	}
	recordStep(next, node, model.StepSucceeded, &ProgramObservation{})
	if err := completeAndSettle(next, definition, index, arrivesAt(outcome)); err != nil {
		return err
	}
	abandonAfterDoom(next, definition)
	return nil
}

// limitWaitCheck ends a wait that reached its limit: along its timedOut
// route, or by failing the step.
func limitWaitCheck(next *Checkpoint, definition *Definition, index int, state WaitCheckState, why string) error {
	node := definition.nodes[index]
	if !node.check.spec.TimesOut() {
		return failWaitCheck(next, definition, index, state, fmt.Sprintf("wait '%s': %s", node.id, why))
	}
	putWaitCheckState(next, node.id, state)
	recordStep(next, node, model.StepSucceeded, &ProgramObservation{})
	if err := completeAndSettle(next, definition, index, arrivesAt(model.WaitOutcomeTimedOut)); err != nil {
		return err
	}
	abandonAfterDoom(next, definition)
	return nil
}

// failWaitCheck fails the step, and with it the run, naming the step.
func failWaitCheck(next *Checkpoint, definition *Definition, index int, state WaitCheckState, reason string) error {
	node := definition.nodes[index]
	putWaitCheckState(next, node.id, state)
	recordStep(next, node, model.StepFailed, &ProgramObservation{ExitCode: 1, Error: reason})
	if !doomed(*next) && next.Failure == nil {
		next.Failure = &StepFailure{NodeID: node.id, Attempt: next.Attempts[node.id], Reason: truncateReason(reason)}
	}
	next.Nodes[node.id] = NodeFailed
	abandonAfterDoom(next, definition)
	return nil
}

func putWaitCheckState(next *Checkpoint, nodeID string, state WaitCheckState) {
	if next.WaitChecks == nil {
		next.WaitChecks = map[string]WaitCheckState{}
	}
	next.WaitChecks[nodeID] = state
}

// waitMetTransition is the engine-owned end of a rule-checked wait whose
// condition holds while it waits for its next check.
func waitMetTransition(checkpoint Checkpoint, definition *Definition) (Transition, bool) {
	for _, node := range definition.nodes {
		if node.check == nil || node.check.rule == nil || checkpoint.Nodes[node.id] != NodeRunning {
			continue
		}
		// Only once it has started: the first check is the timer's to make.
		if checkpoint.WaitChecks[node.id].StartedAt == nil {
			continue
		}
		if waitRuleHolds(checkpoint, node) {
			return Transition{Kind: TransitionWaitMet, NodeID: node.id}, true
		}
	}
	return Transition{}, false
}

// applyWaitMet ends a rule-checked wait whose condition holds: its timer is
// withdrawn (the daemon's later finish is stale) and it goes on as met.
func applyWaitMet(next *Checkpoint, definition *Definition, nodeID string) error {
	index, ok := definition.index[nodeID]
	if !ok {
		return fmt.Errorf("%w: wait_met names unknown node %q", ErrInvalidTransition, nodeID)
	}
	node := definition.nodes[index]
	if node.check == nil || node.check.rule == nil || next.Nodes[nodeID] != NodeRunning {
		return fmt.Errorf("%w: %q is not a rule-checked wait that is waiting", ErrInvalidTransition, nodeID)
	}
	if !waitRuleHolds(*next, node) {
		return fmt.Errorf("%w: %q: its condition does not hold", ErrInvalidTransition, nodeID)
	}
	state := next.WaitChecks[nodeID]
	state.Checks++
	state.Last = model.WaitCheckYes
	removeCommand(next, nodeID)
	return metWaitCheck(next, definition, index, state)
}

// waitRuleHolds reports whether a rule-checked wait's condition holds now.
func waitRuleHolds(checkpoint Checkpoint, node definitionNode) bool {
	ruled := node
	ruled.rule = node.check.rule
	return evaluateRule(checkpoint, ruled).Outcome == model.WaitOutcomeMet
}
