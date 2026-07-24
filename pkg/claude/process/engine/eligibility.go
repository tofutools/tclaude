package engine

import (
	"fmt"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// CheckEligibility reports why an authoring-valid template cannot execute in
// the deliberately small M2 engine. Authoring errors are returned unchanged;
// execution capability checks never redefine what templates are valid to edit.
//
// The executable shape is a DAG of start, program tasks, human decisions,
// engine-owned parallel forks, and end nodes. Decisions branch to one authored
// outcome; a parallel fork takes every authored branch at once; branches may
// converge on shared nodes and a convergence node may declare join: all. Start
// and task nodes keep exactly one outgoing route.
//
// Authoring validation runs first and already proves the structural parallel
// contract this layer relies on: a parallel node has at least two outgoing
// edges and declares no performer or stages, a join names at least two inbound
// candidates, and every fork reduces at exactly one complete structural
// reducer before leaving its scope (model.validateParallelScopePlan). It also
// guarantees reachability and rejects every cycle except the poison-escalation
// retry loop, whose compound source node is itself ineligible here. So no
// graph-shape or scope walk is repeated in this layer; it only names the
// capabilities the engine cannot execute yet.
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

	for _, nodeID := range sortedNodeIDs(tmpl) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID

		// join: all is executable: a join target activates once its complete
		// candidate input set has settled with at least one arrival. join: any
		// needs branch cancellation semantics the engine does not have (TCL-715).
		if node.Join == model.JoinAny {
			add("unsupported_join", path+".join", "join: any is not executable in this engine yet")
		}
		if node.Retry != nil {
			add("unsupported_retry", path+".retry", "retries and poison handling are not executable in the exclusive-decision engine")
		}
		if node.Plan != nil {
			if node.Plan.Retry != nil {
				add("unsupported_retry", path+".plan.retry", "stage retries are not executable in the exclusive-decision engine")
			}
			if node.Plan.ApprovalRetry != nil {
				add("unsupported_retry", path+".plan.approvalRetry", "approval retries are not executable in the exclusive-decision engine")
			}
		}
		for index, check := range node.Checks {
			if check.Retry != nil {
				add("unsupported_retry", fmt.Sprintf("%s.checks[%d].retry", path, index), "stage retries are not executable in the exclusive-decision engine")
			}
		}
		if node.Review != nil && node.Review.Retry != nil {
			add("unsupported_retry", path+".review.retry", "stage retries are not executable in the exclusive-decision engine")
		}
		if node.Plan != nil || len(node.Checks) > 0 || node.Review != nil {
			add("unsupported_compound_stages", path, "plan, check, and review stages are not executable in the exclusive-decision engine")
		}
		if node.Wait != nil && node.Type != model.NodeTypeWait {
			add("unsupported_wait", path+".wait", "wait configuration is not executable in the exclusive-decision engine")
		}
		if len(node.Captures) > 0 {
			add("unsupported_captures", path+".captures", "runtime captures are not executable in the exclusive-decision engine")
		}
		if node.Performer != nil && node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision {
			add("unsupported_performer", path+".performer", "only task and decision nodes may declare performers in the exclusive-decision engine")
		}

		switch node.Type {
		case model.NodeTypeStart:
			if len(node.Next) != 1 {
				add("unsupported_routing", path+".next", "a start node requires exactly one outgoing route in the exclusive-decision engine")
			}
		case model.NodeTypeTask:
			if node.Performer == nil || node.Performer.Kind != model.PerformerProgram {
				add("unsupported_performer", path+".performer.kind", "the exclusive-decision engine executes only program task performers")
			} else if node.Performer.Contact != nil {
				add("unsupported_contact", path+".performer.contact", "performer contact schedules are not executable in the exclusive-decision engine")
			}
			if len(node.Next) != 1 {
				add("unsupported_routing", path+".next", "a task requires exactly one outgoing route in the exclusive-decision engine")
			}
		case model.NodeTypeDecision:
			if node.Performer == nil || node.Performer.Kind != model.PerformerHuman {
				add("unsupported_performer", path+".performer.kind", "the exclusive-decision engine executes only human deciders; agent deciders are not executable yet")
			} else if node.Performer.Contact != nil {
				add("unsupported_contact", path+".performer.contact", "performer contact schedules are not executable in the exclusive-decision engine")
			}
		case model.NodeTypeEnd:
		case model.NodeTypeParallel:
			// Engine-owned: completing a ready fork settles every authored branch
			// as arrived. Authoring validation already proved the degree and
			// structured-scope contract, so nothing is re-checked here.
		case model.NodeTypeWait:
			add("unsupported_wait", path+".type", "wait nodes are not executable in the exclusive-decision engine")
		}
	}
	if start, ok := tmpl.Nodes[tmpl.Start]; !ok || start.Type != model.NodeTypeStart {
		add("unsupported_sequence_start", "start", "the exclusive-decision engine requires start to name an explicit start node")
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
