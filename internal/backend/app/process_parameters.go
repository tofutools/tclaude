package app

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const processParameterSyntax = "mustache-v1"
const maxExpandedProcessText = 128 << 10
const maxExpandedProcessTotal = 1 << 20

var processParameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var processParameterCandidate = regexp.MustCompile(`\{\{\s*params\b`)

var processParameterReference = regexp.MustCompile(`\{\{\s*params\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

func validateProcessParameterSyntax(syntax string, graph model.WorkGraph, declarations []model.ParameterDeclaration) error {
	if syntax == "" {
		return nil
	}
	if syntax != processParameterSyntax {
		return fail(ErrInvalid, "unsupported process parameter syntax")
	}
	declared := make(map[string]bool, len(declarations))
	for _, d := range declarations {
		if !processParameterName.MatchString(d.Name) {
			return fail(ErrInvalid, "mustache-v1 parameter keys must be ASCII identifiers")
		}
		declared[d.Name] = true
	}
	_, err := mapProcessInputText(graph, func(value string) (string, error) {
		if len(value) > maxExpandedProcessText || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return "", fail(ErrInvalid, "process input text exceeds valid text limits")
		}
		remaining := processParameterReference.ReplaceAllString(value, "")
		if processParameterCandidate.MatchString(remaining) {
			return "", fail(ErrInvalid, "malformed mustache-v1 parameter reference")
		}
		for _, match := range processParameterReference.FindAllStringSubmatch(value, -1) {
			if !declared[match[1]] {
				return "", fail(ErrInvalid, "process input references undeclared parameter %s", match[1])
			}
		}
		return value, nil
	})
	return err
}

func expandProcessParameters(graph model.WorkGraph, declarations []model.ParameterDeclaration, parameters map[string]json.RawMessage) (model.WorkGraph, error) {
	values := make(map[string]string, len(declarations))
	for _, d := range declarations {
		raw, exists := parameters[d.Name]
		if !exists {
			values[d.Name] = ""
			continue
		}
		if len(raw) > maxExpandedProcessTotal || !utf8.Valid(raw) {
			return model.WorkGraph{}, fail(ErrInvalid, "parameter value exceeds text limits")
		}
		if d.Type == model.ParameterString {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return model.WorkGraph{}, fail(ErrInvalid, "invalid parameter string")
			}
			values[d.Name] = value
		} else {
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				return model.WorkGraph{}, fail(ErrInvalid, "invalid parameter JSON")
			}
			values[d.Name] = compact.String()
		}
	}
	total := 0
	resolved, err := mapProcessInputText(graph, func(value string) (string, error) {
		var out strings.Builder
		appendText := func(text string) error {
			if len(text) > maxExpandedProcessText-out.Len() || len(text) > maxExpandedProcessTotal-total {
				return fail(ErrInvalid, "expanded process input exceeds text limits")
			}
			out.WriteString(text)
			total += len(text)
			return nil
		}
		previous := 0
		for _, match := range processParameterReference.FindAllStringSubmatchIndex(value, -1) {
			if err := appendText(value[previous:match[0]]); err != nil {
				return "", err
			}
			replacement, exists := values[value[match[2]:match[3]]]
			if !exists {
				return "", fail(ErrInvalid, "process input references undeclared parameter")
			}
			if err := appendText(replacement); err != nil {
				return "", err
			}
			previous = match[1]
		}
		if err := appendText(value[previous:]); err != nil {
			return "", err
		}
		result := out.String()
		if !utf8.ValidString(result) || strings.ContainsRune(result, 0) {
			return "", fail(ErrInvalid, "expanded process input requires valid text")
		}
		return result, nil
	})
	if err != nil {
		return model.WorkGraph{}, err
	}
	for i := range resolved.Nodes {
		if decision := resolved.Nodes[i].Decision; decision != nil {
			// Resolve absence from the authored graph, never the expanded text.
			decision.QuestionResolved = true
			if graph.Nodes[i].Decision.Question == "" {
				decision.Question = graph.Nodes[i].Name
			}
		}
	}
	return resolved, nil
}

// Clone only the input-bearing structures being changed. Configuration, authority,
// route labels and arbitrary JSON are deliberately outside this text surface.
func mapProcessInputText(graph model.WorkGraph, transform func(string) (string, error)) (model.WorkGraph, error) {
	graph.Nodes = append([]model.WorkNode(nil), graph.Nodes...)
	for i := range graph.Nodes {
		node := &graph.Nodes[i]
		if node.Decision != nil {
			copy := *node.Decision
			var err error
			copy.Question, err = transform(copy.Question)
			if err != nil {
				return model.WorkGraph{}, err
			}
			if copy.Decider != nil {
				mapped, err := mapProcessInputText(model.WorkGraph{Nodes: []model.WorkNode{{Performer: copy.Decider}}}, transform)
				if err != nil {
					return model.WorkGraph{}, err
				}
				copy.Decider = mapped.Nodes[0].Performer
			}
			node.Decision = &copy
		}
		if node.Performer == nil {
			continue
		}
		performer := *node.Performer
		node.Performer = &performer
		if performer.Agent != nil {
			copy := *performer.Agent
			var err error
			copy.Brief, err = transform(copy.Brief)
			if err != nil {
				return model.WorkGraph{}, err
			}
			performer.Agent = &copy
		}
		if performer.Human != nil {
			copy := *performer.Human
			var err error
			copy.Ask, err = transform(copy.Ask)
			if err != nil {
				return model.WorkGraph{}, err
			}
			copy.Prompt, err = transform(copy.Prompt)
			if err != nil {
				return model.WorkGraph{}, err
			}
			performer.Human = &copy
		}
		if performer.Program != nil {
			copy := *performer.Program
			copy.Arguments = append([]string(nil), copy.Arguments...)
			for j := range copy.Arguments {
				var err error
				copy.Arguments[j], err = transform(copy.Arguments[j])
				if err != nil {
					return model.WorkGraph{}, err
				}
			}
			performer.Program = &copy
		}
	}
	return graph, nil
}

// A missing RawMessage is encoded as JSON null in retained revisions. Neither
// representation is a typed default for these non-nullable parameters.
func hasParameterDefault(raw json.RawMessage) bool {
	return len(raw) != 0 && string(bytes.TrimSpace(raw)) != "null"
}
