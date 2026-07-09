package model

import (
	"fmt"
	"regexp"
	"strings"
)

var paramRefPattern = regexp.MustCompile(`\{\{\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func Validate(tmpl *Template, edges []Edge) Diagnostics {
	var diagnostics Diagnostics
	if tmpl == nil {
		return Diagnostics{diagError("nil_template", "", "process template is nil")}
	}

	diagnostics = append(diagnostics, validateHeader(tmpl)...)
	diagnostics = append(diagnostics, validateNodes(tmpl)...)
	diagnostics = append(diagnostics, validateEdges(tmpl, edges)...)
	diagnostics = append(diagnostics, validateReachability(tmpl, edges)...)
	diagnostics = append(diagnostics, validateAcyclic(edges)...)
	diagnostics = append(diagnostics, validateParamRefs(tmpl)...)
	diagnostics = append(diagnostics, validateLayout(tmpl)...)
	return diagnostics
}

func validateHeader(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	if tmpl.APIVersion != APIVersion {
		diagnostics = append(diagnostics, diagError("invalid_api_version", "apiVersion", fmt.Sprintf("apiVersion must be %q", APIVersion)))
	}
	if tmpl.Kind != Kind {
		diagnostics = append(diagnostics, diagError("invalid_kind", "kind", fmt.Sprintf("kind must be %q", Kind)))
	}
	if strings.TrimSpace(tmpl.ID) == "" {
		diagnostics = append(diagnostics, diagError("missing_id", "id", "template id is required"))
	} else if !idPattern.MatchString(tmpl.ID) {
		diagnostics = append(diagnostics, diagError("invalid_id", "id", "template id must match "+idPattern.String()))
	}
	if strings.TrimSpace(tmpl.Start) == "" {
		diagnostics = append(diagnostics, diagError("missing_start", "start", "top-level start node is required"))
	} else if _, ok := tmpl.Nodes[tmpl.Start]; !ok {
		diagnostics = append(diagnostics, diagError("unknown_start", "start", fmt.Sprintf("start node %q is not declared", tmpl.Start)))
	}
	if len(tmpl.Nodes) == 0 {
		diagnostics = append(diagnostics, diagError("missing_nodes", "nodes", "at least one node is required"))
	}
	for _, paramID := range sortedKeys(tmpl.Params) {
		if !idPattern.MatchString(paramID) {
			diagnostics = append(diagnostics, diagError("invalid_id", "params."+paramID, "param id must match "+idPattern.String()))
		}
	}
	return diagnostics
}

func validateNodes(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	startNodeCount := 0
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		if !idPattern.MatchString(nodeID) {
			diagnostics = append(diagnostics, diagError("invalid_id", path, "node id must match "+idPattern.String()))
		}
		switch node.Type {
		case NodeTypeTask:
			if node.Performer == nil {
				diagnostics = append(diagnostics, diagError("missing_performer", path+".performer", "task node requires a performer"))
			} else {
				diagnostics = append(diagnostics, validatePerformer(*node.Performer, path+".performer")...)
			}
			for i, check := range node.Checks {
				diagnostics = append(diagnostics, validateStep(check, fmt.Sprintf("%s.checks[%d]", path, i))...)
			}
			if node.Plan != nil {
				diagnostics = append(diagnostics, validateStep(*node.Plan, path+".plan")...)
			}
			if node.Review != nil {
				diagnostics = append(diagnostics, validateStep(*node.Review, path+".review")...)
			}
			diagnostics = append(diagnostics, validateRetry(node.Retry, path+".retry")...)
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "task node requires at least one next outcome"))
			}
		case NodeTypeDecision:
			if node.Performer == nil {
				diagnostics = append(diagnostics, diagError("missing_performer", path+".performer", "decision node requires a decider performer"))
			} else {
				diagnostics = append(diagnostics, validatePerformer(*node.Performer, path+".performer")...)
			}
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "decision node requires outcome edges"))
			}
		case NodeTypeWait:
			if node.Wait == nil || (isBlank(node.Wait.Duration) && isBlank(node.Wait.Until) && isBlank(node.Wait.Signal)) {
				diagnostics = append(diagnostics, diagError("missing_wait", path+".wait", "wait node requires duration, until, or signal"))
			}
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "wait node requires a next target"))
			}
		case NodeTypeStart:
			startNodeCount++
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "start node requires a next target"))
			}
		case NodeTypeEnd:
			if len(node.Next) > 0 {
				diagnostics = append(diagnostics, diagError("end_has_next", path+".next", "end node must not have outgoing edges"))
			}
		default:
			diagnostics = append(diagnostics, diagError("invalid_node_type", path+".type", fmt.Sprintf("unsupported node type %q", node.Type)))
		}
	}
	if startNodeCount > 1 {
		diagnostics = append(diagnostics, diagError("multiple_start_nodes", "nodes", "at most one node may have type start"))
	}
	return diagnostics
}

func validateStep(step Step, path string) Diagnostics {
	var diagnostics Diagnostics
	if strings.TrimSpace(step.ID) == "" {
		diagnostics = append(diagnostics, diagError("missing_step_id", path+".id", "step id is required"))
	}
	diagnostics = append(diagnostics, validatePerformer(step.Performer, path+".performer")...)
	diagnostics = append(diagnostics, validateRetry(step.Retry, path+".retry")...)
	return diagnostics
}

func validatePerformer(performer Performer, path string) Diagnostics {
	var diagnostics Diagnostics
	switch performer.Kind {
	case PerformerAgent:
		if isBlank(performer.Prompt) {
			diagnostics = append(diagnostics, diagError("missing_prompt", path+".prompt", "agent performer requires prompt"))
		}
	case PerformerHuman:
		if isBlank(performer.Ask) && isBlank(performer.Prompt) {
			diagnostics = append(diagnostics, diagError("missing_prompt", path+".ask", "human performer requires ask or prompt"))
		}
	case PerformerProgram:
		if isBlank(performer.Run) {
			diagnostics = append(diagnostics, diagError("missing_run", path+".run", "program performer requires run"))
		}
	default:
		diagnostics = append(diagnostics, diagError("invalid_performer_kind", path+".kind", fmt.Sprintf("unsupported performer kind %q", performer.Kind)))
	}
	return diagnostics
}

func validateRetry(retry *RetryPolicy, path string) Diagnostics {
	if retry == nil {
		return nil
	}
	if retry.MaxAttempts <= 0 {
		return Diagnostics{diagError("invalid_retry_budget", path+".maxAttempts", "retry policy requires maxAttempts greater than zero")}
	}
	return nil
}

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func validateEdges(tmpl *Template, edges []Edge) Diagnostics {
	var diagnostics Diagnostics
	for _, edge := range edges {
		if edge.From == "" {
			continue
		}
		if strings.TrimSpace(edge.Outcome) == "" {
			diagnostics = append(diagnostics, diagError("missing_outcome", "nodes."+edge.From+".next", "next outcome label is required"))
		}
		if strings.TrimSpace(edge.To) == "" {
			diagnostics = append(diagnostics, diagError("missing_target", "nodes."+edge.From+".next."+edge.Outcome, "next target is required"))
			continue
		}
		if _, ok := tmpl.Nodes[edge.To]; !ok {
			diagnostics = append(diagnostics, diagError("unknown_target", "nodes."+edge.From+".next."+edge.Outcome, fmt.Sprintf("target node %q is not declared", edge.To)))
		}
	}
	return diagnostics
}

func validateReachability(tmpl *Template, edges []Edge) Diagnostics {
	if tmpl.Start == "" {
		return nil
	}
	adj := adjacency(edges)
	seen := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, to := range adj[id] {
			if _, ok := tmpl.Nodes[to]; ok {
				visit(to)
			}
		}
	}
	if _, ok := tmpl.Nodes[tmpl.Start]; ok {
		visit(tmpl.Start)
	}

	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		if !seen[nodeID] {
			diagnostics = append(diagnostics, diagError("unreachable_node", "nodes."+nodeID, fmt.Sprintf("node %q is not reachable from start", nodeID)))
		}
	}
	return diagnostics
}

func validateAcyclic(edges []Edge) Diagnostics {
	adj := adjacency(edges)
	const (
		unseen = 0
		active = 1
		done   = 2
	)
	state := map[string]int{}
	var diagnostics Diagnostics
	var visit func(id string, stack []string)
	visit = func(id string, stack []string) {
		switch state[id] {
		case active:
			cycle := append(stack, id)
			diagnostics = append(diagnostics, diagError("graph_cycle", "nodes."+id, "arbitrary graph cycles are not supported in v1: "+strings.Join(cycle, " -> ")))
			return
		case done:
			return
		}
		state[id] = active
		for _, to := range adj[id] {
			nextStack := append(append([]string(nil), stack...), id)
			visit(to, nextStack)
		}
		state[id] = done
	}

	for _, id := range sortedKeys(adj) {
		if state[id] == unseen {
			visit(id, nil)
		}
	}
	return diagnostics
}

func adjacency(edges []Edge) map[string][]string {
	adj := map[string][]string{}
	for _, edge := range edges {
		if edge.From == "" {
			continue
		}
		adj[edge.From] = append(adj[edge.From], edge.To)
	}
	return adj
}

func validateParamRefs(tmpl *Template) Diagnostics {
	declared := map[string]bool{}
	for name := range tmpl.Params {
		declared[name] = true
	}

	var diagnostics Diagnostics
	diagnostics = append(diagnostics, checkProseParamRefs(declared, "name", tmpl.Name)...)
	diagnostics = append(diagnostics, checkProseParamRefs(declared, "description", tmpl.Description)...)
	diagnostics = append(diagnostics, checkProseParamRefs(declared, "doc", tmpl.Doc)...)
	for _, paramID := range sortedKeys(tmpl.Params) {
		param := tmpl.Params[paramID]
		path := "params." + paramID
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".name", param.Name)...)
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".description", param.Description)...)
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".doc", param.Doc)...)
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".name", node.Name)...)
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".description", node.Description)...)
		diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".doc", node.Doc)...)
		if node.Performer != nil {
			diagnostics = append(diagnostics, checkPerformerParamRefs(declared, path+".performer", *node.Performer)...)
		}
		if node.Plan != nil {
			diagnostics = append(diagnostics, checkStepParamRefs(declared, path+".plan", *node.Plan)...)
		}
		for i, check := range node.Checks {
			diagnostics = append(diagnostics, checkStepParamRefs(declared, fmt.Sprintf("%s.checks[%d]", path, i), check)...)
		}
		if node.Review != nil {
			diagnostics = append(diagnostics, checkStepParamRefs(declared, path+".review", *node.Review)...)
		}
	}
	return diagnostics
}

func checkStepParamRefs(declared map[string]bool, path string, step Step) Diagnostics {
	var diagnostics Diagnostics
	diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".name", step.Name)...)
	diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".description", step.Description)...)
	diagnostics = append(diagnostics, checkProseParamRefs(declared, path+".doc", step.Doc)...)
	diagnostics = append(diagnostics, checkPerformerParamRefs(declared, path+".performer", step.Performer)...)
	return diagnostics
}

func checkPerformerParamRefs(declared map[string]bool, path string, performer Performer) Diagnostics {
	var diagnostics Diagnostics
	diagnostics = append(diagnostics, checkExecutableParamRefs(declared, path+".prompt", performer.Prompt)...)
	diagnostics = append(diagnostics, checkExecutableParamRefs(declared, path+".ask", performer.Ask)...)
	diagnostics = append(diagnostics, checkExecutableParamRefs(declared, path+".run", performer.Run)...)
	for i, arg := range performer.Args {
		diagnostics = append(diagnostics, checkExecutableParamRefs(declared, fmt.Sprintf("%s.args[%d]", path, i), arg)...)
	}
	return diagnostics
}

func checkExecutableParamRefs(declared map[string]bool, path, value string) Diagnostics {
	return checkParamRefs(declared, path, value, SeverityError)
}

func checkProseParamRefs(declared map[string]bool, path, value string) Diagnostics {
	return checkParamRefs(declared, path, value, SeverityWarning)
}

func checkParamRefs(declared map[string]bool, path, value string, severity Severity) Diagnostics {
	var diagnostics Diagnostics
	for _, match := range paramRefPattern.FindAllStringSubmatch(value, -1) {
		name := match[1]
		if declared[name] {
			continue
		}
		message := fmt.Sprintf("reference to undeclared param %q", name)
		if severity == SeverityWarning {
			diagnostics = append(diagnostics, diagWarning("undeclared_param_ref", path, message))
		} else {
			diagnostics = append(diagnostics, diagError("undeclared_param_ref", path, message))
		}
	}
	return diagnostics
}

func validateLayout(tmpl *Template) Diagnostics {
	if tmpl.Layout == nil {
		return nil
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(tmpl.Layout.Nodes) {
		if _, ok := tmpl.Nodes[nodeID]; !ok {
			diagnostics = append(diagnostics, diagWarning("stale_layout_node", "layout.nodes."+nodeID, fmt.Sprintf("layout references undeclared node %q", nodeID)))
		}
	}
	return diagnostics
}
