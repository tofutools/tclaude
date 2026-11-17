package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConditionEvaluates(t *testing.T) {
	values := map[string]string{"params.merge": "yes", "steps.ci.result": "succeeded", "steps.ci.exitCode": "0", "outputs.score": "7.5"}
	read := func(operand Operand) (string, bool) {
		value, ok := values[operand.String()]
		return value, ok
	}
	for text, want := range map[string]bool{
		`params.merge == "yes" and steps.ci.result == succeeded`:  true,
		`params.merge == no or steps.ci.exitCode != 0`:            false,
		`not (params.merge == no) and outputs.score >= 7`:         true,
		`outputs.score > 8 or outputs.score < 2`:                  false,
		`steps.ci.exitCode == 0.0`:                                true,
		`outputs.missing == ""`:                                   false,
		`not outputs.missing == x`:                                true,
		`params.merge == 'yes' AND NOT steps.ci.result == failed`: true,
	} {
		cond, err := ParseCondition(text)
		require.NoError(t, err, text)
		require.Equal(t, want, cond.Eval(read), text)
	}
}

func TestConditionRefusesWhatItCannotRead(t *testing.T) {
	for text, says := range map[string]string{
		``:                        "empty",
		`params.merge`:            "needs a comparison",
		`params.merge ==`:         "needs a value",
		`steps.ci == ok`:          "not something a rule can read",
		`steps.ci.output == ok`:   "not something a rule can read",
		`env.HOME == x`:           "not something a rule can read",
		`outputs.score > high`:    "compares numbers",
		`(params.merge == yes`:    "not closed",
		`params.merge == "yes`:    "not closed",
		`params.merge == yes yes`: "unexpected",
		`params.merge =< 3`:       "unknown operator",
		`params.merge == yes and`: "ends too early",
	} {
		_, err := ParseCondition(text)
		require.Error(t, err, text)
		require.Contains(t, err.Error(), says, text)
	}
}

func ruleTemplate(rule *Performer, mutate func(nodes map[string]Node)) *Template {
	task := func(next string) Node {
		return Node{Type: NodeTypeTask, Performer: &Performer{Kind: PerformerProgram, Run: "true"}, Next: Next{DefaultOutcome: next}}
	}
	nodes := map[string]Node{
		"start": {Type: NodeTypeStart, Next: Next{DefaultOutcome: "ci"}},
		"ci":    task("gate"),
		"gate":  {Type: NodeTypeDecision, Performer: rule, Next: Next{"merge": "merge", "stop": "end"}},
		"merge": task("end"),
		"end":   {Type: NodeTypeEnd},
	}
	if mutate != nil {
		mutate(nodes)
	}
	return &Template{APIVersion: APIVersion, Kind: Kind, ID: "rule", Start: "start", Nodes: nodes,
		Params: map[string]Param{"merge": {Type: "string"}}}
}

func TestRuleDecisionValidates(t *testing.T) {
	rule := &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes and steps.ci.result == succeeded and steps.ci.exitCode == 0`, Then: "merge"}}, Else: "stop"}
	require.Empty(t, loopErrorCodes(ruleTemplate(rule, nil)))
}

func TestRuleDecisionRefusesBadReferences(t *testing.T) {
	cases := map[string]struct {
		rule   *Performer
		mutate func(map[string]Node)
		code   string
	}{
		"unknown param":    {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.nope == yes`, Then: "merge"}}, Else: "stop"}, code: "unknown_rule_param"},
		"unknown output":   {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `outputs.nope == yes`, Then: "merge"}}, Else: "stop"}, code: "unknown_rule_output"},
		"unknown step":     {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `steps.nope.result == failed`, Then: "merge"}}, Else: "stop"}, code: "unknown_rule_step"},
		"later step":       {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `steps.merge.result == failed`, Then: "merge"}}, Else: "stop"}, code: "rule_step_not_upstream"},
		"bad condition":    {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge`, Then: "merge"}}, Else: "stop"}, code: "invalid_rule_condition"},
		"unknown outcome":  {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes`, Then: "ship"}}, Else: "stop"}, code: "unknown_rule_outcome"},
		"no else":          {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes`, Then: "merge"}}}, code: "missing_rule_else"},
		"no cases":         {rule: &Performer{Kind: PerformerRule, Else: "stop"}, code: "missing_rule_cases"},
		"prompt on a rule": {rule: &Performer{Kind: PerformerRule, Prompt: "x", Cases: []RuleCase{{When: `params.merge == yes`, Then: "merge"}}, Else: "stop"}, code: "kind_scoped_field"},
		"agent exit code": {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `steps.ci.exitCode == 0`, Then: "merge"}}, Else: "stop"},
			mutate: func(nodes map[string]Node) {
				ci := nodes["ci"]
				ci.Performer = &Performer{Kind: PerformerAgent, Prompt: "run CI"}
				nodes["ci"] = ci
			}, code: "rule_exit_code_not_command"},
		"a rule on a task": {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes`, Then: "merge"}}, Else: "stop"},
			mutate: func(nodes map[string]Node) {
				ci := nodes["ci"]
				ci.Performer = &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes`, Then: "x"}}, Else: "x"}
				nodes["ci"] = ci
			}, code: "rule_on_non_decision"},
		"mayFail on a decision": {rule: &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `params.merge == yes`, Then: "merge"}}, Else: "stop"},
			mutate: func(nodes map[string]Node) {
				gate := nodes["gate"]
				gate.MayFail = true
				nodes["gate"] = gate
			}, code: "may_fail_on_non_task"},
	}
	for name, tc := range cases {
		codes := loopErrorCodes(ruleTemplate(tc.rule, tc.mutate))
		require.Contains(t, codes, tc.code, name)
	}
}

func TestRuleLoopNeedsExhaustion(t *testing.T) {
	rule := &Performer{Kind: PerformerRule, Cases: []RuleCase{{When: `steps.ci.result == failed`, Then: "again"}}, Else: "merge"}
	tmpl := ruleTemplate(rule, func(nodes map[string]Node) {
		gate := nodes["gate"]
		gate.Next = Next{"again": "ci", "merge": "merge", "stop": "end"}
		gate.Loops = map[string]LoopBound{"again": {MaxIterations: 3}}
		nodes["gate"] = gate
		ci := nodes["ci"]
		ci.MayFail = true
		nodes["ci"] = ci
	})
	require.Contains(t, loopErrorCodes(tmpl), "rule_loop_without_exhaustion")
	gate := tmpl.Nodes["gate"]
	gate.Loops = map[string]LoopBound{"again": {MaxIterations: 3, OnExhausted: "stop"}}
	tmpl.Nodes["gate"] = gate
	require.Empty(t, loopErrorCodes(tmpl))
}

func TestRuleDecisionRoundTripsStrictYAML(t *testing.T) {
	source := `apiVersion: ` + APIVersion + `
kind: ` + Kind + `
id: rule
params:
  merge: {type: string}
start: start
nodes:
  start: {type: start, next: {next: ci}}
  ci: {type: task, mayFail: true, performer: {kind: program, run: "true"}, next: {next: gate}}
  gate:
    type: decision
    performer:
      kind: rule
      cases:
        - {when: 'params.merge == yes and steps.ci.result == succeeded', then: merge}
      else: stop
    next: {merge: merge, stop: end}
  merge: {type: task, performer: {kind: program, run: "true"}, next: {next: end}}
  end: {type: end}
`
	require.Empty(t, mustParse(t, source))
}
