package agentd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// TestReferenceHelperTemplate checks a template a helper agent (the inline
// editor's ✨ With an agent, the process scribe, the steward) wrote for one of
// the operator's reference processes (tk_c324d5a4, point 5). It is a manual,
// LLM-backed check, so it runs only when pointed at the agent's YAML:
//
//	TCLAUDE_REFERENCE_HELPER=/path/to/template.yaml \
//	TCLAUDE_REFERENCE_HELPER_KIND=simple|advanced|validate \
//	  go test ./pkg/claude/agentd -run TestReferenceHelperTemplate -v
//
// "validate" only reports what POST /v1/process/validate would, as a
// stand-in for `tclaude agent process-templates validate` where no daemon
// runs. "simple" and "advanced" also require the shape the operator asked
// for, whatever ids and wording the agent chose.
func TestReferenceHelperTemplate(t *testing.T) {
	path := os.Getenv("TCLAUDE_REFERENCE_HELPER")
	if path == "" {
		t.Skip("set TCLAUDE_REFERENCE_HELPER to a helper-written template")
	}
	kind := os.Getenv("TCLAUDE_REFERENCE_HELPER_KIND")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, err := parseProcessEditView(&processTemplateEditView{Source: string(source)})
	require.NoError(t, err)
	errors := 0
	for _, d := range diagnosticsForEditor(parsed.Diagnostics, parsed.Template) {
		t.Logf("%s %s %s: %s", d.Severity, d.Code, d.TargetID, d.Message)
		if d.Severity == model.SeverityError {
			errors++
		}
	}
	require.Zero(t, errors, "the template has errors")
	params := map[string]string{}
	for name := range parsed.Template.Params {
		params[name] = "x"
	}
	_, err = engine.Prepare(parsed.Template, params)
	require.NoError(t, err, "the engine must be able to run it")
	if kind == "validate" {
		return
	}
	require.Contains(t, []string{"simple", "advanced"}, kind)
	shape := referenceShapeOf(parsed.Template, parsed.Edges)
	shape.requireCommon(t)
	if kind == "advanced" {
		shape.requireAdvanced(t)
	} else {
		shape.requireSimple(t)
	}
}

type referenceShape struct {
	tmpl  *model.Template
	edges []model.Edge
}

func referenceShapeOf(tmpl *model.Template, edges []model.Edge) referenceShape {
	return referenceShape{tmpl: tmpl, edges: edges}
}

func (s referenceShape) out(id string) []model.Edge {
	var out []model.Edge
	for _, e := range s.edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

func performerText(p *model.Performer) string {
	if p == nil {
		return ""
	}
	return p.Prompt + " " + p.Ask
}

// decisionsQuoting are the decisions, decided by the lead, whose text
// quotes params.<param>.
func (s referenceShape) leadDecisionsQuoting(param string) []string {
	var ids []string
	for id, node := range s.tmpl.Nodes {
		if node.Type != model.NodeTypeDecision || node.Performer == nil || !node.Performer.Lead {
			continue
		}
		if strings.Contains(strings.ReplaceAll(performerText(node.Performer), " ", ""), "params."+param) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// loopsBackTo are the agent tasks some decision sends work back to.
func (s referenceShape) loopsBackTo() []string {
	var to []string
	for id, node := range s.tmpl.Nodes {
		for outcome := range node.Loops {
			for _, e := range s.out(id) {
				if e.Outcome == outcome {
					target := s.tmpl.Nodes[e.To]
					if target.Type == model.NodeTypeTask && target.Performer != nil && target.Performer.Kind == model.PerformerAgent {
						to = append(to, e.To)
					}
				}
			}
		}
	}
	return to
}

func (s referenceShape) requireCommon(t *testing.T) {
	t.Helper()
	// The operator decides the idea (a person's decision), the lead drives.
	human := false
	lead := 0
	for _, node := range s.tmpl.Nodes {
		if node.Type == model.NodeTypeDecision && node.Performer != nil && node.Performer.Kind == model.PerformerHuman {
			human = true
		}
		if node.Performer != nil && node.Performer.Lead {
			lead++
		}
	}
	assert.True(t, human, "the operator decides the high-level idea")
	assert.GreaterOrEqual(t, lead, 3, "the lead provisions, decides, readies and merges")
	// Merge only when auto-merge is on: a param, quoted by a lead decision
	// with a way that merges and one that does not.
	_, ok := s.tmpl.Params["auto_merge"]
	if !ok {
		for name := range s.tmpl.Params {
			if strings.Contains(name, "merge") {
				ok = true
			}
		}
	}
	assert.True(t, ok, "a param says whether to merge on green CI: %v", s.tmpl.Params)
	merges := 0
	for name := range s.tmpl.Params {
		if strings.Contains(name, "merge") {
			for _, id := range s.leadDecisionsQuoting(name) {
				if len(s.out(id)) >= 2 {
					merges++
				}
			}
		}
	}
	assert.Positive(t, merges, "a lead decision reads the merge param and branches on it")
	assert.NotEmpty(t, s.loopsBackTo(), "a decision sends the work back to the implementor")
}

func (s referenceShape) requireSimple(t *testing.T) {
	t.Helper()
	// One cold reviewer who is not the implementor.
	reviewers := 0
	for _, node := range s.tmpl.Nodes {
		for _, p := range performerSlice(node.Performer) {
			if p.Kind == model.PerformerAgent && len(p.DifferentFrom) > 0 {
				reviewers++
			}
		}
		for _, p := range node.Deciders {
			if p.Kind == model.PerformerAgent && len(p.DifferentFrom) > 0 {
				reviewers++
			}
		}
	}
	assert.Positive(t, reviewers, "the cold reviewer is kept off the implementor")
}

func (s referenceShape) requireAdvanced(t *testing.T) {
	t.Helper()
	// Four reviewers in parallel: a gate of four agent deciders, or a
	// four-way fork of agent steps that joins.
	four := false
	for id, node := range s.tmpl.Nodes {
		if node.Type == model.NodeTypeDecision && len(node.Deciders) >= 4 {
			agents := 0
			for _, d := range node.Deciders {
				if d.Kind == model.PerformerAgent {
					agents++
				}
			}
			four = four || agents >= 4
		}
		if node.Type == model.NodeTypeParallel && len(s.out(id)) >= 4 {
			four = true
		}
	}
	assert.True(t, four, "four reviewers run in parallel")
	param := ""
	for name, p := range s.tmpl.Params {
		about := strings.ToLower(name + " " + p.Name + " " + p.Description)
		if strings.Contains(about, "amend") || strings.Contains(about, "adjust") || strings.Contains(about, "criteria") {
			param = name
		}
	}
	require.NotEmpty(t, param, "a param says whether the lead may amend the criteria: %v", s.tmpl.Params)
	assert.NotEmpty(t, s.leadDecisionsQuoting(param), "a lead decision reads %s", param)
	criteria := false
	for _, node := range s.tmpl.Nodes {
		if strings.Contains(strings.ToLower(node.Name+performerText(node.Performer)), "criteria") {
			criteria = true
		}
	}
	assert.True(t, criteria, "acceptance criteria are defined and checked")
	fmt.Fprintln(os.Stderr, "reference shape: ok")
}

func performerSlice(p *model.Performer) []model.Performer {
	if p == nil {
		return nil
	}
	return []model.Performer{*p}
}
