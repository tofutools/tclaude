package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const subprocessTemplateYAML = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: review-flow
params:
  repo: {type: string}
start: begin
nodes:
  begin:
    type: start
    next: review
  review:
    type: subprocess
    subprocess:
      params:
        target: {type: string}
        strict: {type: boolean, default: false}
      start: lint
      nodes:
        lint:
          type: task
          performer: {kind: program, run: lint, args: ["{{ params.target }}"]}
          next: gate
        gate:
          type: decision
          performer: {kind: human, ask: "Ship it"}
          next: {pass: ok, fail: bad}
        ok: {type: end}
        bad: {type: end, result: failed}
      inputs:
        target: "{{ params.repo }}"
      outputs:
        ok: approved
        bad: rejected
    next: {approved: done, rejected: done}
  done:
    type: end
`

func TestSubprocessInlineBodyParsesValidatesAndRoundTrips(t *testing.T) {
	parsed, err := Parse([]byte(subprocessTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("diagnostics: %#v", parsed.Diagnostics)
	}
	review := parsed.Template.Nodes["review"]
	body, ok := review.Subprocess.Body()
	if !ok || body.Start != "lint" || len(body.Nodes) != 4 {
		t.Fatalf("body = %#v, %v", body, ok)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if again.Diagnostics.HasErrors() || again.SemanticHash != parsed.SemanticHash {
		t.Fatalf("round trip changed semantics: %#v\n%s", again.Diagnostics, canonical)
	}

	var visited []string
	WalkNodes(parsed.Template, func(id string, _ Node) { visited = append(visited, id) })
	if want := "begin,done,review,review/bad,review/gate,review/lint,review/ok"; strings.Join(visited, ",") != want {
		t.Fatalf("WalkNodes = %v", visited)
	}
}

func TestSubprocessPortsAndBodyAreValidatedWithNestedPaths(t *testing.T) {
	source := strings.NewReplacer(
		`target: "{{ params.repo }}"`, `strict: "maybe"
        ghost: "x"`,
		`bad: rejected`, `bad: missing`,
		`next: gate`, `next: nowhere`,
	).Replace(subprocessTemplateYAML)
	diagnostics := mustParse(t, source)
	assertDiagnostic(t, diagnostics, SeverityError, "unknown_input", "nodes.review.subprocess.inputs.ghost")
	assertDiagnostic(t, diagnostics, SeverityError, "input_type_mismatch", "nodes.review.subprocess.inputs.strict")
	assertDiagnostic(t, diagnostics, SeverityError, "unbound_input", "nodes.review.subprocess.inputs.target")
	assertDiagnostic(t, diagnostics, SeverityError, "unknown_output_outcome", "nodes.review.subprocess.outputs.bad")
	// The body is validated as its own scope, and its findings carry its path.
	assertDiagnostic(t, diagnostics, SeverityError, "unknown_target", "nodes.review.subprocess.nodes.lint.next.next")
}

func TestSubprocessSourceAndPlacementRules(t *testing.T) {
	both := strings.Replace(subprocessTemplateYAML, "      start: lint\n", "      start: lint\n      templateRef: other\n", 1)
	assertDiagnostic(t, mustParse(t, both), SeverityError, "subprocess_source_conflict", "nodes.review.subprocess")

	// Only run creation embeds a referenced body; source can never carry one.
	forged := strings.Replace(subprocessTemplateYAML, "      start: lint\n", "      start: lint\n      resolved: {}\n", 1)
	assertDiagnostic(t, mustParse(t, forged), SeverityError, "unknown_field", "nodes.review.subprocess.resolved")

	misplaced := strings.Replace(subprocessTemplateYAML, "  done:\n    type: end\n", "  done:\n    type: end\n    subprocess: {templateRef: other}\n", 1)
	assertDiagnostic(t, mustParse(t, misplaced), SeverityError, "subprocess_on_non_subprocess_node", "nodes.done.subprocess")

	ref := strings.Replace(subprocessTemplateYAML, "      params:\n        target: {type: string}\n        strict: {type: boolean, default: false}\n", "      templateRef: other\n      version: sha256:nope\n", 1)
	ref = ref[:strings.Index(ref, "      start: lint")] + "      outputs: {}\n    next: {approved: done, rejected: done}\n  done:\n    type: end\n"
	diagnostics := mustParse(t, ref)
	assertDiagnostic(t, diagnostics, SeverityError, "invalid_template_version", "nodes.review.subprocess.version")
	assertNoDiagnostic(t, diagnostics, "missing_subprocess_source")
}

func TestResolveSubprocessRefsEmbedsBodiesAndRejectsCycles(t *testing.T) {
	leaf := &Template{APIVersion: APIVersion, Kind: Kind, ID: "leaf", Start: "go",
		Nodes: map[string]Node{
			"go":   {Type: NodeTypeTask, Performer: &Performer{Kind: PerformerProgram, Run: "true"}, Next: Next{"next": "stop"}},
			"stop": {Type: NodeTypeEnd},
		}}
	referencing := func(id, ref string) *Template {
		return &Template{APIVersion: APIVersion, Kind: Kind, ID: id, Start: "box",
			Nodes: map[string]Node{
				"box":  {Type: NodeTypeSubprocess, Subprocess: &Subprocess{TemplateRef: ref}, Next: Next{"next": "stop"}},
				"stop": {Type: NodeTypeEnd},
			}}
	}
	const version = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	store := map[string]*Template{"leaf": leaf, "mid": referencing("mid", "leaf"), "loop-a": referencing("loop-a", "loop-b"), "loop-b": referencing("loop-b", "loop-a")}
	resolve := func(id, _ string) (*Template, string, error) {
		tmpl, ok := store[id]
		if !ok {
			return nil, "", errors.New("not found")
		}
		return tmpl, version, nil
	}

	resolved, err := ResolveSubprocessRefs(referencing("top", "mid"), resolve)
	if err != nil {
		t.Fatal(err)
	}
	box := resolved.Nodes["box"].Subprocess
	if box.Version != "" || box.Resolved == nil || box.Resolved.ID != "mid" {
		t.Fatalf("box = %#v", box)
	}
	inner := box.Resolved.Nodes["box"].Subprocess
	if inner.Resolved == nil || inner.Resolved.ID != "leaf" {
		t.Fatalf("nested reference was not resolved: %#v", inner)
	}
	if diagnostics := Validate(resolved, NormalizeEdges(resolved)); diagnostics.HasErrors() {
		t.Fatalf("resolved template is invalid: %#v", diagnostics)
	}
	// The snapshot is self-contained: the semantic JSON carries the bodies.
	snapshot, err := CanonicalSemanticJSON(resolved)
	if err != nil || !strings.Contains(string(snapshot), `"resolved"`) {
		t.Fatalf("snapshot lacks the resolved body: %v %s", err, snapshot)
	}
	if store["mid"].Nodes["box"].Subprocess.Resolved != nil {
		t.Fatalf("resolution mutated the stored template")
	}
	// Resolution pins bodies without changing the template's authored identity.
	authored, _ := SemanticHash(referencing("top", "mid"))
	if pinned, _ := SemanticHash(resolved); pinned != authored {
		t.Fatalf("semantic hash changed by resolution: %s != %s", pinned, authored)
	}

	if _, err := ResolveSubprocessRefs(store["loop-a"], resolve); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	if _, err := ResolveSubprocessRefs(referencing("top", "ghost"), resolve); err == nil {
		t.Fatalf("missing reference resolved")
	}
}

func TestBindInputsDefaultsAndMissingParentParams(t *testing.T) {
	body := &Template{Params: map[string]Param{
		"limit":  {Type: "number", Default: float64(1000000)},
		"ratio":  {Type: "number", Default: 0.25},
		"target": {Type: "string", Default: "fallback"},
		"name":   {Type: "string"},
	}}
	sp := &Subprocess{Inputs: map[string]string{
		"target": "{{ params.missing }}",
		"name":   "{{ params.who }}-x",
	}}
	got := sp.BindInputs(body, map[string]string{"who": "ann"})
	want := map[string]string{"limit": "1000000", "ratio": "0.25", "target": "fallback", "name": "ann-x"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %#v)", k, got[k], v, got)
		}
	}
	got = sp.BindInputs(body, map[string]string{})
	if _, ok := got["name"]; ok {
		t.Fatalf("binding over a missing parent param must leave the param absent: %#v", got)
	}
}

func TestResolveSubprocessRefsBoundsFanOut(t *testing.T) {
	// wide references "leaf" from many subprocess nodes; each level multiplies.
	wide := func(id, ref string, n int) *Template {
		nodes := map[string]Node{"stop": {Type: NodeTypeEnd}}
		for i := 0; i < n; i++ {
			nodes[fmt.Sprintf("b%d", i)] = Node{Type: NodeTypeSubprocess, Subprocess: &Subprocess{TemplateRef: ref}, Next: Next{"next": "stop"}}
		}
		return &Template{APIVersion: APIVersion, Kind: Kind, ID: id, Start: "b0", Nodes: nodes}
	}
	leaf := &Template{APIVersion: APIVersion, Kind: Kind, ID: "leaf", Start: "go",
		Nodes: map[string]Node{
			"go":   {Type: NodeTypeTask, Performer: &Performer{Kind: PerformerProgram, Run: "true"}, Next: Next{"next": "stop"}},
			"stop": {Type: NodeTypeEnd},
		}}
	store := map[string]*Template{"leaf": leaf, "l1": wide("l1", "leaf", 60), "l2": wide("l2", "l1", 60)}
	resolve := func(id, _ string) (*Template, string, error) { return store[id], "", nil }
	_, err := ResolveSubprocessRefs(wide("top", "l2", 60), resolve)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("fan-out error = %v", err)
	}
}

// The editor lays inline body nodes out under namespaced keys; those are
// live layout, while a key into a missing body node stays stale.
func TestSubprocessNamespacedLayoutKeys(t *testing.T) {
	source := subprocessTemplateYAML + `
layout:
  nodes:
    review: {x: 10, y: 10}
    review/lint: {x: 20, y: 20}
    review/gone: {x: 30, y: 30}
  edges:
    review/gate:
      pass: {labelPinned: true}
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	var stale []string
	for _, d := range parsed.Diagnostics {
		if d.Code == "stale_layout_node" || d.Code == "stale_layout_edge" {
			stale = append(stale, d.Path)
		}
	}
	if fmt.Sprint(stale) != "[layout.nodes.review/gone]" {
		t.Fatalf("stale layout diagnostics = %v", stale)
	}
}
