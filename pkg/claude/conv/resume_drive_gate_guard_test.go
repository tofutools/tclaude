package conv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The helper every plain-CLI resume in this package launches through, and the
// gate every one of them must consult first.
const (
	convResumeLauncher  = "LaunchDetachedTmuxSession"
	convResumeDriveGate = "resumeCopilotDriveGate"
)

// Every function in this package that launches a pane must consult the Copilot
// drive gate, and this is an AST scan rather than a behavioural test because the
// failure mode is a launch site nobody remembered.
//
// That is not hypothetical here: TCL-1076 exists because `tclaude conv resume`
// grew a sibling — the watch-mode resume — that shares resumeLaunchCmd and had
// the same gap, and the ticket was filed against only the verb someone happened
// to look at. TCL-1058's lesson, applied to this family: enumerate by what
// shares the helper, and then make the enumeration durable so the third surface
// inherits the answer instead of the bug.
//
// Deliberately keyed on the LAUNCH, not on the resume-command builder. A
// function may render a launch command for inspection (the tests in this package
// do it constantly) without launching anything; what must never happen is a pane
// coming up for an API-driven conversation without the human being told.
func TestEveryConvResumeLaunchConsultsTheCopilotDriveGate(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	launchers := map[string]bool{} // "file.go:funcName" → gate consulted
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, err)

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			called := calledFunctionNames(fn.Body)
			if !slices.Contains(called, convResumeLauncher) {
				continue
			}
			launchers[name+":"+fn.Name.Name] = slices.Contains(called, convResumeDriveGate)
		}
	}

	// The positive control. Every assertion below is satisfied by finding no
	// launchers at all, which is what a renamed launch helper produces — and a
	// guard that reports clean over calls it cannot see is worse than no guard,
	// because it still reads as protection.
	require.Len(t, launchers, 2,
		"expected exactly the two plain-CLI resume launchers (conv resume and the watch-mode "+
			"resume) to call %s, found %v. A new one is fine — add it here once it consults "+
			"the gate. Zero means this guard has gone blind, most likely because %s was "+
			"renamed", convResumeLauncher, launchers, convResumeLauncher)

	for site, consulted := range launchers {
		assert.Truef(t, consulted,
			"%s launches a pane without calling %s. A conversation that chose the Copilot API "+
				"drive cannot get it from this package — the channel is created and held inside "+
				"agentd — so an ungated launch silently downgrades it, and for a managed agent "+
				"produces a pane that looks healthy while agentd routes its mail to a channel "+
				"that does not exist (TCL-1076)", site, convResumeDriveGate)
	}
}

// The guard's own positive control: an ungated launcher must be REPORTED, and a
// gated one must not. Without this the scan above is satisfied by a matcher that
// never notices anything.
func TestConvResumeDriveGateScannerReportsAnUngatedLauncher(t *testing.T) {
	const gated = `package p

func launch() error {
	if _, err := resumeCopilotDriveGate(h, conv, false); err != nil {
		return err
	}
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}
`
	const ungated = `package p

func launch() error {
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}
`
	for _, tc := range []struct {
		name          string
		source        string
		wantConsulted bool
	}{
		{name: "gated", source: gated, wantConsulted: true},
		{name: "ungated", source: ungated, wantConsulted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, "probe.go", tc.source, 0)
			require.NoError(t, err)
			fn, ok := parsed.Decls[0].(*ast.FuncDecl)
			require.True(t, ok)
			called := calledFunctionNames(fn.Body)
			require.Contains(t, called, convResumeLauncher,
				"the probe must launch, or it is not exercising the matcher")
			assert.Equal(t, tc.wantConsulted, slices.Contains(called, convResumeDriveGate))
		})
	}
}

// calledFunctionNames collects the callee names in a body, matching both a bare
// call and a qualified one — `LaunchDetachedTmuxSession(...)` and
// `session.LaunchDetachedTmuxSession(...)` are the same launch, and a guard that
// saw only one spelling would go blind the moment the other was used. That
// failure has a precedent in this series: a structural guard that matched a
// single receiver spelling stayed green over the package's dominant idiom.
func calledFunctionNames(body *ast.BlockStmt) []string {
	var names []string
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			names = append(names, fun.Name)
		case *ast.SelectorExpr:
			if fun.Sel != nil {
				names = append(names, fun.Sel.Name)
			}
		}
		return true
	})
	return names
}
