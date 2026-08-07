package copilotfixture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smokeManifest is the committed per-PR scenario list. CI uses it twice — as
// the `-run` filter and as the anti-skip gate — so those two cannot disagree
// with each other. What neither can see is a scenario that is missing from the
// file entirely: it is then excluded by the filter, runs nowhere, and leaves
// both the gate and the lab's pass floor green. This test is the third side of
// that triangle, and it needs no CLI, so it runs in the ordinary shard.
const smokeManifest = "../../../../.github/copilot-smoke-scenarios.txt"

// testNamePattern is what a manifest line may contain. A stray line such as
// `.*` would otherwise widen the `-run` filter to the entire suite — including
// every lab scenario — while still satisfying the gate, quietly restoring the
// nine-minute job this list exists to prevent.
var testNamePattern = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)

// TestSmokeManifestMatchesTheGatedScenarios keeps .github/copilot-smoke-scenarios.txt
// equal to the set of scenarios gated for regression mode.
//
// Equality in BOTH directions is the point. A listed-but-ungated name is a
// scenario CI demands a pass from that no longer runs per-PR; an unlisted
// requireSmoke scenario is per-PR coverage someone wrote and CI silently never
// executes. The file's header tells an author to keep it in step; this is what
// makes that instruction hold when they forget.
func TestSmokeManifestMatchesTheGatedScenarios(t *testing.T) {
	t.Parallel()

	listed := readSmokeManifest(t)
	gated := regressionGatedScenarios(t)

	assert.ElementsMatch(t, gated, listed,
		"%s must list exactly the scenarios gated with requireSmoke/requireSmokeParallel.\n"+
			"Adding a per-PR scenario means adding its name here; moving one to the "+
			"discovery lab means deleting the line AND switching its gate to "+
			"requireLab/requireLabParallel.", smokeManifest)
}

// readSmokeManifest returns the scenario names in the committed list, applying
// the same comment and blank-line stripping the workflow does.
func readSmokeManifest(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(smokeManifest)
	require.NoError(t, err, "reading the per-PR scenario manifest")

	var names []string
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		require.Equalf(t, line, trimmed,
			"%s:%d has surrounding whitespace; the workflow greps for the line "+
				"verbatim, so the scenario would silently stop matching", smokeManifest, i+1)
		require.Regexpf(t, testNamePattern, trimmed,
			"%s:%d is not a bare Go test name. The line becomes part of a `-run` "+
				"regex, so a pattern here would widen the per-PR job to scenarios "+
				"that were deliberately demoted to the lab", smokeManifest, i+1)
		names = append(names, trimmed)
	}
	require.NotEmpty(t, names, "the per-PR scenario manifest is empty")
	return names
}

// regressionGatedScenarios reports the Test functions in this package whose
// body calls requireSmoke or requireSmokeParallel — i.e. the per-PR set.
//
// Read from the AST rather than from a registry the tests populate at runtime:
// a registry only records scenarios that were compiled AND reached, which is
// the property under test here.
func regressionGatedScenarios(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "no test files found; is the working directory wrong?")

	var gated []string
	for _, path := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		require.NoErrorf(t, err, "parsing %s", path)

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			// Only top-level Test functions: this skips the gate helpers
			// themselves, whose bodies necessarily call requireSmoke.
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name == "requireSmoke" || ident.Name == "requireSmokeParallel" {
					gated = append(gated, fn.Name.Name)
					return false
				}
				return true
			})
		}
	}
	return gated
}
