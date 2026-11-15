package model

import (
	"fmt"
	"regexp"
	"strings"
)

// captureRefPattern is a reference to a step output in a prompt or ask:
// {{ outputs.name }}, with name a capture some step of the scope declares.
// The outputs. prefix keeps it apart from a parameter ({{ params.x }}) and from
// the editors' bare {{x}} shorthand, which means {{ params.x }}.
var captureRefPattern = regexp.MustCompile(`\{\{\s*outputs\.([a-z0-9][a-z0-9._-]*)\s*\}\}`)

// CaptureReferences returns the step outputs one interpolated performer field
// refers to, in source order.
func CaptureReferences(value string) []string {
	var references []string
	for _, match := range captureRefPattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 {
			references = append(references, match[1])
		}
	}
	return references
}

// InterpolateCaptures replaces each {{ outputs.name }} that names a known output with
// value(name). An unknown name is left as written.
func InterpolateCaptures(text string, value func(name string) (string, bool)) string {
	return captureRefPattern.ReplaceAllStringFunc(text, func(reference string) string {
		match := captureRefPattern.FindStringSubmatch(reference)
		if len(match) != 2 {
			return reference
		}
		if replaced, ok := value(match[1]); ok {
			return replaced
		}
		return reference
	})
}

// agentPerformed reports whether an agent may perform node: a task or
// decision whose performer (or a compound task's do stage) is an agent.
func agentPerformed(node Node) bool {
	return node.Performer != nil && node.Performer.Kind == PerformerAgent &&
		(node.Type == NodeTypeTask || node.Type == NodeTypeDecision)
}

// performerSlots lists every performer a node declares, with its path: the
// node's own, and a compound task's stage performers.
func performerSlots(nodeID string, node Node) []struct {
	path      string
	performer Performer
} {
	path := "nodes." + nodeID
	var out []struct {
		path      string
		performer Performer
	}
	add := func(p string, performer *Performer) {
		if performer != nil {
			out = append(out, struct {
				path      string
				performer Performer
			}{p, *performer})
		}
	}
	add(path+".performer", node.Performer)
	if node.Plan != nil {
		add(path+".plan.performer", &node.Plan.Performer)
	}
	for i := range node.Checks {
		add(fmt.Sprintf("%s.checks[%d].performer", path, i), &node.Checks[i].Performer)
	}
	if node.Review != nil {
		add(path+".review.performer", &node.Review.Performer)
	}
	return out
}

// validatePerformerConstraints checks sameAs and differentFrom: each names
// another node of the same scope that an agent performs, and a node is not
// both the same as and different from another.
func validatePerformerConstraints(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		for _, slot := range performerSlots(nodeID, tmpl.Nodes[nodeID]) {
			performer := slot.performer
			if performer.Kind != PerformerAgent {
				continue
			}
			check := func(field, ref string) bool {
				target, ok := tmpl.Nodes[ref]
				switch {
				case ref == nodeID:
					return diagnostics.Add(diagError("invalid_performer_constraint", slot.path+"."+field,
						fmt.Sprintf("%s names the step itself", field)))
				case !ok:
					return diagnostics.Add(diagError("invalid_performer_constraint", slot.path+"."+field,
						fmt.Sprintf("%s names %q, which is no step of this process", field, ref)))
				case !agentPerformed(target):
					return diagnostics.Add(diagError("invalid_performer_constraint", slot.path+"."+field,
						fmt.Sprintf("%s names %q, which no agent performs", field, ref)))
				}
				return true
			}
			if same := strings.TrimSpace(performer.SameAs); same != "" && !check("sameAs", same) {
				return false
			}
			seen := map[string]bool{}
			for i, ref := range performer.DifferentFrom {
				ref = strings.TrimSpace(ref)
				field := fmt.Sprintf("differentFrom[%d]", i)
				if seen[ref] {
					if !diagnostics.Add(diagError("invalid_performer_constraint", slot.path+"."+field,
						fmt.Sprintf("differentFrom names %q twice", ref))) {
						return false
					}
					continue
				}
				seen[ref] = true
				if !check(field, ref) {
					return false
				}
				if ref == strings.TrimSpace(performer.SameAs) {
					if !diagnostics.Add(diagError("conflicting_performer_constraint", slot.path+"."+field,
						fmt.Sprintf("the step cannot be both the same agent as and a different agent from %q", ref))) {
						return false
					}
				}
			}
		}
	}
	return true
}

// validateCaptureRefs checks the outputs prompts and asks refer to: an output
// is declared by one step only, and a {{ outputs.name }} that names no declared output
// is a warning (it stays as written at run time).
func validateCaptureRefs(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	owners := map[string]string{}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		for i, name := range tmpl.Nodes[nodeID].Captures {
			if first, taken := owners[name]; taken && first != nodeID {
				if !diagnostics.Add(diagError("duplicate_capture", fmt.Sprintf("nodes.%s.captures[%d]", nodeID, i),
					fmt.Sprintf("output %q is already declared by step %q", name, first))) {
					return false
				}
				continue
			}
			owners[name] = nodeID
		}
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		for _, slot := range performerSlots(nodeID, tmpl.Nodes[nodeID]) {
			for _, field := range []struct{ name, value string }{{"prompt", slot.performer.Prompt}, {"ask", slot.performer.Ask}} {
				for _, ref := range CaptureReferences(field.value) {
					// A step may read its own output: in a loop, that is the
					// previous round's value.
					if _, ok := owners[ref]; !ok {
						if !diagnostics.Add(diagWarning("unknown_capture_ref", slot.path+"."+field.name,
							fmt.Sprintf("{{ outputs.%s }} names no step output; it is left as written", ref))) {
							return false
						}
					}
				}
			}
		}
	}
	return true
}
