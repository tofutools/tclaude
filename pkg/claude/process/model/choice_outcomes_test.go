package model

import (
	"strings"
	"testing"
)

func TestChoiceOutcomeValidation(t *testing.T) {
	workTemplate := func(performer Performer) *Template {
		return &Template{
			APIVersion: APIVersion, Kind: Kind, ID: "choice-routing", Start: "work",
			Nodes: map[string]Node{
				"work": {Type: NodeTypeTask, Performer: &performer, Next: Next{"pass": "done"}},
				"done": {Type: NodeTypeEnd},
			},
		}
	}
	find := func(diagnostics Diagnostics, code, path string) bool {
		for _, diagnostic := range diagnostics {
			if diagnostic.Code == code && strings.Contains(diagnostic.Path, path) {
				return true
			}
		}
		return false
	}

	t.Run("duplicate labels use executor matching semantics", func(t *testing.T) {
		performer := Performer{Kind: PerformerHuman, Ask: "Ship?", Choices: []string{"Ship", " ship "},
			ChoiceOutcomes: map[string]string{"Ship": "pass", "ship": "fail"}}
		tmpl := workTemplate(performer)
		diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
		if !find(diagnostics, "duplicate_choice", "choices[1]") {
			t.Fatalf("missing path-specific duplicate diagnostic: %#v", diagnostics)
		}
	})

	t.Run("unicode simple-fold labels are ambiguous", func(t *testing.T) {
		performer := Performer{Kind: PerformerHuman, Ask: "Route?", Choices: []string{"Σ", "ς"},
			ChoiceOutcomes: map[string]string{"Σ": "pass", "ς": "fail"}}
		tmpl := workTemplate(performer)
		diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
		if !find(diagnostics, "duplicate_choice", "choices[1]") {
			t.Fatalf("missing EqualFold-equivalent duplicate diagnostic: %#v", diagnostics)
		}
	})

	t.Run("missing and extra map keys fail at exact paths", func(t *testing.T) {
		performer := Performer{Kind: PerformerHuman, Ask: "Ship?", Choices: []string{"ship", "hold"},
			ChoiceOutcomes: map[string]string{"ship": "pass", "later": "fail"}}
		tmpl := workTemplate(performer)
		diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
		if !find(diagnostics, "missing_choice_outcome", "choiceOutcomes.hold") ||
			!find(diagnostics, "extra_choice_outcome", "choiceOutcomes.later") {
			t.Fatalf("missing exact-map diagnostics: %#v", diagnostics)
		}
	})

	t.Run("invalid outcome fails loudly", func(t *testing.T) {
		performer := Performer{Kind: PerformerHuman, Ask: "Ship?", Choices: []string{"ship"},
			ChoiceOutcomes: map[string]string{"ship": "maybe"}}
		tmpl := workTemplate(performer)
		diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
		if !find(diagnostics, "invalid_choice_outcome", "choiceOutcomes.ship") {
			t.Fatalf("missing invalid outcome diagnostic: %#v", diagnostics)
		}
	})
}

func TestDecisionChoicesRemainEdgeDriven(t *testing.T) {
	performer := Performer{Kind: PerformerHuman, Ask: "Route?", Choices: []string{"ship", "hold"}}
	tmpl := &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "decision-choices", Start: "decide",
		Nodes: map[string]Node{
			"decide": {Type: NodeTypeDecision, Performer: &performer, Next: Next{"ship": "done", "hold": "done"}},
			"done":   {Type: NodeTypeEnd},
		},
	}
	if diagnostics := Validate(tmpl, NormalizeEdges(tmpl)); diagnostics.HasErrors() {
		t.Fatalf("decision choices must not require choiceOutcomes: %#v", diagnostics.Errors())
	}
	performer.ChoiceOutcomes = map[string]string{"ship": "pass", "hold": "fail"}
	tmpl.Nodes["decide"] = Node{Type: NodeTypeDecision, Performer: &performer, Next: Next{"ship": "done", "hold": "done"}}
	diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
	found := false
	for _, diagnostic := range diagnostics {
		found = found || diagnostic.Code == "choice_outcomes_on_decision"
	}
	if !found {
		t.Fatalf("decision choiceOutcomes must be rejected as inapplicable: %#v", diagnostics)
	}
}
