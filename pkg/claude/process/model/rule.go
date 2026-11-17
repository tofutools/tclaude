package model

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// A rule decides a decision with no agent or person involved: the engine
// takes the outcome of the first case whose condition holds, or Else.
//
// A condition is a small, closed language, parsed at authoring time:
// comparisons `<operand> <op> <value>` joined by and, or and not, with
// parentheses. The operands are
//
//	params.<name>          a param of the run, bound when it starts
//	steps.<id>.result      succeeded, failed or skipped
//	steps.<id>.exitCode    a command step's exit code
//	outputs.<name>         an output a step reported
//
// The ops are == != < <= > >=; the ordering ops compare numbers. A value is a
// quoted string, a number or a bare word (yes, succeeded).

// RuleCase is one case of a rule: when its condition holds, the decision
// takes outcome Then.
type RuleCase struct {
	When string `json:"when" yaml:"when"`
	Then string `json:"then" yaml:"then"`
}

// Step results a rule reads from steps.<id>.result.
const (
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// Operand kinds of a condition.
const (
	OperandParam    = "params"
	OperandResult   = "result"
	OperandExitCode = "exitCode"
	OperandOutput   = "outputs"
)

// Operand is one value a condition reads.
type Operand struct {
	// Kind is OperandParam, OperandResult, OperandExitCode or OperandOutput.
	Kind string
	// Name is the param, the step id, or the output name.
	Name string
}

// String is the operand as written.
func (o Operand) String() string {
	switch o.Kind {
	case OperandResult, OperandExitCode:
		return "steps." + o.Name + "." + o.Kind
	default:
		return o.Kind + "." + o.Name
	}
}

// Condition is a parsed rule condition.
type Condition struct {
	op       string // "and", "or", "not", or a comparison op
	children []*Condition
	operand  Operand
	value    string
}

var comparisonOps = []string{"==", "!=", "<=", ">=", "<", ">"}

// ParseCondition parses a rule condition.
func ParseCondition(text string) (*Condition, error) {
	tokens, err := lexCondition(text)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("the condition is empty")
	}
	p := &conditionParser{tokens: tokens}
	cond, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.at < len(p.tokens) {
		return nil, fmt.Errorf("unexpected %q", p.tokens[p.at].text)
	}
	return cond, nil
}

// Operands are the values the condition reads, in the order written.
func (c *Condition) Operands() []Operand {
	var out []Operand
	c.walk(func(leaf *Condition) {
		if !slices.Contains(out, leaf.operand) {
			out = append(out, leaf.operand)
		}
	})
	return out
}

func (c *Condition) walk(visit func(*Condition)) {
	if len(c.children) == 0 && c.op != "not" {
		visit(c)
		return
	}
	for _, child := range c.children {
		child.walk(visit)
	}
}

// Eval evaluates the condition. read returns an operand's value, and whether
// it has one: a step that never ran has no exit code, and an output nobody
// reported has no value. A comparison with an operand that has no value is
// false, whatever its op.
func (c *Condition) Eval(read func(Operand) (string, bool)) bool {
	switch c.op {
	case "and":
		return c.children[0].Eval(read) && c.children[1].Eval(read)
	case "or":
		return c.children[0].Eval(read) || c.children[1].Eval(read)
	case "not":
		return !c.children[0].Eval(read)
	}
	got, ok := read(c.operand)
	if !ok {
		return false
	}
	return compare(got, c.op, c.value)
}

func compare(got, op, want string) bool {
	gotNum, gotErr := strconv.ParseFloat(strings.TrimSpace(got), 64)
	wantNum, wantErr := strconv.ParseFloat(want, 64)
	numeric := gotErr == nil && wantErr == nil
	switch op {
	case "==":
		if numeric {
			return gotNum == wantNum
		}
		return strings.TrimSpace(got) == want
	case "!=":
		if numeric {
			return gotNum != wantNum
		}
		return strings.TrimSpace(got) != want
	}
	if !numeric {
		return false
	}
	switch op {
	case "<":
		return gotNum < wantNum
	case "<=":
		return gotNum <= wantNum
	case ">":
		return gotNum > wantNum
	case ">=":
		return gotNum >= wantNum
	}
	return false
}

type conditionToken struct {
	text   string
	quoted bool
}

func lexCondition(text string) ([]conditionToken, error) {
	var tokens []conditionToken
	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(' || r == ')':
			tokens = append(tokens, conditionToken{text: string(r)})
			i++
		case r == '"' || r == '\'':
			end := i + 1
			for end < len(runes) && runes[end] != r {
				end++
			}
			if end == len(runes) {
				return nil, fmt.Errorf("a quoted value is not closed")
			}
			tokens = append(tokens, conditionToken{text: string(runes[i+1 : end]), quoted: true})
			i = end + 1
		case strings.ContainsRune("=!<>", r):
			op := string(r)
			if i+1 < len(runes) && runes[i+1] == '=' {
				op += "="
			}
			if !slices.Contains(comparisonOps, op) {
				return nil, fmt.Errorf("unknown operator %q", op)
			}
			tokens = append(tokens, conditionToken{text: op})
			i += len([]rune(op))
		default:
			end := i
			for end < len(runes) && !unicode.IsSpace(runes[end]) && !strings.ContainsRune("()=!<>\"'", runes[end]) {
				end++
			}
			tokens = append(tokens, conditionToken{text: string(runes[i:end])})
			i = end
		}
	}
	return tokens, nil
}

type conditionParser struct {
	tokens []conditionToken
	at     int
}

func (p *conditionParser) peek() (conditionToken, bool) {
	if p.at >= len(p.tokens) {
		return conditionToken{}, false
	}
	return p.tokens[p.at], true
}

func (p *conditionParser) keyword(word string) bool {
	token, ok := p.peek()
	if ok && !token.quoted && strings.EqualFold(token.text, word) {
		p.at++
		return true
	}
	return false
}

func (p *conditionParser) or() (*Condition, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.keyword("or") {
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = &Condition{op: "or", children: []*Condition{left, right}}
	}
	return left, nil
}

func (p *conditionParser) and() (*Condition, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.keyword("and") {
		right, err := p.not()
		if err != nil {
			return nil, err
		}
		left = &Condition{op: "and", children: []*Condition{left, right}}
	}
	return left, nil
}

func (p *conditionParser) not() (*Condition, error) {
	if p.keyword("not") {
		inner, err := p.not()
		if err != nil {
			return nil, err
		}
		return &Condition{op: "not", children: []*Condition{inner}}, nil
	}
	return p.primary()
}

func (p *conditionParser) primary() (*Condition, error) {
	token, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("the condition ends too early")
	}
	if !token.quoted && token.text == "(" {
		p.at++
		inner, err := p.or()
		if err != nil {
			return nil, err
		}
		if closing, ok := p.peek(); !ok || closing.quoted || closing.text != ")" {
			return nil, fmt.Errorf("a ( is not closed")
		}
		p.at++
		return inner, nil
	}
	p.at++
	operand, err := parseOperand(token)
	if err != nil {
		return nil, err
	}
	opToken, ok := p.peek()
	if !ok || opToken.quoted || !slices.Contains(comparisonOps, opToken.text) {
		return nil, fmt.Errorf("%s needs a comparison, such as %s == \"yes\"", token.text, token.text)
	}
	p.at++
	valueToken, ok := p.peek()
	if !ok || (!valueToken.quoted && (valueToken.text == "(" || valueToken.text == ")" || slices.Contains(comparisonOps, valueToken.text))) {
		return nil, fmt.Errorf("%s %s needs a value", token.text, opToken.text)
	}
	p.at++
	if opToken.text != "==" && opToken.text != "!=" {
		if _, err := strconv.ParseFloat(valueToken.text, 64); err != nil {
			return nil, fmt.Errorf("%s compares numbers; %q is not a number", opToken.text, valueToken.text)
		}
	}
	return &Condition{op: opToken.text, operand: operand, value: valueToken.text}, nil
}

func parseOperand(token conditionToken) (Operand, error) {
	bad := fmt.Errorf("%q is not something a rule can read: use params.<name>, steps.<id>.result, steps.<id>.exitCode or outputs.<name>", token.text)
	if token.quoted {
		return Operand{}, bad
	}
	parts := strings.Split(token.text, ".")
	switch {
	case len(parts) == 2 && parts[0] == OperandParam && parts[1] != "":
		return Operand{Kind: OperandParam, Name: parts[1]}, nil
	case len(parts) == 2 && parts[0] == OperandOutput && parts[1] != "":
		return Operand{Kind: OperandOutput, Name: parts[1]}, nil
	case len(parts) == 3 && parts[0] == "steps" && parts[1] != "" && (parts[2] == OperandResult || parts[2] == OperandExitCode):
		return Operand{Kind: parts[2], Name: parts[1]}, nil
	}
	return Operand{}, bad
}

// IsRule reports whether a performer is a rule.
func (p *Performer) IsRule() bool { return p != nil && p.Kind == PerformerRule }

// validateRules checks every rule decision of one scope: each condition
// parses and reads only what exists before the decision, and each outcome it
// names is one of the decision's.
func validateRules(tmpl *Template, edges []Edge) Diagnostics {
	var diagnostics Diagnostics
	reaches := adjacency(edges)
	upstream := func(stepID, decisionID string) bool {
		seen := map[string]bool{stepID: true}
		queue := []string{stepID}
		for len(queue) > 0 {
			at := queue[0]
			queue = queue[1:]
			for _, next := range reaches[at] {
				if next == decisionID {
					return true
				}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		return false
	}
	outputs := map[string]bool{}
	for _, node := range tmpl.Nodes {
		for _, name := range node.Captures {
			outputs[name] = true
		}
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		if !node.Performer.IsRule() {
			continue
		}
		path := "nodes." + nodeID + ".performer"
		add := func(code, at, message string) {
			diagnostics = append(diagnostics, diagError(code, at, message))
		}
		if node.Type != NodeTypeDecision {
			add("rule_on_non_decision", path, "a rule decides a decision; it is only valid on decision nodes")
			continue
		}
		rule := node.Performer
		if len(rule.Cases) == 0 {
			add("missing_rule_cases", path+".cases", "a rule needs at least one case")
		}
		if isBlank(rule.Else) {
			add("missing_rule_else", path+".else", "a rule needs an else outcome, taken when no case holds")
		} else if _, ok := node.Next[rule.Else]; !ok {
			add("unknown_rule_outcome", path+".else", fmt.Sprintf("else names %q, which is not an outcome of this decision", rule.Else))
		}
		for i, ruleCase := range rule.Cases {
			casePath := fmt.Sprintf("%s.cases[%d]", path, i)
			if _, ok := node.Next[ruleCase.Then]; !ok {
				add("unknown_rule_outcome", casePath+".then", fmt.Sprintf("then names %q, which is not an outcome of this decision", ruleCase.Then))
			}
			cond, err := ParseCondition(ruleCase.When)
			if err != nil {
				add("invalid_rule_condition", casePath+".when", err.Error())
				continue
			}
			for _, operand := range cond.Operands() {
				switch operand.Kind {
				case OperandParam:
					if _, ok := tmpl.Params[operand.Name]; !ok {
						add("unknown_rule_param", casePath+".when", fmt.Sprintf("%s names no param of this process", operand))
					}
				case OperandOutput:
					if !outputs[operand.Name] {
						add("unknown_rule_output", casePath+".when", fmt.Sprintf("%s names no step output", operand))
					}
				case OperandResult, OperandExitCode:
					step, ok := tmpl.Nodes[operand.Name]
					switch {
					case !ok:
						add("unknown_rule_step", casePath+".when", fmt.Sprintf("%s names no step", operand))
					case step.Type != NodeTypeTask:
						add("unknown_rule_step", casePath+".when", fmt.Sprintf("%s: %q is not a task, so it has no result", operand, operand.Name))
					case !upstream(operand.Name, nodeID):
						add("rule_step_not_upstream", casePath+".when", fmt.Sprintf("%s: step %q does not run before this decision", operand, operand.Name))
					case operand.Kind == OperandExitCode && (step.Performer == nil || step.Performer.Kind != PerformerProgram || step.IsCompound()):
						add("rule_exit_code_not_command", casePath+".when", fmt.Sprintf("%s: only a command step has an exit code", operand))
					}
				}
			}
		}
		for outcome, loop := range node.Loops {
			if isBlank(loop.OnExhausted) {
				add("rule_loop_without_exhaustion", "nodes."+nodeID+".loops."+outcome,
					"a rule's loop needs onExhausted: once its rounds are used up there is nobody to choose otherwise")
			}
		}
	}
	return diagnostics
}
