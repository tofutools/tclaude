package model

import (
	"fmt"
	"testing"
	"testing/quick"
	"time"
)

const legacyJoinSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: legacy-join
start: fork
nodes:
  fork:
    type: parallel
    next: {left: merge, right: merge}
  merge:
    type: end
    metadata: {join: any, owner: routing-team}
`

func TestAuthoringPromotesLegacyJoinButExactSourceDoesNot(t *testing.T) {
	authored, err := ParseAuthoring([]byte(legacyJoinSource))
	if err != nil {
		t.Fatal(err)
	}
	if authored.Diagnostics.HasErrors() {
		t.Fatalf("authoring diagnostics = %#v", authored.Diagnostics.Errors())
	}
	merge := authored.Template.Nodes["merge"]
	if merge.Join != JoinAny || merge.Metadata["join"] != nil || merge.Metadata["owner"] != "routing-team" {
		t.Fatalf("promoted merge = %#v", merge)
	}

	exact, err := ParseExactSource([]byte(legacyJoinSource))
	if err != nil {
		t.Fatal(err)
	}
	merge = exact.Template.Nodes["merge"]
	if merge.Join != "" || merge.Metadata["join"] != "any" {
		t.Fatalf("exact source was reinterpreted: %#v", merge)
	}
	if authored.SemanticHash == exact.SemanticHash {
		t.Fatal("authoring promotion must create a new semantic version")
	}
	const (
		wantExactHash    = "a3e2fdf0c127a7912e9e2ba893c049b8b67cf454c93bc1b9a54579abe8f954c3"
		wantAuthoredHash = "900fc7fb2957a7262b7ac8530a6b79cb774425a05884aacec2489e8e80e3770d"
	)
	if exact.SemanticHash != wantExactHash || authored.SemanticHash != wantAuthoredHash {
		t.Fatalf("golden hashes changed: exact=%s authored=%s", exact.SemanticHash, authored.SemanticHash)
	}
}

func TestUnchangedLegacyTemplateGoldenHash(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: legacy-unchanged
start: done
nodes:
  done: {type: end}
`)
	authored, err := ParseAuthoring(source)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := ParseExactSource(source)
	if err != nil {
		t.Fatal(err)
	}
	const want = "691989882c16b16dc00742f071614ad483807b3f44378840a8ab468bf0f9d33e"
	if authored.SemanticHash != want || exact.SemanticHash != want {
		t.Fatalf("unchanged legacy golden changed: authored=%s exact=%s", authored.SemanticHash, exact.SemanticHash)
	}
}

func TestTypedJoinWinsAndDisagreementBlocksAuthoring(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: join-conflict
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: choose}
    next: {left: merge, right: merge}
  merge:
    type: end
    join: all
    metadata: {join: any}
`)
	parsed, err := ParseAuthoring(source)
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityError, "join_metadata_conflict") {
		t.Fatalf("diagnostics = %#v", parsed.Diagnostics)
	}
	merge := parsed.Template.Nodes["merge"]
	if merge.Join != JoinAll || merge.Metadata["join"] != nil {
		t.Fatalf("typed join did not win: %#v", merge)
	}
}

func TestParallelAndJoinSchemaRoundTrip(t *testing.T) {
	parsed, err := ParseAuthoring([]byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: parallel-round-trip
start: fork
nodes:
  fork:
    type: parallel
    next: {test: merge, review: merge}
  merge:
    type: end
    join: all
`))
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse = %#v, %v", parsed, err)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ParseAuthoring(canonical)
	if err != nil || roundTrip.Diagnostics.HasErrors() {
		t.Fatalf("round trip = %#v, %v\n%s", roundTrip, err, canonical)
	}
	if roundTrip.Template.Nodes["fork"].Type != NodeTypeParallel || roundTrip.Template.Nodes["merge"].Join != JoinAll {
		t.Fatalf("round trip template = %#v", roundTrip.Template)
	}
	if roundTrip.SemanticHash != parsed.SemanticHash {
		t.Fatalf("semantic hash changed: %s != %s", parsed.SemanticHash, roundTrip.SemanticHash)
	}
}

func TestParallelFieldAndJoinValidation(t *testing.T) {
	tmpl := simpleParallelTemplate(2)
	fork := tmpl.Nodes["fork"]
	fork.Performer = &Performer{Kind: PerformerAgent, Prompt: "must not run"}
	tmpl.Nodes["fork"] = fork
	merge := tmpl.Nodes["merge"]
	merge.Join = "some"
	tmpl.Nodes["merge"] = merge
	diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
	for _, code := range []string{"parallel_fields", "invalid_join"} {
		if !hasDiagnostic(diagnostics, SeverityError, code) {
			t.Fatalf("missing %s in %#v", code, diagnostics)
		}
	}

	tmpl = simpleParallelTemplate(1)
	merge = tmpl.Nodes["merge"]
	merge.Join = JoinAll
	tmpl.Nodes["merge"] = merge
	diagnostics = Validate(tmpl, NormalizeEdges(tmpl))
	for _, code := range []string{"parallel_degree", "join_degree"} {
		if !hasDiagnostic(diagnostics, SeverityError, code) {
			t.Fatalf("missing %s in %#v", code, diagnostics)
		}
	}
}

func TestNormalizedDegreeAndInboundCap(t *testing.T) {
	for _, tc := range []struct {
		degree    int
		wantError bool
	}{
		{degree: MaxNormalizedDegree},
		{degree: MaxNormalizedDegree + 1, wantError: true},
	} {
		t.Run(fmt.Sprint(tc.degree), func(t *testing.T) {
			tmpl := simpleParallelTemplate(tc.degree)
			diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
			gotDegree := hasDiagnostic(diagnostics, SeverityError, "normalized_degree_limit")
			gotInbound := hasDiagnostic(diagnostics, SeverityError, "normalized_inbound_limit")
			if gotDegree != tc.wantError || gotInbound != tc.wantError {
				t.Fatalf("degree=%v inbound=%v diagnostics=%#v", gotDegree, gotInbound, diagnostics)
			}
		})
	}
}

func TestStaticScopeValidationAcceptsLocalReductionAndNesting(t *testing.T) {
	for name, tmpl := range map[string]*Template{
		"local merge": localMergeTemplate(),
		"one scope":   simpleParallelTemplate(3),
		"nested":      nestedParallelTemplate(),
	} {
		t.Run(name, func(t *testing.T) {
			diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
			if hasDiagnostic(diagnostics, SeverityError, "cross_scope_join_v1") {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
		})
	}
}

func TestStaticScopeValidationRejectsUnstructuredClasses(t *testing.T) {
	for name, tmpl := range map[string]*Template{
		"partial and bypass": partialParallelTemplate(),
		"unrelated forks":    unrelatedForkJoinTemplate(),
		"multiple scopes":    multipleScopeJoinTemplate(),
		"multiple reducers":  duplicateReducerTemplate(),
	} {
		t.Run(name, func(t *testing.T) {
			diagnostics := Validate(tmpl, NormalizeEdges(tmpl))
			if !hasDiagnostic(diagnostics, SeverityError, "cross_scope_join_v1") {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
		})
	}
}

func TestStaticScopeValidatorPropertyCompleteVsBypass(t *testing.T) {
	property := func(seed uint8) bool {
		degree := 2 + int(seed%9)
		valid := simpleParallelTemplate(degree)
		if hasDiagnostic(Validate(valid, NormalizeEdges(valid)), SeverityError, "cross_scope_join_v1") {
			return false
		}
		broken := cloneTemplate(valid)
		broken.Nodes["escape"] = Node{Type: NodeTypeEnd}
		fork := broken.Nodes["fork"]
		fork.Next[fmt.Sprintf("branch-%04d", degree-1)] = "escape"
		broken.Nodes["fork"] = fork
		return hasDiagnostic(Validate(&broken, NormalizeEdges(&broken)), SeverityError, "cross_scope_join_v1")
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministicTopologicalOrderWideFrontierIsBounded(t *testing.T) {
	const width = 25_000
	indegree := make(map[string]int, width+1)
	indegree["root"] = 0
	outbound := map[string][]Edge{"root": make([]Edge, 0, width)}
	for i := 0; i < width; i++ {
		target := fmt.Sprintf("node-%05d", width-i-1)
		indegree[target] = 1
		outbound["root"] = append(outbound["root"], Edge{From: "root", Outcome: fmt.Sprintf("branch-%05d", i), To: target})
	}

	started := time.Now()
	order := deterministicTopologicalOrder(indegree, outbound)
	elapsed := time.Since(started)
	if len(order) != width+1 || order[0] != "root" || order[1] != "node-00000" || order[len(order)-1] != "node-24999" {
		t.Fatalf("unexpected order boundary: len=%d first=%q second=%q last=%q", len(order), order[0], order[1], order[len(order)-1])
	}
	// This deliberately generous bound distinguishes the heap traversal from
	// re-sorting the growing ready frontier after every unlocked successor.
	if elapsed > 3*time.Second {
		t.Fatalf("wide-frontier topological traversal took %s", elapsed)
	}
}

func simpleParallelTemplate(degree int) *Template {
	next := make(Next, degree)
	for i := 0; i < degree; i++ {
		next[fmt.Sprintf("branch-%04d", i)] = "merge"
	}
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "parallel", Start: "fork",
		Nodes: map[string]Node{
			"fork":  {Type: NodeTypeParallel, Next: next},
			"merge": {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}

func localMergeTemplate() *Template {
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "local", Start: "choose",
		Nodes: map[string]Node{
			"choose": {Type: NodeTypeDecision, Performer: &Performer{Kind: PerformerHuman, Ask: "choose"}, Next: Next{"left": "merge", "right": "merge"}},
			"merge":  {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}

func nestedParallelTemplate() *Template {
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "nested", Start: "outer",
		Nodes: map[string]Node{
			"outer":      {Type: NodeTypeParallel, Next: Next{"left": "inner", "right": "outer-join"}},
			"inner":      {Type: NodeTypeParallel, Next: Next{"a": "inner-join", "b": "inner-join"}},
			"inner-join": {Type: NodeTypeStart, Join: JoinAll, Next: Next{"pass": "outer-join"}},
			"outer-join": {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}

func partialParallelTemplate() *Template {
	tmpl := simpleParallelTemplate(3)
	tmpl.ID = "partial"
	tmpl.Nodes["escape"] = Node{Type: NodeTypeEnd}
	fork := tmpl.Nodes["fork"]
	fork.Next["branch-0002"] = "escape"
	tmpl.Nodes["fork"] = fork
	return tmpl
}

func multipleScopeJoinTemplate() *Template {
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "multiple-scope", Start: "outer",
		Nodes: map[string]Node{
			"outer": {Type: NodeTypeParallel, Next: Next{"left": "inner", "right": "join"}},
			"inner": {Type: NodeTypeParallel, Next: Next{"a": "join", "b": "join"}},
			"join":  {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}

func unrelatedForkJoinTemplate() *Template {
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "unrelated-forks", Start: "choose",
		Nodes: map[string]Node{
			"choose": {Type: NodeTypeDecision, Performer: &Performer{Kind: PerformerHuman, Ask: "choose"}, Next: Next{"a": "fork-a", "b": "fork-b"}},
			"fork-a": {Type: NodeTypeParallel, Next: Next{"a1": "join-1", "a2": "join-2"}},
			"fork-b": {Type: NodeTypeParallel, Next: Next{"b1": "join-1", "b2": "join-2"}},
			"join-1": {Type: NodeTypeEnd, Join: JoinAll},
			"join-2": {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}

func duplicateReducerTemplate() *Template {
	decider := func() Node {
		return Node{Type: NodeTypeDecision, Performer: &Performer{Kind: PerformerHuman, Ask: "choose"}, Next: Next{"one": "join-1", "two": "join-2"}}
	}
	return &Template{
		APIVersion: APIVersion, Kind: Kind, ID: "duplicate-reducer", Start: "fork",
		Nodes: map[string]Node{
			"fork":   {Type: NodeTypeParallel, Next: Next{"a": "a", "b": "b"}},
			"a":      decider(),
			"b":      decider(),
			"join-1": {Type: NodeTypeEnd, Join: JoinAll},
			"join-2": {Type: NodeTypeEnd, Join: JoinAll},
		},
	}
}
