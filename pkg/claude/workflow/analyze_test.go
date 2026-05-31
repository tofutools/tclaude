package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadOK loads a template expected to be valid and returns it.
func loadOK(t *testing.T, workflowYAML, mmd string, nodes map[string]string) *Template {
	t.Helper()
	tmpl, err := LoadFS(tmplFS(workflowYAML, mmd, nodes), "ok", SourceUser, "")
	require.NoError(t, err)
	return tmpl
}

// assertNoneContains fails if any problem contains substr — used to prove the
// graph checks stay separate (e.g. an unreachable node is not also flagged as a
// no-exit pocket).
func assertNoneContains(t *testing.T, problems []string, substr string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, substr) {
			t.Errorf("expected no problem to contain %q, but one did; problems: %v", substr, problems)
			return
		}
	}
}

func humanNodes(ids ...string) map[string]string {
	m := map[string]string{}
	for _, id := range ids {
		m[id] = "executor: {kind: human}\n"
	}
	return m
}

func TestAnalyze_UnreachableNode(t *testing.T) {
	// entry is a, so x (a separate source) can never run. x still reaches the
	// terminal `done`, so only the reachability check should fire.
	p := loadProblems(t,
		"name: t\nentry: a\n",
		"flowchart TD\n a --> done\n x --> done\n",
		humanNodes("a", "x", "done"),
	)
	assertAnyContains(t, p, `node "x" is unreachable`)
	assertNoneContains(t, p, "cannot reach any terminal")
	assertNoneContains(t, p, "no terminal node")
}

func TestAnalyze_NoExitPocket(t *testing.T) {
	// b<->c is a cycle with no exit; both are reachable from entry a but neither
	// can reach the terminal `done`. Only the can-reach-terminal check fires.
	p := loadProblems(t,
		"name: t\n",
		"flowchart TD\n a --> done\n a --> b\n b --> c\n c --> b\n",
		humanNodes("a", "b", "c", "done"),
	)
	assertAnyContains(t, p, `node "b" cannot reach any terminal`)
	assertAnyContains(t, p, `node "c" cannot reach any terminal`)
	assertNoneContains(t, p, "is unreachable")
	assertNoneContains(t, p, "no terminal node")
}

func TestAnalyze_NoTerminal(t *testing.T) {
	// a --> b --> c --> b: a source exists (entry computes to a) but every node
	// has an outgoing edge, so there is no terminal. The single root-cause
	// problem fires, not a per-node co-reachability flood.
	p := loadProblems(t,
		"name: t\n",
		"flowchart TD\n a --> b\n b --> c\n c --> b\n",
		humanNodes("a", "b", "c"),
	)
	assertAnyContains(t, p, "no terminal node")
	assertNoneContains(t, p, "is unreachable")
	assertNoneContains(t, p, "cannot reach any terminal")
}

func TestAnalyze_EnumValueWithoutEdge_Warns(t *testing.T) {
	// `changes` is a declared enum value with no outgoing edge: a non-fatal
	// warning, and the template still loads.
	tmpl := loadOK(t,
		"name: t\nentry: review\n",
		"flowchart TD\n review{Review} -->|approved| done\n",
		map[string]string{
			"review": "executor: {kind: human}\nverify:\n  kind: enum\n  values: [approved, changes]\n",
			"done":   "executor: {kind: human}\n",
		},
	)
	require.Len(t, tmpl.Warnings, 1)
	assert.Contains(t, tmpl.Warnings[0], `enum value "changes"`)
}

func TestAnalyze_EnumFullyCovered_NoWarning(t *testing.T) {
	tmpl := loadOK(t,
		"name: t\nentry: review\n",
		"flowchart TD\n review{Review} -->|approved| done\n review -->|changes| fix\n fix --> done\n",
		map[string]string{
			"review": "executor: {kind: human}\nverify:\n  kind: enum\n  values: [approved, changes]\n",
			"fix":    "executor: {kind: human}\n",
			"done":   "executor: {kind: human}\n",
		},
	)
	assert.Empty(t, tmpl.Warnings)
}

func TestAnalyze_SingleNode_Passes(t *testing.T) {
	// One node, no edges: it is both the (computed) entry and a terminal, so it
	// is reachable and can-reach-terminal. No problems, no warnings.
	tmpl := loadOK(t,
		"name: t\n",
		"flowchart TD\n only[Only node]\n",
		humanNodes("only"),
	)
	assert.Equal(t, []string{"only"}, tmpl.Entry)
	assert.Empty(t, tmpl.Warnings)
}

func TestAnalyze_UnreachableNodesSortedDeterministically(t *testing.T) {
	// Two unreachable nodes must be reported in sorted id order (mmm before zzz).
	p := loadProblems(t,
		"name: t\nentry: a\n",
		"flowchart TD\n a --> done\n zzz --> done\n mmm --> done\n",
		humanNodes("a", "zzz", "mmm", "done"),
	)
	var iMMM, iZZZ = -1, -1
	for i, prob := range p {
		switch {
		case strings.Contains(prob, `node "mmm" is unreachable`):
			iMMM = i
		case strings.Contains(prob, `node "zzz" is unreachable`):
			iZZZ = i
		}
	}
	require.NotEqual(t, -1, iMMM, "mmm unreachable problem missing: %v", p)
	require.NotEqual(t, -1, iZZZ, "zzz unreachable problem missing: %v", p)
	assert.Less(t, iMMM, iZZZ, "expected sorted order mmm before zzz: %v", p)
}

func TestAnalyze_ExampleLoadsCleanWithNoWarnings(t *testing.T) {
	tmpl, err := loadExample("implement-microservice")
	require.NoError(t, err)
	assert.Empty(t, tmpl.Warnings, "the shipped example must load with no topology warnings")
}

// JOH-39: a cycle with no edge leaving it can only end by hitting max_visits.
func TestAnalyze_LoopWithoutExitWarns(t *testing.T) {
	tmpl := build(t, "flowchart TD\n a --> b\n b -->|fail| a\n", map[string]*Node{"b": {OnFail: OnFailContinue}})
	var found bool
	for _, w := range tmpl.Analyze() {
		if strings.Contains(w, "no exit edge") {
			found = true
		}
	}
	assert.True(t, found, "a cycle with no exit edge should warn; got %v", tmpl.Analyze())
}

// A loop WITH a break-on-pass exit edge must not warn.
func TestAnalyze_LoopWithExitNoWarn(t *testing.T) {
	tmpl := build(t, "flowchart TD\n a --> b\n b -->|fail| a\n b -->|pass| done\n",
		map[string]*Node{"b": {OnFail: OnFailContinue}})
	for _, w := range tmpl.Analyze() {
		assert.NotContains(t, w, "no exit edge", "a loop with a |pass| exit must not warn")
	}
}
