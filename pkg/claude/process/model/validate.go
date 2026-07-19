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
	diagnostics := newTemplateDiagnosticCollector(nil)
	validateWithDiagnosticCollector(tmpl, edges, diagnostics)
	return diagnostics.Diagnostics()
}

func validateWithDiagnosticCollector(tmpl *Template, edges []Edge, diagnostics *templateDiagnosticCollector) {
	if tmpl == nil {
		diagnostics.Add(diagError("nil_template", "", "process template is nil"))
		return
	}
	if cardinalityDiagnostics := normalizedGraphCardinalityDiagnostics(NormalizedGraphCardinality{
		Nodes: saturatingCount(len(tmpl.Nodes), MaxNormalizedNodes),
		Edges: saturatingCount(len(edges), MaxNormalizedEdges),
	}); cardinalityDiagnostics.HasErrors() {
		diagnostics.AddAll(cardinalityDiagnostics)
		return
	}

	if !validateHeader(tmpl, diagnostics) ||
		!validateNodes(tmpl, diagnostics) ||
		!validateExpansionCollisions(tmpl, diagnostics) {
		return
	}
	for _, validate := range []func() Diagnostics{
		func() Diagnostics { return validateEdges(tmpl, edges) },
		func() Diagnostics { return validateJoinAndDegree(tmpl, edges) },
		func() Diagnostics { return validateParallelScopePlan(tmpl, edges) },
		func() Diagnostics { return validateOutcomeRouting(tmpl) },
		func() Diagnostics { return validateLoopBudgets(tmpl) },
	} {
		if !diagnostics.AddAll(validate()) {
			return
		}
	}
	if !validatePoisonEscalations(tmpl, diagnostics) {
		return
	}
	for _, validate := range []func() Diagnostics{
		func() Diagnostics { return validateReachability(tmpl, edges) },
		func() Diagnostics { return validateAcyclic(tmpl, edges) },
	} {
		if !diagnostics.AddAll(validate()) {
			return
		}
	}
	validateParamRefs(tmpl, diagnostics)
	if diagnostics.Exhausted() {
		return
	}
	validateLayout(tmpl, diagnostics)
}

func validateHeader(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	if tmpl.APIVersion != APIVersion {
		if !diagnostics.Add(diagError("invalid_api_version", "apiVersion", fmt.Sprintf("apiVersion must be %q", APIVersion))) {
			return false
		}
	}
	if tmpl.Kind != Kind {
		if !diagnostics.Add(diagError("invalid_kind", "kind", fmt.Sprintf("kind must be %q", Kind))) {
			return false
		}
	}
	if strings.TrimSpace(tmpl.ID) == "" {
		if !diagnostics.Add(diagError("missing_id", "id", "template id is required")) {
			return false
		}
	} else if !idPattern.MatchString(tmpl.ID) {
		if !diagnostics.Add(diagError("invalid_id", "id", "template id must match "+idPattern.String())) {
			return false
		}
	}
	if strings.TrimSpace(tmpl.Start) == "" {
		if !diagnostics.Add(diagError("missing_start", "start", "top-level start node is required")) {
			return false
		}
	} else if _, ok := tmpl.Nodes[tmpl.Start]; !ok {
		if !diagnostics.Add(diagError("unknown_start", "start", fmt.Sprintf("start node %q is not declared", tmpl.Start))) {
			return false
		}
	}
	if len(tmpl.Nodes) == 0 {
		if !diagnostics.Add(diagError("missing_nodes", "nodes", "at least one node is required")) {
			return false
		}
	}
	for _, paramID := range sortedKeys(tmpl.Params) {
		if !idPattern.MatchString(paramID) {
			if !diagnostics.Add(diagError("invalid_id", "params."+paramID, "param id must match "+idPattern.String())) {
				return false
			}
		}
	}
	return true
}

func validateNodes(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	startNodeCount := 0
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		if !idPattern.MatchString(nodeID) {
			if !diagnostics.Add(diagError("invalid_id", path, "node id must match "+idPattern.String())) {
				return false
			}
		}
		if node.Type != NodeTypeEnd && !isBlank(node.Result) {
			if !diagnostics.Add(diagError("result_on_non_end_node", path+".result", "result is only valid on end nodes")) {
				return false
			}
		}
		if !validateCaptures(node, path, diagnostics) {
			return false
		}
		switch node.Type {
		case NodeTypeTask:
			if node.Performer == nil {
				if !diagnostics.Add(diagError("missing_performer", path+".performer", "task node requires a performer")) {
					return false
				}
			} else {
				if !validatePerformer(*node.Performer, path+".performer", false, diagnostics) {
					return false
				}
			}
			checkIDs := map[string]int{}
			for i, check := range node.Checks {
				checkPath := fmt.Sprintf("%s.checks[%d]", path, i)
				if !validateStep(check, checkPath, false, diagnostics) {
					return false
				}
				if check.ID == "" {
					continue
				}
				if first, ok := checkIDs[check.ID]; ok {
					if !diagnostics.Add(diagError("duplicate_step_id", checkPath+".id", fmt.Sprintf("check step id %q is already used by checks[%d]", check.ID, first))) {
						return false
					}
					continue
				}
				checkIDs[check.ID] = i
			}
			if node.Plan != nil {
				if !validateStep(*node.Plan, path+".plan", true, diagnostics) {
					return false
				}
			}
			if node.Review != nil {
				if !validateStep(*node.Review, path+".review", false, diagnostics) {
					return false
				}
			}
			if !diagnostics.AddAll(validateRetry(node.Retry, path+".retry")) {
				return false
			}
			if len(node.Next) == 0 {
				if !diagnostics.Add(diagError("missing_next", path+".next", "task node requires at least one next outcome")) {
					return false
				}
			}
		case NodeTypeDecision:
			if node.Performer == nil {
				if !diagnostics.Add(diagError("missing_performer", path+".performer", "decision node requires a decider performer")) {
					return false
				}
			} else {
				if !validatePerformer(*node.Performer, path+".performer", true, diagnostics) {
					return false
				}
			}
			if len(node.Next) == 0 {
				if !diagnostics.Add(diagError("missing_next", path+".next", "decision node requires outcome edges")) {
					return false
				}
			}
		case NodeTypeWait:
			if node.Wait == nil || (isBlank(node.Wait.Duration) && isBlank(node.Wait.Until) && isBlank(node.Wait.Signal)) {
				if !diagnostics.Add(diagError("missing_wait", path+".wait", "wait node requires duration, until, or signal")) {
					return false
				}
			}
			if node.Wait != nil {
				for _, produced := range []Diagnostics{
					checkInertParamRef(path+".wait.duration", node.Wait.Duration),
					validateDuration(path+".wait.duration", node.Wait.Duration),
					checkInertParamRef(path+".wait.until", node.Wait.Until),
					validateUntil(path+".wait.until", node.Wait.Until),
					checkInertParamRef(path+".wait.signal", node.Wait.Signal),
				} {
					if !diagnostics.AddAll(produced) {
						return false
					}
				}
			}
			if len(node.Next) == 0 {
				if !diagnostics.Add(diagError("missing_next", path+".next", "wait node requires a next target")) {
					return false
				}
			}
		case NodeTypeStart:
			startNodeCount++
			if len(node.Next) == 0 {
				if !diagnostics.Add(diagError("missing_next", path+".next", "start node requires a next target")) {
					return false
				}
			}
		case NodeTypeEnd:
			if len(node.Next) > 0 {
				if !diagnostics.Add(diagError("end_has_next", path+".next", "end node must not have outgoing edges")) {
					return false
				}
			}
			if !diagnostics.AddAll(validateEndResult(node.Result, path+".result")) {
				return false
			}
		case NodeTypeParallel:
			if len(node.Next) < 2 {
				if !diagnostics.Add(diagError("parallel_degree", path+".next", "parallel node requires at least two outgoing edges")) {
					return false
				}
			}
			if node.Performer != nil || node.Plan != nil || len(node.Checks) > 0 || node.Review != nil || node.Retry != nil || node.Wait != nil || !isBlank(node.Result) || len(node.Captures) > 0 {
				if !diagnostics.Add(diagError("parallel_fields", path, "parallel node cannot declare performer, plan, checks, review, retry, wait, result, or captures")) {
					return false
				}
			}
		default:
			if !diagnostics.Add(diagError("invalid_node_type", path+".type", fmt.Sprintf("unsupported node type %q", node.Type))) {
				return false
			}
		}
	}
	if startNodeCount > 1 {
		if !diagnostics.Add(diagError("multiple_start_nodes", "nodes", "at most one node may have type start")) {
			return false
		}
	}
	return true
}

// validateCaptures keeps capture names task-scoped, id-shaped, and unique so
// they can graduate into upstream-capture references without a breaking
// change when the runtime plumbing lands.
func validateCaptures(node Node, path string, diagnostics *templateDiagnosticCollector) bool {
	if len(node.Captures) == 0 {
		return true
	}
	if node.Type != NodeTypeTask {
		if !diagnostics.Add(diagError("captures_on_non_task_node", path+".captures", "captures are only valid on task nodes")) {
			return false
		}
	}
	seen := map[string]int{}
	for i, name := range node.Captures {
		capturePath := fmt.Sprintf("%s.captures[%d]", path, i)
		if !idPattern.MatchString(name) {
			if !diagnostics.Add(diagError("invalid_id", capturePath, "capture name must match "+idPattern.String())) {
				return false
			}
			continue
		}
		if first, ok := seen[name]; ok {
			if !diagnostics.Add(diagError("duplicate_capture", capturePath, fmt.Sprintf("capture name %q is already used by captures[%d]", name, first))) {
				return false
			}
			continue
		}
		seen[name] = i
	}
	return true
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

func validateStep(step Step, path string, allowApproval bool, diagnostics *templateDiagnosticCollector) bool {
	if strings.TrimSpace(step.ID) == "" {
		if !diagnostics.Add(diagError("missing_step_id", path+".id", "step id is required")) {
			return false
		}
	} else if !idPattern.MatchString(step.ID) {
		if !diagnostics.Add(diagError("invalid_id", path+".id", "step id must match "+idPattern.String())) {
			return false
		}
	}
	switch {
	case step.Approval == "":
	case !allowApproval:
		if !diagnostics.Add(diagError("approval_on_non_plan_step", path+".approval", "approval is only valid on plan steps")) {
			return false
		}
	case step.Approval != PlanApprovalHuman && step.Approval != PlanApprovalAuto:
		if !diagnostics.Add(diagError("invalid_plan_approval", path+".approval", fmt.Sprintf("plan approval must be %s or %s; got %q", PlanApprovalHuman, PlanApprovalAuto, step.Approval))) {
			return false
		}
	}
	switch {
	case step.ApprovalRetry == nil:
	case !allowApproval:
		if !diagnostics.Add(diagError("approval_retry_on_non_plan_step", path+".approvalRetry", "approvalRetry is only valid on plan steps")) {
			return false
		}
	case step.Approval != PlanApprovalHuman:
		if !diagnostics.Add(diagError("approval_retry_without_human_approval", path+".approvalRetry", "approvalRetry requires approval: human")) {
			return false
		}
	}
	return validatePerformer(step.Performer, path+".performer", false, diagnostics) &&
		diagnostics.AddAll(validateRetry(step.ApprovalRetry, path+".approvalRetry")) &&
		diagnostics.AddAll(validateRetry(step.Retry, path+".retry"))
}

// validateExpansionCollisions rejects child-stage id collisions across the
// whole template: node ids may contain dots, so an authored node id can
// collide with a derived child id (for example "implement.do"), and two
// different compound nodes can derive the same child id (for example node "a"
// with check id "do" and node "a.test" both derive "a.test.do"). Either kind
// of collision would wedge the run at expansion time.
func validateExpansionCollisions(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	derivedBy := map[string]string{}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		if !node.IsCompound() {
			continue
		}
		visitChildID := func(childID string) bool {
			if owner, ok := derivedBy[childID]; ok {
				return diagnostics.Add(diagError(
					"node_id_collides_with_expansion",
					"nodes."+nodeID,
					fmt.Sprintf("compound nodes %q and %q both derive child stage %q", owner, nodeID, childID),
				))
			}
			derivedBy[childID] = nodeID
			if _, ok := tmpl.Nodes[childID]; ok {
				return diagnostics.Add(diagError(
					"node_id_collides_with_expansion",
					"nodes."+childID,
					fmt.Sprintf("node id %q collides with a child stage of compound node %q", childID, nodeID),
				))
			}
			return true
		}
		if node.Plan != nil {
			if !visitChildID(stageChildID(nodeID, StagePlan, "")) {
				return false
			}
			if node.Plan.Approval == PlanApprovalHuman {
				if !visitChildID(stageChildID(nodeID, StagePlanApproval, "")) {
					return false
				}
			}
		}
		if !visitChildID(stageChildID(nodeID, StageDo, "")) {
			return false
		}
		for _, check := range node.Checks {
			if !visitChildID(stageChildID(nodeID, StageTest, check.ID)) {
				return false
			}
		}
		if node.Review != nil {
			if !visitChildID(stageChildID(nodeID, StageReview, "")) {
				return false
			}
		}
		if !visitChildID(stageChildID(nodeID, StageDone, "")) {
			return false
		}
	}
	return true
}

func validatePerformer(performer Performer, path string, decision bool, diagnostics *templateDiagnosticCollector) bool {
	var finding *Diagnostic
	switch performer.Kind {
	case PerformerAgent:
		if isBlank(performer.Prompt) {
			diagnostic := diagError("missing_prompt", path+".prompt", "agent performer requires prompt")
			finding = &diagnostic
		}
	case PerformerHuman:
		if isBlank(performer.Ask) && isBlank(performer.Prompt) {
			diagnostic := diagError("missing_prompt", path+".ask", "human performer requires ask or prompt")
			finding = &diagnostic
		}
	case PerformerProgram:
		if isBlank(performer.Run) {
			diagnostic := diagError("missing_run", path+".run", "program performer requires run")
			finding = &diagnostic
		}
	default:
		diagnostic := diagError("invalid_performer_kind", path+".kind", fmt.Sprintf("unsupported performer kind %q", performer.Kind))
		finding = &diagnostic
	}
	if finding != nil && !diagnostics.Add(*finding) {
		return false
	}
	if !validateKindScopedFields(performer, path, diagnostics) ||
		!validateChoiceOutcomes(performer, path, decision, diagnostics) {
		return false
	}
	for _, produced := range []Diagnostics{
		checkInertParamRef(path+".profile", performer.Profile),
		checkInertParamRef(path+".timeout", performer.Timeout),
		validateDuration(path+".timeout", performer.Timeout),
	} {
		if !diagnostics.AddAll(produced) {
			return false
		}
	}
	if performer.Contact != nil {
		if !diagnostics.AddAll(validateDuration(path+".contact.cadence", performer.Contact.Cadence)) {
			return false
		}
		if strings.TrimSpace(performer.Contact.Cadence) == "" {
			if !diagnostics.Add(diagError("missing_contact_cadence", path+".contact.cadence", "contact schedule requires cadence")) {
				return false
			}
		}
		if performer.Contact.Budget <= 0 {
			if !diagnostics.Add(diagError("invalid_contact_budget", path+".contact.budget", "contact schedule requires budget greater than zero")) {
				return false
			}
		}
		if strings.TrimSpace(performer.Contact.EscalationTarget) == "" {
			if !diagnostics.Add(diagError("missing_escalation_target", path+".contact.escalationTarget", "contact schedule requires an escalation target")) {
				return false
			}
		}
	}
	return true
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
func validateKindScopedFields(performer Performer, path string, diagnostics *templateDiagnosticCollector) bool {
	if performer.Kind != PerformerHuman && performer.Kind != PerformerAgent && performer.Kind != PerformerProgram {
		return true
	}
	scoped := func(field string, set bool, kinds ...PerformerKind) bool {
		if !set || slices.Contains(kinds, performer.Kind) {
			return true
		}
		kindNames := make([]string, len(kinds))
		for i, kind := range kinds {
			kindNames[i] = string(kind)
		}
		return diagnostics.Add(diagError("kind_scoped_field", path+"."+field,
			fmt.Sprintf("%s is only valid on %s performers", field, strings.Join(kindNames, " or "))))
	}
	if !scoped("ask", !isBlank(performer.Ask), PerformerHuman) ||
		!scoped("choices", len(performer.Choices) > 0, PerformerHuman) ||
		!scoped("choiceOutcomes", len(performer.ChoiceOutcomes) > 0, PerformerHuman) ||
		!scoped("assignee", !isBlank(performer.Assignee), PerformerHuman) ||
		!scoped("prompt", !isBlank(performer.Prompt), PerformerAgent, PerformerHuman) ||
		!scoped("model", !isBlank(performer.Model), PerformerAgent) ||
		!scoped("effort", !isBlank(performer.Effort), PerformerAgent) ||
		!scoped("run", !isBlank(performer.Run), PerformerProgram) ||
		!scoped("args", len(performer.Args) > 0, PerformerProgram) {
		return false
	}
	for i, choice := range performer.Choices {
		if isBlank(choice) {
			if !diagnostics.Add(diagError("invalid_choice", fmt.Sprintf("%s.choices[%d]", path, i), "choices must not be blank")) {
				return false
			}
		}
	}
	for _, produced := range []Diagnostics{
		checkInertParamRef(path+".assignee", performer.Assignee),
		checkInertParamRef(path+".model", performer.Model),
		checkInertParamRef(path+".effort", performer.Effort),
	} {
		if !diagnostics.AddAll(produced) {
			return false
		}
	}
	for i, choice := range performer.Choices {
		if !diagnostics.AddAll(checkInertParamRef(fmt.Sprintf("%s.choices[%d]", path, i), choice)) {
			return false
		}
	}
	return true
}

// ValidateChoiceRouting defensively validates the persisted performer shape at
// dispatch/reconcile time. Authoring normally catches these diagnostics first,
// but old or manually edited run records must fail loudly rather than expose
// actions that cannot settle an attempt.
func ValidateChoiceRouting(performer Performer, decision bool) error {
	collector := newTemplateDiagnosticCollector(nil)
	validateChoiceOutcomes(performer, "performer", decision, collector)
	diagnostics := collector.Diagnostics()
	if len(diagnostics) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", diagnostics[0].Path, diagnostics[0].Message)
}

func validateChoiceOutcomes(performer Performer, path string, decision bool, diagnostics *templateDiagnosticCollector) bool {
	if performer.Kind != PerformerHuman {
		return true
	}
	if decision {
		if len(performer.ChoiceOutcomes) > 0 {
			return diagnostics.Add(diagError("choice_outcomes_on_decision", path+".choiceOutcomes",
				"decision performer choices route through outcome edges; choiceOutcomes is not applicable"))
		}
		return true
	}
	labels := make([]string, 0, len(performer.Choices))
	canonical := make(map[string]struct{}, len(performer.Choices))
	for i, raw := range performer.Choices {
		label := strings.TrimSpace(raw)
		choicePath := fmt.Sprintf("%s.choices[%d]", path, i)
		if label == "" {
			continue // invalid_choice is emitted by validateKindScopedFields.
		}
		if raw != label {
			if !diagnostics.Add(diagError("noncanonical_choice", choicePath,
				"choice labels must not have leading or trailing whitespace")) {
				return false
			}
		}
		for first, existing := range labels {
			if strings.EqualFold(existing, label) {
				if !diagnostics.Add(diagError("duplicate_choice", choicePath,
					fmt.Sprintf("choice %q conflicts with choices[%d] under case-insensitive matching", label, first))) {
					return false
				}
				break
			}
		}
		labels = append(labels, label)
		canonical[label] = struct{}{}
		outcome, ok := performer.ChoiceOutcomes[label]
		if !ok {
			if !diagnostics.Add(diagError("missing_choice_outcome", path+".choiceOutcomes."+label,
				fmt.Sprintf("choice %q requires an explicit pass or fail outcome", label))) {
				return false
			}
			continue
		}
		switch strings.TrimSpace(outcome) {
		case "pass", "fail":
		default:
			if !diagnostics.Add(diagError("invalid_choice_outcome", path+".choiceOutcomes."+label,
				fmt.Sprintf("choice outcome must be pass or fail; got %q", outcome))) {
				return false
			}
		}
	}
	for _, key := range sortedKeys(performer.ChoiceOutcomes) {
		if _, ok := canonical[key]; !ok {
			if !diagnostics.Add(diagError("extra_choice_outcome", path+".choiceOutcomes."+key,
				fmt.Sprintf("choice outcome key %q does not exactly match an authored choice label", key))) {
				return false
			}
		}
	}
	return true
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
func validatePoisonEscalations(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
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
			if !diagnostics.Add(diagError("invalid_poison_escalation", path, "poison escalation requires exactly retry and cancel choices")) {
				return false
			}
		}
		if retryTarget, ok := decision.Next["retry"]; !ok || retryTarget != sourceID {
			if !diagnostics.Add(diagError("invalid_poison_escalation", path+".retry", fmt.Sprintf("poison escalation retry must target compound node %q", sourceID))) {
				return false
			}
		}
		cancelTarget, ok := decision.Next["cancel"]
		cancelNode, targetOK := tmpl.Nodes[cancelTarget]
		if !ok || !targetOK || cancelNode.Type != NodeTypeEnd || !IsCanceledResult(cancelNode.Result) {
			if !diagnostics.Add(diagError("invalid_poison_escalation", path+".cancel", "poison escalation cancel must target an end node with result canceled")) {
				return false
			}
		}
		if tmpl.Start == decisionID {
			if !diagnostics.Add(diagError("invalid_poison_escalation", "start", fmt.Sprintf("poison escalation decision %q cannot also be the template start", decisionID))) {
				return false
			}
		}
		for _, incomingID := range sortedKeys(tmpl.Nodes) {
			incoming := tmpl.Nodes[incomingID].Next
			for _, outcome := range sortedKeys(incoming) {
				target := incoming[outcome]
				if target != decisionID || incomingID == sourceID && IsFailOutcomeLabel(outcome) {
					continue
				}
				if !diagnostics.Add(diagError(
					"invalid_poison_escalation",
					"nodes."+incomingID+".next."+outcome,
					fmt.Sprintf("poison escalation decision %q may only be entered by compound node %q's fail edge", decisionID, sourceID),
				)) {
					return false
				}
			}
		}
	}
	return true
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

func validateParamRefs(tmpl *Template, diagnostics *templateDiagnosticCollector) {
	declared := map[string]bool{}
	for name := range tmpl.Params {
		declared[name] = true
	}

	if !collectProseParamRefs(diagnostics, declared, "name", tmpl.Name) ||
		!collectProseParamRefs(diagnostics, declared, "description", tmpl.Description) ||
		!collectProseParamRefs(diagnostics, declared, "doc", tmpl.Doc) {
		return
	}
	for _, paramID := range sortedKeys(tmpl.Params) {
		param := tmpl.Params[paramID]
		path := "params." + paramID
		if !collectProseParamRefs(diagnostics, declared, path+".name", param.Name) ||
			!collectProseParamRefs(diagnostics, declared, path+".description", param.Description) ||
			!collectProseParamRefs(diagnostics, declared, path+".doc", param.Doc) {
			return
		}
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		if !collectProseParamRefs(diagnostics, declared, path+".name", node.Name) ||
			!collectProseParamRefs(diagnostics, declared, path+".description", node.Description) ||
			!collectProseParamRefs(diagnostics, declared, path+".doc", node.Doc) {
			return
		}
		if node.Performer != nil {
			if !collectPerformerParamRefs(diagnostics, declared, path+".performer", *node.Performer) {
				return
			}
		}
		if node.Plan != nil {
			if !collectStepParamRefs(diagnostics, declared, path+".plan", *node.Plan) {
				return
			}
		}
		for i, check := range node.Checks {
			if !collectStepParamRefs(diagnostics, declared, fmt.Sprintf("%s.checks[%d]", path, i), check) {
				return
			}
		}
		if node.Review != nil {
			if !collectStepParamRefs(diagnostics, declared, path+".review", *node.Review) {
				return
			}
		}
	}
}

func collectStepParamRefs(diagnostics *templateDiagnosticCollector, declared map[string]bool, path string, step Step) bool {
	return collectProseParamRefs(diagnostics, declared, path+".name", step.Name) &&
		collectProseParamRefs(diagnostics, declared, path+".description", step.Description) &&
		collectProseParamRefs(diagnostics, declared, path+".doc", step.Doc) &&
		collectPerformerParamRefs(diagnostics, declared, path+".performer", step.Performer)
}

func collectPerformerParamRefs(diagnostics *templateDiagnosticCollector, declared map[string]bool, path string, performer Performer) bool {
	if !collectExecutableParamRefs(diagnostics, declared, path+".prompt", performer.Prompt) ||
		!collectExecutableParamRefs(diagnostics, declared, path+".ask", performer.Ask) ||
		!collectExecutableParamRefs(diagnostics, declared, path+".run", performer.Run) {
		return false
	}
	for i, arg := range performer.Args {
		if !collectExecutableParamRefs(diagnostics, declared, fmt.Sprintf("%s.args[%d]", path, i), arg) {
			return false
		}
	}
	return true
}

func collectExecutableParamRefs(diagnostics *templateDiagnosticCollector, declared map[string]bool, path, value string) bool {
	return collectParamRefs(diagnostics, declared, path, value, SeverityError)
}

func collectProseParamRefs(diagnostics *templateDiagnosticCollector, declared map[string]bool, path, value string) bool {
	return collectParamRefs(diagnostics, declared, path, value, SeverityWarning)
}

func collectParamRefs(diagnostics *templateDiagnosticCollector, declared map[string]bool, path, value string, severity Severity) bool {
	for offset := 0; offset < len(value); {
		match := paramRefPattern.FindStringSubmatchIndex(value[offset:])
		if match == nil {
			return true
		}
		name := value[offset+match[2] : offset+match[3]]
		offset += match[1]
		if declared[name] {
			continue
		}
		message := fmt.Sprintf("reference to undeclared param %q", name)
		var diagnostic Diagnostic
		if severity == SeverityWarning {
			diagnostic = diagWarning("undeclared_param_ref", path, message)
		} else {
			diagnostic = diagError("undeclared_param_ref", path, message)
		}
		if !diagnostics.Add(diagnostic) {
			return false
		}
	}
	return true
}

func validateLayout(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	if tmpl.Layout == nil {
		return true
	}
	for _, nodeID := range sortedKeys(tmpl.Layout.Nodes) {
		if _, ok := tmpl.Nodes[nodeID]; !ok {
			if !diagnostics.Add(diagWarning("stale_layout_node", "layout.nodes."+nodeID, fmt.Sprintf("layout references undeclared node %q", nodeID))) {
				return false
			}
		}
	}
	// Per-connector layout entries outlive their edges silently otherwise: the
	// editor keys them by (from, outcome) and reuses outcome names as soon as
	// they are free, so an orphan is not merely dead weight -- it is an opinion
	// waiting to be inherited by an unrelated future connector.
	for _, from := range sortedKeys(tmpl.Layout.Edges) {
		node, ok := tmpl.Nodes[from]
		if !ok {
			if !diagnostics.Add(diagWarning("stale_layout_edge", "layout.edges."+from, fmt.Sprintf("layout references undeclared node %q", from))) {
				return false
			}
			continue
		}
		for _, outcome := range sortedKeys(tmpl.Layout.Edges[from]) {
			if _, ok := node.Next[outcome]; !ok {
				if !diagnostics.Add(diagWarning("stale_layout_edge", "layout.edges."+from+"."+outcome, fmt.Sprintf("layout references undeclared outcome %q on node %q", outcome, from))) {
					return false
				}
			}
		}
	}
	return true
}
