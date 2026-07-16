package model

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

var paramRefPattern = regexp.MustCompile(`\{\{\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func Validate(tmpl *Template, edges []Edge) Diagnostics {
	var diagnostics Diagnostics
	if tmpl == nil {
		return Diagnostics{diagError("nil_template", "", "process template is nil")}
	}
	if diagnostics := normalizedGraphCardinalityDiagnostics(NormalizedGraphCardinality{
		Nodes: saturatingCount(len(tmpl.Nodes), MaxNormalizedNodes),
		Edges: saturatingCount(len(edges), MaxNormalizedEdges),
	}); diagnostics.HasErrors() {
		return diagnostics
	}

	diagnostics = append(diagnostics, validateHeader(tmpl)...)
	diagnostics = append(diagnostics, validateNodes(tmpl)...)
	diagnostics = append(diagnostics, validateExpansionCollisions(tmpl)...)
	diagnostics = append(diagnostics, validateEdges(tmpl, edges)...)
	diagnostics = append(diagnostics, validateJoinAndDegree(tmpl, edges)...)
	diagnostics = append(diagnostics, validateParallelScopePlan(tmpl, edges)...)
	diagnostics = append(diagnostics, validateOutcomeRouting(tmpl)...)
	diagnostics = append(diagnostics, validateLoopBudgets(tmpl)...)
	diagnostics = append(diagnostics, validatePoisonEscalations(tmpl)...)
	diagnostics = append(diagnostics, validateReachability(tmpl, edges)...)
	diagnostics = append(diagnostics, validateAcyclic(tmpl, edges)...)
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
		if node.Type != NodeTypeEnd && !isBlank(node.Result) {
			diagnostics = append(diagnostics, diagError("result_on_non_end_node", path+".result", "result is only valid on end nodes"))
		}
		diagnostics = append(diagnostics, validateCaptures(node, path)...)
		switch node.Type {
		case NodeTypeTask:
			if node.Performer == nil {
				diagnostics = append(diagnostics, diagError("missing_performer", path+".performer", "task node requires a performer"))
			} else {
				diagnostics = append(diagnostics, validatePerformer(*node.Performer, path+".performer", false)...)
			}
			checkIDs := map[string]int{}
			for i, check := range node.Checks {
				checkPath := fmt.Sprintf("%s.checks[%d]", path, i)
				diagnostics = append(diagnostics, validateStep(check, checkPath, false)...)
				if check.ID == "" {
					continue
				}
				if first, ok := checkIDs[check.ID]; ok {
					diagnostics = append(diagnostics, diagError("duplicate_step_id", checkPath+".id", fmt.Sprintf("check step id %q is already used by checks[%d]", check.ID, first)))
					continue
				}
				checkIDs[check.ID] = i
			}
			if node.Plan != nil {
				diagnostics = append(diagnostics, validateStep(*node.Plan, path+".plan", true)...)
			}
			if node.Review != nil {
				diagnostics = append(diagnostics, validateStep(*node.Review, path+".review", false)...)
			}
			diagnostics = append(diagnostics, validateRetry(node.Retry, path+".retry")...)
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "task node requires at least one next outcome"))
			}
		case NodeTypeDecision:
			if node.Performer == nil {
				diagnostics = append(diagnostics, diagError("missing_performer", path+".performer", "decision node requires a decider performer"))
			} else {
				diagnostics = append(diagnostics, validatePerformer(*node.Performer, path+".performer", true)...)
			}
			if len(node.Next) == 0 {
				diagnostics = append(diagnostics, diagError("missing_next", path+".next", "decision node requires outcome edges"))
			}
		case NodeTypeWait:
			if node.Wait == nil || (isBlank(node.Wait.Duration) && isBlank(node.Wait.Until) && isBlank(node.Wait.Signal)) {
				diagnostics = append(diagnostics, diagError("missing_wait", path+".wait", "wait node requires duration, until, or signal"))
			}
			if node.Wait != nil {
				diagnostics = append(diagnostics, checkInertParamRef(path+".wait.duration", node.Wait.Duration)...)
				diagnostics = append(diagnostics, validateDuration(path+".wait.duration", node.Wait.Duration)...)
				diagnostics = append(diagnostics, checkInertParamRef(path+".wait.until", node.Wait.Until)...)
				diagnostics = append(diagnostics, validateUntil(path+".wait.until", node.Wait.Until)...)
				diagnostics = append(diagnostics, checkInertParamRef(path+".wait.signal", node.Wait.Signal)...)
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
			diagnostics = append(diagnostics, validateEndResult(node.Result, path+".result")...)
		case NodeTypeParallel:
			if len(node.Next) < 2 {
				diagnostics = append(diagnostics, diagError("parallel_degree", path+".next", "parallel node requires at least two outgoing edges"))
			}
			if node.Performer != nil || node.Plan != nil || len(node.Checks) > 0 || node.Review != nil || node.Retry != nil || node.Wait != nil || !isBlank(node.Result) || len(node.Captures) > 0 {
				diagnostics = append(diagnostics, diagError("parallel_fields", path, "parallel node cannot declare performer, plan, checks, review, retry, wait, result, or captures"))
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

// validateCaptures keeps capture names task-scoped, id-shaped, and unique so
// they can graduate into upstream-capture references without a breaking
// change when the runtime plumbing lands.
func validateCaptures(node Node, path string) Diagnostics {
	if len(node.Captures) == 0 {
		return nil
	}
	var diagnostics Diagnostics
	if node.Type != NodeTypeTask {
		diagnostics = append(diagnostics, diagError("captures_on_non_task_node", path+".captures", "captures are only valid on task nodes"))
	}
	seen := map[string]int{}
	for i, name := range node.Captures {
		capturePath := fmt.Sprintf("%s.captures[%d]", path, i)
		if !idPattern.MatchString(name) {
			diagnostics = append(diagnostics, diagError("invalid_id", capturePath, "capture name must match "+idPattern.String()))
			continue
		}
		if first, ok := seen[name]; ok {
			diagnostics = append(diagnostics, diagError("duplicate_capture", capturePath, fmt.Sprintf("capture name %q is already used by captures[%d]", name, first)))
			continue
		}
		seen[name] = i
	}
	return diagnostics
}

func validateEndResult(result string, path string) Diagnostics {
	if isBlank(result) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "success", "succeeded", "complete", "completed", "done", "pass", "passed", "ok",
		"fail", "failed", "failure", "error",
		"cancel", "canceled", "cancelled":
		return nil
	default:
		return Diagnostics{diagError("invalid_end_result", path, fmt.Sprintf("end node result must be success, failed, or canceled; got %q", result))}
	}
}

func validateStep(step Step, path string, allowApproval bool) Diagnostics {
	var diagnostics Diagnostics
	if strings.TrimSpace(step.ID) == "" {
		diagnostics = append(diagnostics, diagError("missing_step_id", path+".id", "step id is required"))
	} else if !idPattern.MatchString(step.ID) {
		diagnostics = append(diagnostics, diagError("invalid_id", path+".id", "step id must match "+idPattern.String()))
	}
	switch {
	case step.Approval == "":
	case !allowApproval:
		diagnostics = append(diagnostics, diagError("approval_on_non_plan_step", path+".approval", "approval is only valid on plan steps"))
	case step.Approval != PlanApprovalHuman && step.Approval != PlanApprovalAuto:
		diagnostics = append(diagnostics, diagError("invalid_plan_approval", path+".approval", fmt.Sprintf("plan approval must be %s or %s; got %q", PlanApprovalHuman, PlanApprovalAuto, step.Approval)))
	}
	switch {
	case step.ApprovalRetry == nil:
	case !allowApproval:
		diagnostics = append(diagnostics, diagError("approval_retry_on_non_plan_step", path+".approvalRetry", "approvalRetry is only valid on plan steps"))
	case step.Approval != PlanApprovalHuman:
		diagnostics = append(diagnostics, diagError("approval_retry_without_human_approval", path+".approvalRetry", "approvalRetry requires approval: human"))
	}
	diagnostics = append(diagnostics, validatePerformer(step.Performer, path+".performer", false)...)
	diagnostics = append(diagnostics, validateRetry(step.ApprovalRetry, path+".approvalRetry")...)
	diagnostics = append(diagnostics, validateRetry(step.Retry, path+".retry")...)
	return diagnostics
}

// validateExpansionCollisions rejects child-stage id collisions across the
// whole template: node ids may contain dots, so an authored node id can
// collide with a derived child id (for example "implement.do"), and two
// different compound nodes can derive the same child id (for example node "a"
// with check id "do" and node "a.test" both derive "a.test.do"). Either kind
// of collision would wedge the run at expansion time.
func validateExpansionCollisions(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	derivedBy := map[string]string{}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		for _, spec := range ExpandNode(nodeID, tmpl.Nodes[nodeID]) {
			if owner, ok := derivedBy[spec.ChildID]; ok {
				diagnostics = append(diagnostics, diagError(
					"node_id_collides_with_expansion",
					"nodes."+nodeID,
					fmt.Sprintf("compound nodes %q and %q both derive child stage %q", owner, nodeID, spec.ChildID),
				))
				continue
			}
			derivedBy[spec.ChildID] = nodeID
			if _, ok := tmpl.Nodes[spec.ChildID]; ok {
				diagnostics = append(diagnostics, diagError(
					"node_id_collides_with_expansion",
					"nodes."+spec.ChildID,
					fmt.Sprintf("node id %q collides with a child stage of compound node %q", spec.ChildID, nodeID),
				))
			}
		}
	}
	return diagnostics
}

func validatePerformer(performer Performer, path string, decision bool) Diagnostics {
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
	diagnostics = append(diagnostics, validateKindScopedFields(performer, path)...)
	diagnostics = append(diagnostics, validateChoiceOutcomes(performer, path, decision)...)
	diagnostics = append(diagnostics, checkInertParamRef(path+".profile", performer.Profile)...)
	diagnostics = append(diagnostics, checkInertParamRef(path+".timeout", performer.Timeout)...)
	diagnostics = append(diagnostics, validateDuration(path+".timeout", performer.Timeout)...)
	if performer.Contact != nil {
		diagnostics = append(diagnostics, validateDuration(path+".contact.cadence", performer.Contact.Cadence)...)
		if strings.TrimSpace(performer.Contact.Cadence) == "" {
			diagnostics = append(diagnostics, diagError("missing_contact_cadence", path+".contact.cadence", "contact schedule requires cadence"))
		}
		if performer.Contact.Budget <= 0 {
			diagnostics = append(diagnostics, diagError("invalid_contact_budget", path+".contact.budget", "contact schedule requires budget greater than zero"))
		}
		if strings.TrimSpace(performer.Contact.EscalationTarget) == "" {
			diagnostics = append(diagnostics, diagError("missing_escalation_target", path+".contact.escalationTarget", "contact schedule requires an escalation target"))
		}
	}
	return diagnostics
}

// validateKindScopedFields enforces the uniform-contract discipline rule
// (design §2): a kind-scoped field set on the wrong kind is a hard authoring
// error, never something the runtime silently ignores — a stray field the
// editing surface does not even show for the current kind (say, a run
// command on a human performer) must not lurk until a kind switch makes it
// live. Ask/run/args were historically unchecked on wrong kinds; they are
// scoped here alongside the new fields. Prompt is shared by design: agent
// instruction text or a human's long-form context. An unrecognized kind gets
// its own invalid_performer_kind error; skip the scoping noise then.
func validateKindScopedFields(performer Performer, path string) Diagnostics {
	if performer.Kind != PerformerHuman && performer.Kind != PerformerAgent && performer.Kind != PerformerProgram {
		return nil
	}
	var diagnostics Diagnostics
	scoped := func(field string, set bool, kinds ...PerformerKind) {
		if !set || slices.Contains(kinds, performer.Kind) {
			return
		}
		kindNames := make([]string, len(kinds))
		for i, kind := range kinds {
			kindNames[i] = string(kind)
		}
		diagnostics = append(diagnostics, diagError("kind_scoped_field", path+"."+field,
			fmt.Sprintf("%s is only valid on %s performers", field, strings.Join(kindNames, " or "))))
	}
	scoped("ask", !isBlank(performer.Ask), PerformerHuman)
	scoped("choices", len(performer.Choices) > 0, PerformerHuman)
	scoped("choiceOutcomes", len(performer.ChoiceOutcomes) > 0, PerformerHuman)
	scoped("assignee", !isBlank(performer.Assignee), PerformerHuman)
	scoped("prompt", !isBlank(performer.Prompt), PerformerAgent, PerformerHuman)
	scoped("model", !isBlank(performer.Model), PerformerAgent)
	scoped("effort", !isBlank(performer.Effort), PerformerAgent)
	scoped("run", !isBlank(performer.Run), PerformerProgram)
	scoped("args", len(performer.Args) > 0, PerformerProgram)
	for i, choice := range performer.Choices {
		if isBlank(choice) {
			diagnostics = append(diagnostics, diagError("invalid_choice", fmt.Sprintf("%s.choices[%d]", path, i), "choices must not be blank"))
		}
	}
	diagnostics = append(diagnostics, checkInertParamRef(path+".assignee", performer.Assignee)...)
	diagnostics = append(diagnostics, checkInertParamRef(path+".model", performer.Model)...)
	diagnostics = append(diagnostics, checkInertParamRef(path+".effort", performer.Effort)...)
	for i, choice := range performer.Choices {
		diagnostics = append(diagnostics, checkInertParamRef(fmt.Sprintf("%s.choices[%d]", path, i), choice)...)
	}
	return diagnostics
}

// ValidateChoiceRouting defensively validates the persisted performer shape at
// dispatch/reconcile time. Authoring normally catches these diagnostics first,
// but old or manually edited run records must fail loudly rather than expose
// actions that cannot settle an attempt.
func ValidateChoiceRouting(performer Performer, decision bool) error {
	diagnostics := validateChoiceOutcomes(performer, "performer", decision)
	if len(diagnostics) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", diagnostics[0].Path, diagnostics[0].Message)
}

func validateChoiceOutcomes(performer Performer, path string, decision bool) Diagnostics {
	if performer.Kind != PerformerHuman {
		return nil
	}
	if decision {
		if len(performer.ChoiceOutcomes) > 0 {
			return Diagnostics{diagError("choice_outcomes_on_decision", path+".choiceOutcomes",
				"decision performer choices route through outcome edges; choiceOutcomes is not applicable")}
		}
		return nil
	}
	var diagnostics Diagnostics
	labels := make([]string, 0, len(performer.Choices))
	canonical := make(map[string]struct{}, len(performer.Choices))
	for i, raw := range performer.Choices {
		label := strings.TrimSpace(raw)
		choicePath := fmt.Sprintf("%s.choices[%d]", path, i)
		if label == "" {
			continue // invalid_choice is emitted by validateKindScopedFields.
		}
		if raw != label {
			diagnostics = append(diagnostics, diagError("noncanonical_choice", choicePath,
				"choice labels must not have leading or trailing whitespace"))
		}
		for first, existing := range labels {
			if strings.EqualFold(existing, label) {
				diagnostics = append(diagnostics, diagError("duplicate_choice", choicePath,
					fmt.Sprintf("choice %q conflicts with choices[%d] under case-insensitive matching", label, first)))
				break
			}
		}
		labels = append(labels, label)
		canonical[label] = struct{}{}
		outcome, ok := performer.ChoiceOutcomes[label]
		if !ok {
			diagnostics = append(diagnostics, diagError("missing_choice_outcome", path+".choiceOutcomes."+label,
				fmt.Sprintf("choice %q requires an explicit pass or fail outcome", label)))
			continue
		}
		switch strings.TrimSpace(outcome) {
		case "pass", "fail":
		default:
			diagnostics = append(diagnostics, diagError("invalid_choice_outcome", path+".choiceOutcomes."+label,
				fmt.Sprintf("choice outcome must be pass or fail; got %q", outcome)))
		}
	}
	for key := range performer.ChoiceOutcomes {
		if _, ok := canonical[key]; !ok {
			diagnostics = append(diagnostics, diagError("extra_choice_outcome", path+".choiceOutcomes."+key,
				fmt.Sprintf("choice outcome key %q does not exactly match an authored choice label", key)))
		}
	}
	return diagnostics
}

func validateRetry(retry *RetryPolicy, path string) Diagnostics {
	if retry == nil {
		return nil
	}
	var diagnostics Diagnostics
	if retry.MaxAttempts <= 0 {
		diagnostics = append(diagnostics, diagError("invalid_retry_budget", path+".maxAttempts", "retry policy requires maxAttempts greater than zero"))
	}
	switch retry.OnFail {
	case "", RetryModeFeedbackSameSession, RetryModeFreshAttempt:
	default:
		diagnostics = append(diagnostics, diagError("invalid_retry_mode", path+".onFail", fmt.Sprintf("retry onFail must be %s or %s; got %q", RetryModeFeedbackSameSession, RetryModeFreshAttempt, retry.OnFail)))
	}
	diagnostics = append(diagnostics, checkInertParamRef(path+".backoff", retry.Backoff)...)
	diagnostics = append(diagnostics, validateDuration(path+".backoff", retry.Backoff)...)
	return diagnostics
}

// validateDuration rejects duration-ish fields that Go's time.ParseDuration
// cannot parse or that are not strictly positive, so authoring-time failure
// beats runtime failure. Blank values are optional. Duration fields are never
// interpolated (see checkInertParamRef), so a `{{ params.x }}` reference is a
// literal that fails to parse here rather than being skipped.
func validateDuration(path, value string) Diagnostics {
	if isBlank(value) {
		return nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return Diagnostics{diagError("invalid_duration", path,
			fmt.Sprintf("must be a Go duration such as 30s, 5m, or 1h30m; got %q", value))}
	}
	if d <= 0 {
		return Diagnostics{diagError("invalid_duration", path,
			fmt.Sprintf("must be a positive duration; got %q", value))}
	}
	return nil
}

// validateUntil rejects absolute wait deadlines that the executor's timer
// boundary cannot parse. Blank values are optional; nonblank values use the
// same trimmed RFC3339 contract as commandDueAt.
func validateUntil(path, value string) Diagnostics {
	if isBlank(value) {
		return nil
	}
	if _, err := ParseRFC3339(value); err != nil {
		return Diagnostics{diagError("invalid_until", path,
			fmt.Sprintf("must be an RFC3339 timestamp such as 2026-07-14T10:03:44Z; got %q", value))}
	}
	return nil
}

// checkInertParamRef warns when a param reference appears in a field that is not
// interpolated. Only performer prompt/ask/run/args are templatable; references
// elsewhere (profile, timeout, backoff, wait fields) are used literally.
func checkInertParamRef(path, value string) Diagnostics {
	if paramRefPattern.MatchString(value) {
		return Diagnostics{diagWarning("inert_param_ref", path,
			"param references are only interpolated in performer prompt, ask, run, and args; this field is used literally")}
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

// validateOutcomeRouting checks each node's outcome map against the runtime
// edge-resolution rules (plan.ResolvePassEdge / model.FailTarget), so edges
// that can never be taken — or plain-pass outcomes that have nowhere to go and
// would stall the run — surface at authoring time. Decision nodes are exempt:
// their edges are matched exactly against free-form verdicts.
func validateOutcomeRouting(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		// A single edge always resolves via the runtime's lone-edge fallback,
		// and missing_next already covers empty maps.
		if len(node.Next) < 2 {
			continue
		}
		path := "nodes." + nodeID + ".next"
		passWinner := firstPresentLabel(node.Next, passOutcomeLabels[:])
		switch node.Type {
		case NodeTypeTask:
			// Cancel-labeled edges can never be taken from a task: the runtime
			// classifies cancel verdicts as failures (state.IsFailOutcome), and
			// fail routing (FailTarget) only consults the fail vocabulary.
			for _, outcome := range sortedKeys(node.Next) {
				if IsCanceledResult(outcome) {
					diagnostics = append(diagnostics, diagWarning("dead_edge", path+"."+outcome,
						fmt.Sprintf("cancel verdicts route through the fail edge; outcome %q can never be taken", outcome)))
				}
			}
			if passWinner == "" {
				failOnly := true
				for _, outcome := range sortedKeys(node.Next) {
					if !IsFailOutcomeLabel(outcome) && !IsCanceledResult(outcome) {
						failOnly = false
						break
					}
				}
				if failOnly {
					diagnostics = append(diagnostics, diagError("missing_pass_edge", path,
						"task node has only fail/cancel edges; a passing attempt cannot route and stalls the run (add a pass, done, success, or next edge)"))
				} else {
					diagnostics = append(diagnostics, diagWarning("missing_pass_edge", path,
						"task node has no pass, done, success, or next edge; an attempt that passes without a custom verdict matching an edge stalls the run"))
				}
			} else {
				for _, outcome := range passOutcomeLabels {
					if outcome != passWinner && node.Next[outcome] != "" {
						// Not a dead edge: pass routing checks the exact attempt
						// verdict before the alias fallback, so an exact-match
						// verdict still takes this edge.
						diagnostics = append(diagnostics, diagWarning("ambiguous_pass_edge", path+"."+outcome,
							fmt.Sprintf("outcome edge %q is shadowed by %q for plain pass outcomes (resolution order: %s); an exact %q attempt verdict still routes here", outcome, passWinner, strings.Join(passOutcomeLabels[:], ", "), outcome)))
					}
				}
			}
			failWinner := firstPresentLabel(node.Next, failOutcomeLabels[:])
			for _, outcome := range failOutcomeLabels {
				if failWinner != "" && outcome != failWinner && node.Next[outcome] != "" {
					diagnostics = append(diagnostics, diagWarning("ambiguous_fail_edge", path+"."+outcome,
						fmt.Sprintf("fail edge %q is shadowed by %q (resolution order: %s)", outcome, failWinner, strings.Join(failOutcomeLabels[:], ", "))))
				}
			}
		case NodeTypeWait, NodeTypeStart:
			if passWinner == "" {
				diagnostics = append(diagnostics, diagError("missing_pass_edge", path,
					fmt.Sprintf("%s node routes only through a pass edge; none of its outcomes is pass, done, success, or next, so it stalls the run", node.Type)))
				continue
			}
			for _, outcome := range sortedKeys(node.Next) {
				if outcome != passWinner {
					diagnostics = append(diagnostics, diagWarning("dead_edge", path+"."+outcome,
						fmt.Sprintf("%s node only follows its %q edge; outcome %q can never be taken", node.Type, passWinner, outcome)))
				}
			}
		}
	}
	return diagnostics
}

func firstPresentLabel(next Next, labels []string) string {
	for _, label := range labels {
		if next[label] != "" {
			return label
		}
	}
	return ""
}

// validateLoopBudgets surfaces budget-less retry loops (design §8a). The one
// sanctioned v1 loop is the poison-escalation retry edge back into a compound
// task; without a declared retry budget on that compound, every escalation
// round runs a single attempt and immediately re-poisons, so the loop's real
// window is implicit. Advisory: the loop stays human-gated either way.
func validateLoopBudgets(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	for _, sourceID := range sortedKeys(tmpl.Nodes) {
		source := tmpl.Nodes[sourceID]
		if !source.IsCompound() || source.Retry != nil {
			continue
		}
		decisionID := FailTarget(source.Next)
		decision, ok := tmpl.Nodes[decisionID]
		if decisionID == "" || !ok || decision.Type != NodeTypeDecision || decision.Next["retry"] != sourceID {
			continue
		}
		if decision.Performer == nil || decision.Performer.Kind != PerformerHuman {
			// Not the sanctioned loop shape; validateAcyclic reports the cycle.
			continue
		}
		diagnostics = append(diagnostics, diagWarning("retry_loop_without_budget", "nodes."+sourceID+".retry",
			fmt.Sprintf("retry loop through decision %q has no declared budget on %q; each round runs one attempt before re-escalating (declare retry.maxAttempts to size the loop window)", decisionID, sourceID)))
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

// validatePoisonEscalations reserves the human decision reached by a compound
// fail edge for the engine's generation-bound poison resolution bridge. The
// v1 bridge intentionally supports only retrying that compound or canceling
// the run, so an unsupported choice is rejected before a run can be created.
func validatePoisonEscalations(tmpl *Template) Diagnostics {
	var diagnostics Diagnostics
	for _, sourceID := range sortedKeys(tmpl.Nodes) {
		source := tmpl.Nodes[sourceID]
		if !source.IsCompound() {
			continue
		}
		decisionID := FailTarget(source.Next)
		decision, ok := tmpl.Nodes[decisionID]
		if decisionID == "" || !ok || decision.Type != NodeTypeDecision || decision.Performer == nil || decision.Performer.Kind != PerformerHuman {
			continue
		}
		path := "nodes." + decisionID + ".next"
		if len(decision.Next) != 2 {
			diagnostics = append(diagnostics, diagError("invalid_poison_escalation", path, "poison escalation requires exactly retry and cancel choices"))
		}
		if retryTarget, ok := decision.Next["retry"]; !ok || retryTarget != sourceID {
			diagnostics = append(diagnostics, diagError("invalid_poison_escalation", path+".retry", fmt.Sprintf("poison escalation retry must target compound node %q", sourceID)))
		}
		cancelTarget, ok := decision.Next["cancel"]
		cancelNode, targetOK := tmpl.Nodes[cancelTarget]
		if !ok || !targetOK || cancelNode.Type != NodeTypeEnd || !IsCanceledResult(cancelNode.Result) {
			diagnostics = append(diagnostics, diagError("invalid_poison_escalation", path+".cancel", "poison escalation cancel must target an end node with result canceled"))
		}
		if tmpl.Start == decisionID {
			diagnostics = append(diagnostics, diagError("invalid_poison_escalation", "start", fmt.Sprintf("poison escalation decision %q cannot also be the template start", decisionID)))
		}
		for _, incomingID := range sortedKeys(tmpl.Nodes) {
			incoming := tmpl.Nodes[incomingID].Next
			for _, outcome := range sortedKeys(incoming) {
				target := incoming[outcome]
				if target != decisionID || incomingID == sourceID && IsFailOutcomeLabel(outcome) {
					continue
				}
				diagnostics = append(diagnostics, diagError(
					"invalid_poison_escalation",
					"nodes."+incomingID+".next."+outcome,
					fmt.Sprintf("poison escalation decision %q may only be entered by compound node %q's fail edge", decisionID, sourceID),
				))
			}
		}
	}
	return diagnostics
}

func validateAcyclic(tmpl *Template, edges []Edge) Diagnostics {
	acyclicEdges := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		if IsPoisonEscalationRetryEdge(tmpl, edge) {
			continue
		}
		acyclicEdges = append(acyclicEdges, edge)
	}
	adj := adjacency(acyclicEdges)
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

// IsPoisonEscalationRetryEdge recognizes the one v1 loop that is not an
// arbitrary graph cycle: a compound task's fail edge offers a human decision,
// whose retry edge points back to that same task. Runtime planning intercepts
// this edge as an audited block resolution; it never re-activates a completed
// graph node through the ordinary edge machinery.
func IsPoisonEscalationRetryEdge(tmpl *Template, edge Edge) bool {
	if tmpl == nil || edge.Outcome != "retry" {
		return false
	}
	decision, decisionOK := tmpl.Nodes[edge.From]
	target, targetOK := tmpl.Nodes[edge.To]
	return decisionOK && targetOK && decision.Type == NodeTypeDecision && decision.Performer != nil && decision.Performer.Kind == PerformerHuman &&
		target.IsCompound() && FailTarget(target.Next) == edge.From
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
