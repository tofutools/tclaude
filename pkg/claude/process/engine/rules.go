package engine

import (
	"fmt"
	"maps"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// A rule decision (model.PerformerRule) is engine-owned: once it is ready the
// engine evaluates its cases against the run's params and step records, in
// the same pass as a start or a fork, and takes the outcome it picks. Nobody
// is asked. Each evaluation is kept in Checkpoint.Rules, which is what the
// run's events and views show.

// StepRecord is what a rule can read of a step that ran: its result, a
// command's exit code, and the outputs it reported. The checkpoint keeps one
// only for a step some rule reads.
type StepRecord struct {
	Result   string            `json:"result"`
	ExitCode *int              `json:"exitCode,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
}

// RuleChoice is a rule decision's last evaluation: the case that held (-1 for
// else), the outcome it took, and every value the rule read, by operand as
// written. A value nobody had is absent from Read. Count is how many times
// the rule has been evaluated in this run, so a loop's next round shows as a
// new choice.
type RuleChoice struct {
	Case    int               `json:"case"`
	Outcome string            `json:"outcome"`
	Read    map[string]string `json:"read,omitempty"`
	Count   int               `json:"count"`
}

// preparedRule is a rule decision bound to its run: params are bound, and
// step and output names resolve to the prepared node ids of its scope.
type preparedRule struct {
	cases       []preparedCase
	elseOutcome string
	params      map[string]string
	// steps maps a step id as written to its prepared node id; outputs maps an
	// output name to the prepared id of the step that reports it.
	steps   map[string]string
	outputs map[string]string
}

type preparedCase struct {
	when *model.Condition
	text string
	then string
}

// prepareRule binds a rule decision of the scope whose node ids carry prefix.
func prepareRule(nodeID string, performer model.Performer, scope *model.Template, params map[string]string, prefix string) (*preparedRule, error) {
	rule := &preparedRule{elseOutcome: performer.Else, params: map[string]string{}, steps: map[string]string{}, outputs: map[string]string{}}
	captures := map[string]string{}
	for id, node := range scope.Nodes {
		for _, name := range node.Captures {
			captures[name] = prefix + id
		}
	}
	for i, ruleCase := range performer.Cases {
		cond, err := model.ParseCondition(ruleCase.When)
		if err != nil {
			return nil, fmt.Errorf("%w: rule %q case %d: %v", ErrInvalidDefinition, nodeID, i, err)
		}
		for _, operand := range cond.Operands() {
			switch operand.Kind {
			case model.OperandParam:
				if value, ok := params[operand.Name]; ok {
					rule.params[operand.Name] = value
				} else if value, ok := model.ParamDefault(scope.Params[operand.Name]); ok {
					// A run started without the param reads its default.
					rule.params[operand.Name] = value
				}
			case model.OperandOutput:
				owner, ok := captures[operand.Name]
				if !ok {
					return nil, fmt.Errorf("%w: rule %q reads %s, which no step reports", ErrInvalidDefinition, nodeID, operand)
				}
				rule.outputs[operand.Name] = owner
			case model.OperandResult, model.OperandExitCode:
				rule.steps[operand.Name] = prefix + operand.Name
			}
		}
		rule.cases = append(rule.cases, preparedCase{when: cond, text: ruleCase.When, then: ruleCase.Then})
	}
	return rule, nil
}

// recordedSteps are the prepared ids of the steps whose records a rule reads.
func (r *preparedRule) recordedSteps() []string {
	var ids []string
	for _, id := range r.steps {
		ids = append(ids, id)
	}
	for _, id := range r.outputs {
		ids = append(ids, id)
	}
	return ids
}

// evaluateRule picks a ready rule decision's outcome from the checkpoint.
func evaluateRule(checkpoint Checkpoint, node definitionNode) RuleChoice {
	rule := node.rule
	read := map[string]string{}
	lookup := func(operand model.Operand) (string, bool) {
		value, ok := readOperand(checkpoint, rule, operand)
		if ok {
			read[operand.String()] = value
		}
		return value, ok
	}
	choice := RuleChoice{Case: -1, Outcome: rule.elseOutcome}
	for i, ruleCase := range rule.cases {
		// Every operand of every case up to the one that holds is read, so the
		// record shows why the earlier cases did not.
		for _, operand := range ruleCase.when.Operands() {
			lookup(operand)
		}
		if ruleCase.when.Eval(lookup) {
			choice = RuleChoice{Case: i, Outcome: ruleCase.then}
			break
		}
	}
	if len(read) > 0 {
		choice.Read = read
	}
	choice.Count = checkpoint.Rules[node.id].Count + 1
	return choice
}

func readOperand(checkpoint Checkpoint, rule *preparedRule, operand model.Operand) (string, bool) {
	switch operand.Kind {
	case model.OperandParam:
		value, ok := rule.params[operand.Name]
		return value, ok
	case model.OperandOutput:
		// Only a step done this round has outputs: what it reported in an
		// earlier round, or on a branch not taken, no longer stands.
		owner := rule.outputs[operand.Name]
		if checkpoint.Nodes[owner] != NodeDone {
			return "", false
		}
		value, ok := checkpoint.Steps[owner].Outputs[operand.Name]
		return value, ok
	case model.OperandResult:
		return stepResult(checkpoint, rule.steps[operand.Name])
	case model.OperandExitCode:
		id := rule.steps[operand.Name]
		if result, ok := stepResult(checkpoint, id); !ok || result == model.StepSkipped {
			return "", false
		}
		record, ok := checkpoint.Steps[id]
		if !ok || record.ExitCode == nil {
			return "", false
		}
		return strconv.Itoa(*record.ExitCode), true
	}
	return "", false
}

// stepResult is a step's result as a rule reads it. The node's status comes
// first: a record outlives a round, but a step skipped this round was skipped.
func stepResult(checkpoint Checkpoint, id string) (string, bool) {
	switch checkpoint.Nodes[id] {
	case NodeSkipped:
		return model.StepSkipped, true
	case NodeFailed:
		return model.StepFailed, true
	case NodeDone:
		if record, ok := checkpoint.Steps[id]; ok && record.Result != "" {
			return record.Result, true
		}
		return model.StepSucceeded, true
	}
	return "", false
}

// recordStep keeps what a rule may read of a step's attempt.
func recordStep(checkpoint *Checkpoint, node definitionNode, result string, observation *ProgramObservation) {
	if !node.recorded {
		return
	}
	record := StepRecord{Result: result}
	var captures []string
	switch {
	case node.agent != nil:
		captures = node.agent.Captures
	case node.human != nil:
		captures = node.human.Captures
	default:
		exitCode := observation.ExitCode
		record.ExitCode = &exitCode
	}
	if len(observation.Outputs) > 0 && len(captures) > 0 {
		record.Outputs = map[string]string{}
		for _, name := range captures {
			if value, ok := observation.Outputs[name]; ok {
				record.Outputs[name] = value
			}
		}
	}
	if checkpoint.Steps == nil {
		checkpoint.Steps = map[string]StepRecord{}
	}
	checkpoint.Steps[node.id] = record
}

func cloneSteps(steps map[string]StepRecord) map[string]StepRecord {
	if len(steps) == 0 {
		return steps
	}
	out := make(map[string]StepRecord, len(steps))
	for id, record := range steps {
		record.Outputs = maps.Clone(record.Outputs)
		if record.ExitCode != nil {
			exitCode := *record.ExitCode
			record.ExitCode = &exitCode
		}
		out[id] = record
	}
	return out
}

func cloneRules(rules map[string]RuleChoice) map[string]RuleChoice {
	if len(rules) == 0 {
		return rules
	}
	out := make(map[string]RuleChoice, len(rules))
	for id, choice := range rules {
		choice.Read = maps.Clone(choice.Read)
		out[id] = choice
	}
	return out
}

// RuleCase returns the condition of a rule decision's case as written, for
// the run's events and views.
func (d *Definition) RuleCase(nodeID string, index int) (string, bool) {
	i, ok := d.index[nodeID]
	if !ok || d.nodes[i].rule == nil || index < 0 || index >= len(d.nodes[i].rule.cases) {
		return "", false
	}
	return d.nodes[i].rule.cases[index].text, true
}
