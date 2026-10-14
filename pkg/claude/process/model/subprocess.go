package model

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Subprocess is the nested graph of a subprocess (▣) node. The body is either
// INLINE — its own start, nodes, and params declared right here — or a
// TEMPLATE REFERENCE to another stored template, optionally pinned to one
// semantic version. Exactly one of the two is authored.
//
// Either way the body is its own parameter scope: inner nodes see only the
// body's declared params, and Inputs (the in-ports) binds each of them from a
// literal or from the PARENT's params. Outputs (the out-ports) maps each inner
// end node to one outcome of the parent node's next map, which is how the
// nested run leaves the subprocess.
type Subprocess struct {
	TemplateRef string `json:"templateRef,omitempty" yaml:"templateRef,omitempty"`
	// Version pins TemplateRef to one semantic version, spelled the way a
	// template ref spells it after the "@" (sha256:<hex>). Omitted means the
	// template's head at run creation, which the run then pins by embedding it
	// as Resolved in its snapshot.
	Version string           `json:"version,omitempty" yaml:"version,omitempty"`
	Params  map[string]Param `json:"params,omitempty" yaml:"params,omitempty"`
	Start   string           `json:"start,omitempty" yaml:"start,omitempty"`
	Nodes   map[string]Node  `json:"nodes,omitempty" yaml:"nodes,omitempty"`
	// Inputs binds body params (key) to a literal or {{ params.x }} expression
	// over the parent scope's params (value).
	Inputs map[string]string `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	// Outputs maps an inner end node id (key) to a parent next outcome (value).
	// When the subprocess node has exactly one next outcome, unmapped end nodes
	// route through it.
	Outputs map[string]string `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	// Resolved is the referenced template, embedded by ResolveSubprocessRefs
	// when a run is created so the run snapshot is self-contained and a cold
	// load needs no template store. It is runtime-only: YAML never carries it,
	// so authoring source cannot set it.
	Resolved *Template `json:"resolved,omitempty" yaml:"-"`
}

const (
	// MaxSubprocessDepth bounds subprocess nesting, inline and by reference
	// combined. Nesting is a readability tool, not a recursion mechanism.
	MaxSubprocessDepth = 4

	// SubprocessNodeIDSeparator joins a subprocess instance's node id and an
	// inner node id into the namespaced id the run state uses ("review/lint").
	// Authored ids cannot contain it, so a namespaced id never collides with an
	// authored or compound-stage id.
	SubprocessNodeIDSeparator = "/"
)

var templateVersionPattern = regexp.MustCompile(`^` + HashAlgorithm + `:[0-9a-f]{64}$`)

// SubprocessChildID returns the namespaced run-state id of one inner node.
func SubprocessChildID(subprocessNodeID, innerNodeID string) string {
	return subprocessNodeID + SubprocessNodeIDSeparator + innerNodeID
}

// IsInline reports whether the body is authored inline.
func (s *Subprocess) IsInline() bool {
	return s != nil && (s.Start != "" || len(s.Nodes) > 0 || len(s.Params) > 0)
}

// Body returns the subprocess body as a template-shaped scope: the inline body,
// or the resolved referenced template. It reports false for an unresolved
// reference.
func (s *Subprocess) Body() (*Template, bool) {
	if s == nil {
		return nil, false
	}
	if s.IsInline() {
		return &Template{APIVersion: APIVersion, Kind: Kind, Params: s.Params, Start: s.Start, Nodes: s.Nodes}, true
	}
	if s.Resolved != nil {
		return s.Resolved, true
	}
	return nil, false
}

// OutputOutcome returns the parent outcome an inner end node leaves through.
func (s *Subprocess) OutputOutcome(endNodeID string, parentNext Next) (string, bool) {
	if s == nil {
		return "", false
	}
	if outcome, ok := s.Outputs[endNodeID]; ok {
		return outcome, true
	}
	if len(parentNext) == 1 {
		for outcome := range parentNext {
			return outcome, true
		}
	}
	return "", false
}

// BindInputs computes the body's runtime params from the parent scope's
// params: each bound input is interpolated over the parent params, and an
// unbound param falls back to its declared default. Unbound params without a
// default are left absent, which program binding reports if anything uses one.
func (s *Subprocess) BindInputs(body *Template, parentParams map[string]string) map[string]string {
	params := make(map[string]string)
	if body == nil {
		return params
	}
	for name, param := range body.Params {
		if expression, ok := s.Inputs[name]; ok && paramRefsPresent(expression, parentParams) {
			params[name] = interpolateParams(expression, parentParams)
			continue
		}
		if param.Default != nil {
			params[name] = formatParamDefault(param.Default)
		}
	}
	return params
}

// paramRefsPresent reports whether every {{ params.x }} an input expression
// references exists in params. A binding over a missing parent param must not
// silently become "" — it falls back to the default or stays absent, so
// program binding reports the param as missing.
func paramRefsPresent(expression string, params map[string]string) bool {
	for _, match := range paramRefPattern.FindAllStringSubmatch(expression, -1) {
		if len(match) != 2 {
			continue
		}
		if _, ok := params[match[1]]; !ok {
			return false
		}
	}
	return true
}

// formatParamDefault renders a declared default the same way whether the
// template came from YAML (ints) or a cold JSON reload (float64): a whole
// number must not turn into exponent notation such as "1e+06".
func formatParamDefault(value any) string {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	default:
		return fmt.Sprint(v)
	}
}

// WalkNodes visits every node of a template, descending into subprocess
// bodies (inline or resolved). id is the namespaced run-state id. Unresolved
// references have no body and are not descended into; descent stops past
// MaxSubprocessDepth so hostile input cannot recurse without bound.
func WalkNodes(tmpl *Template, visit func(id string, node Node)) {
	walkNodes(tmpl, "", 0, visit)
}

func walkNodes(tmpl *Template, prefix string, depth int, visit func(id string, node Node)) {
	if tmpl == nil || depth > MaxSubprocessDepth {
		return
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		id := prefix + nodeID
		visit(id, node)
		if node.Type != NodeTypeSubprocess {
			continue
		}
		if body, ok := node.Subprocess.Body(); ok {
			walkNodes(body, id+SubprocessNodeIDSeparator, depth+1, visit)
		}
	}
}

// subprocessGraphCardinality counts the nodes and edges inside every
// subprocess body of a template, recursively, saturating at the graph budget.
func subprocessGraphCardinality(tmpl *Template) NormalizedGraphCardinality {
	var counts NormalizedGraphCardinality
	var walk func(scope *Template, depth int)
	walk = func(scope *Template, depth int) {
		if scope == nil || depth > MaxSubprocessDepth+1 {
			return
		}
		for _, node := range scope.Nodes {
			if counts.Nodes > MaxNormalizedNodes && counts.Edges > MaxNormalizedEdges {
				return
			}
			if node.Type != NodeTypeSubprocess && node.Subprocess == nil {
				continue
			}
			body, ok := node.Subprocess.Body()
			if !ok {
				continue
			}
			counts.Nodes = saturatingAdd(counts.Nodes, len(body.Nodes), MaxNormalizedNodes)
			for _, inner := range body.Nodes {
				counts.Edges = saturatingAdd(counts.Edges, len(inner.Next), MaxNormalizedEdges)
			}
			walk(body, depth+1)
		}
	}
	walk(tmpl, 0)
	return counts
}

// validateSubprocessNode validates one subprocess node's static shape, its
// ports against the body when the body is known, and the body itself as a
// nested scope.
func validateSubprocessNode(scope *Template, nodeID string, node Node, path string, depth int, diagnostics *templateDiagnosticCollector) bool {
	add := func(code, at, message string) bool {
		return diagnostics.Add(diagError(code, at, message))
	}
	if len(node.Next) == 0 {
		if !add("missing_next", path+".next", "subprocess node requires at least one next outcome") {
			return false
		}
	}
	if node.Performer != nil || node.Plan != nil || len(node.Checks) > 0 || node.Review != nil || node.Retry != nil || node.Wait != nil || len(node.Captures) > 0 {
		if !add("subprocess_fields", path, "subprocess node cannot declare performer, plan, checks, review, retry, wait, or captures") {
			return false
		}
	}
	sp := node.Subprocess
	spPath := path + ".subprocess"
	if sp == nil {
		return add("missing_subprocess", spPath, "subprocess node requires a subprocess body: inline start and nodes, or a templateRef")
	}
	inline, ref := sp.IsInline(), sp.TemplateRef != ""
	switch {
	case inline && ref:
		if !add("subprocess_source_conflict", spPath, "subprocess declares both an inline body and a templateRef; author exactly one") {
			return false
		}
	case !inline && !ref:
		if !add("missing_subprocess_source", spPath, "subprocess requires an inline body (start and nodes) or a templateRef") {
			return false
		}
	}
	if ref {
		if !idPattern.MatchString(sp.TemplateRef) {
			if !add("invalid_id", spPath+".templateRef", "templateRef must name a template id matching "+idPattern.String()) {
				return false
			}
		} else if sp.TemplateRef == scope.ID && scope.ID != "" {
			if !add("subprocess_ref_cycle", spPath+".templateRef", fmt.Sprintf("template %q cannot reference itself as a subprocess", sp.TemplateRef)) {
				return false
			}
		}
	}
	if sp.Version != "" {
		if !ref {
			if !add("version_without_template_ref", spPath+".version", "version is only valid together with templateRef") {
				return false
			}
		} else if !templateVersionPattern.MatchString(sp.Version) {
			if !add("invalid_template_version", spPath+".version", "version must be "+HashAlgorithm+":<64 lowercase hex digits>") {
				return false
			}
		}
	}
	if sp.Resolved != nil && (!ref || sp.Resolved.ID != sp.TemplateRef) {
		if !add("invalid_resolved_subprocess", spPath+".resolved", "a resolved subprocess body must be the template its templateRef names") {
			return false
		}
	}
	if len(sp.Outputs) > 0 || len(node.Next) > 0 {
		for _, endID := range sortedKeys(sp.Outputs) {
			outcome := sp.Outputs[endID]
			if _, ok := node.Next[outcome]; !ok {
				if !add("unknown_output_outcome", spPath+".outputs."+endID, fmt.Sprintf("out-port maps to outcome %q, which is not in this node's next", outcome)) {
					return false
				}
			}
		}
	}
	body, known := sp.Body()
	if !known {
		// An unresolved reference: its ports are checked when the run resolves it.
		return true
	}
	if depth+1 > MaxSubprocessDepth {
		return add("subprocess_depth_limit", spPath, fmt.Sprintf("subprocesses nest at most %d levels deep", MaxSubprocessDepth))
	}
	if !validateSubprocessPorts(scope, node, body, spPath, diagnostics) {
		return false
	}
	bodyPath := spPath
	if !inline {
		bodyPath = spPath + ".resolved"
	}
	edges := NormalizeEdges(body)
	validateScope(body, edges, diagnostics.scoped(bodyPath), depth+1, inline)
	return !diagnostics.Exhausted()
}

// validateSubprocessPorts type-checks in-ports against the body's declared
// params and the parent's params, and out-ports against the body's end nodes
// and the parent node's next outcomes.
func validateSubprocessPorts(scope *Template, node Node, body *Template, spPath string, diagnostics *templateDiagnosticCollector) bool {
	sp := node.Subprocess
	add := func(diagnostic Diagnostic) bool { return diagnostics.Add(diagnostic) }
	for _, name := range sortedKeys(sp.Inputs) {
		at := spPath + ".inputs." + name
		bodyParam, declared := body.Params[name]
		if !declared {
			if !add(diagError("unknown_input", at, fmt.Sprintf("in-port %q is not a declared param of the subprocess body", name))) {
				return false
			}
			continue
		}
		expression := sp.Inputs[name]
		for _, reference := range ParamReferences(expression) {
			if _, ok := scope.Params[reference]; !ok {
				if !add(diagError("undeclared_param_ref", at, fmt.Sprintf("reference to undeclared param %q", reference))) {
					return false
				}
			}
		}
		if message, ok := inputTypeMismatch(scope, bodyParam, expression); !ok {
			if !add(diagError("input_type_mismatch", at, message)) {
				return false
			}
		}
	}
	for _, name := range sortedKeys(body.Params) {
		param := body.Params[name]
		if _, bound := sp.Inputs[name]; bound || param.Default != nil || param.Required != nil && !*param.Required {
			continue
		}
		if !add(diagError("unbound_input", spPath+".inputs."+name, fmt.Sprintf("subprocess param %q has no default and needs an in-port binding", name))) {
			return false
		}
	}
	ends := map[string]bool{}
	for innerID, inner := range body.Nodes {
		if inner.Type == NodeTypeEnd {
			ends[innerID] = true
		}
	}
	for _, endID := range sortedKeys(sp.Outputs) {
		if !ends[endID] {
			if !add(diagError("unknown_output", spPath+".outputs."+endID, fmt.Sprintf("out-port %q is not an end node of the subprocess body", endID))) {
				return false
			}
		}
	}
	produced := map[string]bool{}
	for _, endID := range sortedKeys(ends) {
		outcome, ok := sp.OutputOutcome(endID, node.Next)
		if !ok {
			if !add(diagError("unmapped_output", spPath+".outputs."+endID, fmt.Sprintf("end node %q needs an out-port mapping to one of this node's next outcomes", endID))) {
				return false
			}
			continue
		}
		produced[outcome] = true
	}
	for _, outcome := range sortedKeys(node.Next) {
		if !produced[outcome] {
			if !add(diagWarning("dead_edge", strings.TrimSuffix(spPath, ".subprocess")+".next."+outcome, fmt.Sprintf("no out-port of the subprocess leaves through outcome %q", outcome))) {
				return false
			}
		}
	}
	return true
}

// inputTypeMismatch reports a binding whose value cannot have the body param's
// declared type: a bare {{ params.x }} forwarding a differently typed parent
// param, or a literal that does not parse as a number or boolean param.
func inputTypeMismatch(scope *Template, bodyParam Param, expression string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(bodyParam.Type))
	if want == "" {
		return "", true
	}
	trimmed := strings.TrimSpace(expression)
	if match := paramRefPattern.FindStringSubmatchIndex(trimmed); match != nil && match[0] == 0 && match[1] == len(trimmed) {
		source, ok := scope.Params[trimmed[match[2]:match[3]]]
		have := strings.ToLower(strings.TrimSpace(source.Type))
		if !ok || have == "" || have == want {
			return "", true
		}
		return fmt.Sprintf("forwards a %s param into a %s port", have, want), false
	}
	if len(ParamReferences(expression)) > 0 {
		// A composed string can only be checked once interpolated.
		return "", true
	}
	switch want {
	case "number":
		if _, err := strconv.ParseFloat(trimmed, 64); err != nil {
			return fmt.Sprintf("literal %q is not a number", expression), false
		}
	case "integer", "int":
		if _, err := strconv.ParseInt(trimmed, 10, 64); err != nil {
			return fmt.Sprintf("literal %q is not an integer", expression), false
		}
	case "boolean", "bool":
		if _, err := strconv.ParseBool(trimmed); err != nil {
			return fmt.Sprintf("literal %q is not a boolean", expression), false
		}
	}
	return "", true
}

// TemplateResolver loads one stored template for subprocess resolution: the
// exact version when version is set, the head otherwise. It returns the
// template and its semantic version (sha256:<hex>).
type TemplateResolver func(id, version string) (*Template, string, error)

// ResolveSubprocessRefs returns a copy of tmpl in which every subprocess
// templateRef — at any depth — carries its referenced template as Resolved and
// a pinned Version. Run creation calls it so the persisted snapshot is
// self-contained; the engine never reads the template store. Reference cycles
// and nesting beyond MaxSubprocessDepth are rejected here, and each resolved
// body is validated in the context of its subprocess node (ports included).
func ResolveSubprocessRefs(tmpl *Template, resolve TemplateResolver) (*Template, error) {
	if tmpl == nil {
		return nil, fmt.Errorf("nil process template")
	}
	clone := cloneTemplate(tmpl)
	budget := scopeGraphCardinality(&clone)
	if err := resolveScope(&clone, resolve, []string{tmpl.ID}, 0, &budget); err != nil {
		return nil, err
	}
	return &clone, nil
}

// scopeGraphCardinality counts a scope's own nodes and edges plus every body
// already embedded below it.
func scopeGraphCardinality(scope *Template) NormalizedGraphCardinality {
	counts := NormalizedGraphCardinality{Nodes: saturatingCount(len(scope.Nodes), MaxNormalizedNodes)}
	for _, node := range scope.Nodes {
		counts.Edges = saturatingAdd(counts.Edges, len(node.Next), MaxNormalizedEdges)
	}
	nested := subprocessGraphCardinality(scope)
	counts.Nodes = saturatingAdd(counts.Nodes, nested.Nodes, MaxNormalizedNodes)
	counts.Edges = saturatingAdd(counts.Edges, nested.Edges, MaxNormalizedEdges)
	return counts
}

// resolveScope embeds referenced bodies below scope. budget is the running
// node/edge total of the whole resolved tree: references fan out, so without
// it a few small templates referencing each other many times over could
// materialize an arbitrarily large snapshot before anything validated it.
func resolveScope(scope *Template, resolve TemplateResolver, stack []string, depth int, budget *NormalizedGraphCardinality) error {
	for _, nodeID := range sortedKeys(scope.Nodes) {
		node := scope.Nodes[nodeID]
		if node.Type != NodeTypeSubprocess || node.Subprocess == nil {
			continue
		}
		if depth+1 > MaxSubprocessDepth {
			return fmt.Errorf("subprocess %q: subprocesses nest at most %d levels deep", nodeID, MaxSubprocessDepth)
		}
		sp := node.Subprocess
		if sp.IsInline() {
			body, _ := sp.Body()
			if err := resolveScope(body, resolve, stack, depth+1, budget); err != nil {
				return fmt.Errorf("subprocess %q: %w", nodeID, err)
			}
			continue
		}
		if sp.TemplateRef == "" {
			continue
		}
		for _, active := range stack {
			if active == sp.TemplateRef {
				return fmt.Errorf("subprocess %q: template reference cycle %s -> %s", nodeID, strings.Join(stack, " -> "), sp.TemplateRef)
			}
		}
		if resolve == nil {
			return fmt.Errorf("subprocess %q: template %q cannot be resolved", nodeID, sp.TemplateRef)
		}
		referenced, version, err := resolve(sp.TemplateRef, sp.Version)
		if err != nil {
			return fmt.Errorf("subprocess %q: resolve template %q: %w", nodeID, sp.TemplateRef, err)
		}
		if referenced == nil || referenced.ID != sp.TemplateRef {
			return fmt.Errorf("subprocess %q: resolver returned the wrong template for %q", nodeID, sp.TemplateRef)
		}
		if sp.Version != "" && version != sp.Version {
			return fmt.Errorf("subprocess %q: resolver returned version %q, want %q", nodeID, version, sp.Version)
		}
		body := cloneTemplate(referenced)
		body.Layout = nil
		added := scopeGraphCardinality(&body)
		budget.Nodes = saturatingAdd(budget.Nodes, added.Nodes, MaxNormalizedNodes)
		budget.Edges = saturatingAdd(budget.Edges, added.Edges, MaxNormalizedEdges)
		if budget.Nodes > MaxNormalizedNodes || budget.Edges > MaxNormalizedEdges {
			return fmt.Errorf("subprocess %q: resolved process exceeds %d nodes or %d edges", nodeID, MaxNormalizedNodes, MaxNormalizedEdges)
		}
		if err := resolveScope(&body, resolve, append(append([]string(nil), stack...), sp.TemplateRef), depth+1, budget); err != nil {
			return fmt.Errorf("subprocess %q: %w", nodeID, err)
		}
		// The pin is the embedded body itself (its own semantic hash is the
		// version); Version stays as authored so the template's identity — its
		// semantic hash — is unchanged by resolution.
		resolved := *sp
		resolved.Resolved = &body
		node.Subprocess = &resolved
		scope.Nodes[nodeID] = node
	}
	return nil
}

// stripResolvedSubprocesses drops every embedded body from a template the
// caller owns, at any inline depth.
func stripResolvedSubprocesses(tmpl *Template) {
	for id, node := range tmpl.Nodes {
		if node.Subprocess == nil {
			continue
		}
		node.Subprocess.Resolved = nil
		if node.Subprocess.IsInline() {
			body := Template{Nodes: node.Subprocess.Nodes}
			stripResolvedSubprocesses(&body)
		}
		tmpl.Nodes[id] = node
	}
}

func cloneSubprocess(sp *Subprocess) *Subprocess {
	if sp == nil {
		return nil
	}
	clone := *sp
	if sp.Params != nil || sp.Nodes != nil {
		body := cloneTemplate(&Template{Params: sp.Params, Nodes: sp.Nodes})
		if sp.Params != nil {
			clone.Params = body.Params
		}
		if sp.Nodes != nil {
			clone.Nodes = body.Nodes
		}
	}
	if sp.Inputs != nil {
		clone.Inputs = make(map[string]string, len(sp.Inputs))
		for key, value := range sp.Inputs {
			clone.Inputs[key] = value
		}
	}
	if sp.Outputs != nil {
		clone.Outputs = make(map[string]string, len(sp.Outputs))
		for key, value := range sp.Outputs {
			clone.Outputs[key] = value
		}
	}
	if sp.Resolved != nil {
		resolved := cloneTemplate(sp.Resolved)
		clone.Resolved = &resolved
	}
	return &clone
}
