package engine

import (
	"fmt"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// CheckEligibility reports why an authoring-valid template cannot execute in
// the deliberately small M1 engine. Authoring errors are returned unchanged;
// execution capability checks never redefine what templates are valid to edit.
func CheckEligibility(tmpl *model.Template) model.Diagnostics {
	edges, budgetDiagnostics := model.NormalizeEdgesWithinBudget(tmpl)
	if budgetDiagnostics.HasErrors() {
		return budgetDiagnostics.Errors()
	}
	authoringDiagnostics := model.Validate(tmpl, edges)
	if authoringDiagnostics.HasErrors() {
		return authoringDiagnostics.Errors()
	}

	var diagnostics model.Diagnostics
	add := func(code, path, message string) {
		diagnostics = append(diagnostics, model.Diagnostic{
			Severity: model.SeverityError,
			Code:     code,
			Path:     path,
			Message:  message,
		})
	}

	nodeIDs := sortedNodeIDs(tmpl)
	endCount := 0
	taskCount := 0
	pathShapeOK := true
	incoming := make(map[string]int, len(tmpl.Nodes))
	for _, nodeID := range nodeIDs {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		for _, target := range node.Next {
			incoming[target]++
		}

		if node.Join != "" {
			add("unsupported_join", path+".join", "joins are not executable in the sequential MVP")
			pathShapeOK = false
		}
		if node.Retry != nil {
			add("unsupported_retry", path+".retry", "retries and poison handling are not executable in the sequential MVP")
		}
		if node.Plan != nil {
			if node.Plan.Retry != nil {
				add("unsupported_retry", path+".plan.retry", "stage retries are not executable in the sequential MVP")
			}
			if node.Plan.ApprovalRetry != nil {
				add("unsupported_retry", path+".plan.approvalRetry", "approval retries are not executable in the sequential MVP")
			}
		}
		for index, check := range node.Checks {
			if check.Retry != nil {
				add("unsupported_retry", fmt.Sprintf("%s.checks[%d].retry", path, index), "stage retries are not executable in the sequential MVP")
			}
		}
		if node.Review != nil && node.Review.Retry != nil {
			add("unsupported_retry", path+".review.retry", "stage retries are not executable in the sequential MVP")
		}
		if node.Plan != nil || len(node.Checks) > 0 || node.Review != nil {
			add("unsupported_compound_stages", path, "plan, check, and review stages are not executable in the sequential MVP")
		}
		if node.Wait != nil && node.Type != model.NodeTypeWait {
			add("unsupported_wait", path+".wait", "wait configuration is not executable in the sequential MVP")
		}
		if len(node.Captures) > 0 {
			add("unsupported_captures", path+".captures", "runtime captures are not executable in the sequential MVP")
		}
		if node.Performer != nil && node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision {
			add("unsupported_performer", path+".performer", "only program task nodes may declare performers in the sequential MVP")
		}

		switch node.Type {
		case model.NodeTypeStart:
			if len(node.Next) != 1 {
				add("unsupported_routing", path+".next", "a sequential start node requires exactly one outgoing route")
				pathShapeOK = false
			}
		case model.NodeTypeTask:
			taskCount++
			if node.Performer == nil || node.Performer.Kind != model.PerformerProgram {
				add("unsupported_performer", path+".performer.kind", "the sequential MVP executes only program performers")
			} else if node.Performer.Contact != nil {
				add("unsupported_contact", path+".performer.contact", "performer contact schedules are not executable in the sequential MVP")
			}
			if len(node.Next) != 1 {
				add("unsupported_routing", path+".next", "a sequential task requires exactly one outgoing route")
				pathShapeOK = false
			}
		case model.NodeTypeEnd:
			endCount++
		case model.NodeTypeDecision:
			add("unsupported_decision", path+".type", "decision nodes are not executable in the sequential MVP")
			pathShapeOK = false
		case model.NodeTypeParallel:
			add("unsupported_parallel", path+".type", "parallel forks are not executable in the sequential MVP")
			pathShapeOK = false
		case model.NodeTypeWait:
			add("unsupported_wait", path+".type", "wait nodes are not executable in the sequential MVP")
			pathShapeOK = false
		default:
			pathShapeOK = false // authoring validation normally makes this unreachable.
		}
	}

	for _, nodeID := range nodeIDs {
		want := 1
		if nodeID == tmpl.Start {
			want = 0
		}
		if incoming[nodeID] != want {
			add("unsupported_inbound_routing", "nodes."+nodeID,
				fmt.Sprintf("a sequential path requires %d incoming route(s); found %d", want, incoming[nodeID]))
			pathShapeOK = false
		}
	}
	if start, ok := tmpl.Nodes[tmpl.Start]; !ok || start.Type != model.NodeTypeStart {
		add("unsupported_sequence_start", "start", "the sequential MVP requires start to name an explicit start node")
		pathShapeOK = false
	}
	if taskCount == 0 {
		add("missing_program_task", "nodes", "the sequential MVP requires at least one program task")
		pathShapeOK = false
	}
	if endCount != 1 {
		add("unsupported_end_count", "nodes", fmt.Sprintf("a sequential path requires exactly one end node; found %d", endCount))
		pathShapeOK = false
	}

	if pathShapeOK {
		seen := make(map[string]bool, len(tmpl.Nodes))
		current := tmpl.Start
		for {
			if seen[current] {
				add("unsupported_cycle", "nodes."+current, "the sequential execution path contains a cycle")
				break
			}
			seen[current] = true
			node := tmpl.Nodes[current]
			if node.Type == model.NodeTypeEnd {
				if len(seen) != len(tmpl.Nodes) {
					add("unsupported_disconnected_route", "nodes", "every node must lie on the single sequential execution path")
				}
				break
			}
			current = soleTarget(node.Next)
		}
	}
	return diagnostics
}

func RequireEligible(tmpl *model.Template) error {
	diagnostics := CheckEligibility(tmpl)
	if len(diagnostics) == 0 {
		return nil
	}
	return &EligibilityError{Diagnostics: diagnostics}
}

func sortedNodeIDs(tmpl *model.Template) []string {
	if tmpl == nil {
		return nil
	}
	ids := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func soleTarget(next model.Next) string {
	for _, target := range next {
		return target
	}
	return ""
}
