package conv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
// drive gate AND ACT ON WHAT IT SAYS, and this is an AST scan rather than a
// behavioural test because the failure mode is a launch site nobody remembered.
//
// The three shape checks past "was it called" are here because the first version
// of this guard had only that one, and a cold reviewer beat it in one edit:
// replacing the gate block with `_, _ = resumeCopilotDriveGate(h, conv, true)`
// left the ENTIRE package green — refusal discarded, override pinned on, no
// refusal reachable, guard satisfied. That is the difference between asserting
// the property that broke and the property that was easy to express, so each
// check below names the mutation it exists to catch:
//
//   - the error result must be BOUND, not `_`: a discarded refusal is a gate that
//     runs and decides nothing.
//   - the override argument must not be a `true` LITERAL: a hardcoded override
//     turns every refusal into a notice while the call still reads as a gate.
//   - the call must appear BEFORE the launch: a gate consulted afterwards is a
//     report about a pane that already exists.
//
// What it still does not cover, stated rather than left for the next reviewer to
// find: source order is not control flow. A gate call inside a branch that is
// never taken, or one whose bound error is then dropped on the floor by the
// caller, passes this scan. The behavioural table in
// resume_copilot_drive_test.go is what covers the semantics; this covers the
// existence and shape of the call site. Nor can it see a launcher that creates a
// pane by some route other than convResumeLauncher — see the count assertion
// below, which is why the expected number is pinned rather than "at least one".
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

	launchers := map[string]gateUse{} // "file.go:funcName" → how it uses the gate
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
			use := inspectGateUse(fn.Body)
			if !use.launches {
				continue
			}
			launchers[name+":"+fn.Name.Name] = use
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

	for site, use := range launchers {
		assert.Truef(t, use.consultsGate,
			"%s launches a pane without calling %s. A conversation that chose the Copilot API "+
				"drive cannot get it from this package — the channel is created and held inside "+
				"agentd — so an ungated launch silently downgrades it, and for a managed agent "+
				"produces a pane that looks healthy while agentd routes its mail to a channel "+
				"that does not exist (TCL-1076)", site, convResumeDriveGate)
		if !use.consultsGate {
			continue
		}
		assert.Truef(t, use.bindsError,
			"%s calls %s and discards its error. A refusal nobody reads is not a gate: the "+
				"launch proceeds and the human is told nothing", site, convResumeDriveGate)
		assert.Falsef(t, use.overridePinned,
			"%s passes a literal true for the send-keys override, which turns every refusal "+
				"into a notice while the call site still reads as gated. The override is the "+
				"HUMAN's to give", site)
		assert.Truef(t, use.gateBeforeLaunch,
			"%s consults %s after it has already launched. A gate that runs after the pane "+
				"exists reports on it rather than deciding about it", site, convResumeDriveGate)
	}
}

// gateUse is what one function does about the gate: enough to tell a real gating
// from a call that merely mentions it.
type gateUse struct {
	launches         bool
	consultsGate     bool
	bindsError       bool
	overridePinned   bool
	gateBeforeLaunch bool
}

// inspectGateUse reads one function body. Both call spellings are matched — bare
// and qualified — because a guard that saw only one goes blind the moment the
// other is used, and this series has already shipped one that did (a structural
// guard matching a single receiver spelling stayed green over the package's
// dominant idiom).
func inspectGateUse(body *ast.BlockStmt) gateUse {
	var use gateUse
	gatePos, launchPos := token.NoPos, token.NoPos
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calleeName(call) {
		case convResumeLauncher:
			use.launches = true
			if !launchPos.IsValid() || call.Pos() < launchPos {
				launchPos = call.Pos()
			}
		case convResumeDriveGate:
			use.consultsGate = true
			if !gatePos.IsValid() || call.Pos() < gatePos {
				gatePos = call.Pos()
			}
			// The override argument, by position: (harness, convID, allowSendKeys,
			// overrideHint). A literal `true` there is the mutation this catches; a
			// variable or a literal `false` is fine.
			if len(call.Args) >= 3 {
				if ident, ok := call.Args[2].(*ast.Ident); ok && ident.Name == "true" {
					use.overridePinned = true
				}
			}
		}
		return true
	})
	if use.consultsGate {
		use.bindsError = gateErrorIsBound(body)
		use.gateBeforeLaunch = !launchPos.IsValid() || (gatePos.IsValid() && gatePos < launchPos)
	}
	return use
}

// gateErrorIsBound reports whether the gate's second result lands in a real
// variable rather than in `_`. `_, _ = gate(...)` is the shape that defeated the
// first version of this guard.
func gateErrorIsBound(body *ast.BlockStmt) bool {
	bound := false
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) < 2 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || calleeName(call) != convResumeDriveGate {
			return true
		}
		errLHS, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
		if ok && errLHS.Name != "_" {
			bound = true
		}
		return true
	})
	return bound
}

// calleeName reduces a call to the identifier being called, matching a bare
// `f(...)` and a qualified `pkg.f(...)` alike.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if fun.Sel != nil {
			return fun.Sel.Name
		}
	}
	return ""
}

// The guard's own positive controls, one per mutation it claims to catch. Every
// assertion in the scan above is satisfied by a matcher that notices nothing, so
// each shape is driven through the real inspector here — including, verbatim, the
// edit a cold reviewer used to beat the first version of this guard.
func TestConvResumeDriveGateScannerCatchesEachMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   gateUse
	}{
		{
			name: "gated properly",
			source: `package p
func launch() error {
	notice, err := resumeCopilotDriveGate(h, conv, sendKeys, hint)
	if err != nil {
		return err
	}
	_ = notice
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}`,
			want: gateUse{launches: true, consultsGate: true, bindsError: true, gateBeforeLaunch: true},
		},
		{
			name: "no gate at all",
			source: `package p
func launch() error {
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}`,
			want: gateUse{launches: true},
		},
		{
			// The reviewer's edit, character for character in shape: the call is
			// there, the refusal is thrown away, the override is pinned on.
			name: "refusal discarded and override pinned",
			source: `package p
func launch() error {
	_, _ = resumeCopilotDriveGate(h, conv, true, hint)
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}`,
			want: gateUse{launches: true, consultsGate: true, overridePinned: true, gateBeforeLaunch: true},
		},
		{
			name: "gate consulted only after the pane exists",
			source: `package p
func launch() error {
	if err := session.LaunchDetachedTmuxSession(name, cwd, cmd); err != nil {
		return err
	}
	_, err := resumeCopilotDriveGate(h, conv, sendKeys, hint)
	return err
}`,
			want: gateUse{launches: true, consultsGate: true, bindsError: true, gateBeforeLaunch: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, "probe.go", tc.source, 0)
			require.NoError(t, err)
			fn, ok := parsed.Decls[0].(*ast.FuncDecl)
			require.True(t, ok)
			require.NotNil(t, fn.Body)

			use := inspectGateUse(fn.Body)
			require.True(t, use.launches,
				"every probe must launch, or it is not exercising the matcher at all")
			assert.Equal(t, tc.want, use)
		})
	}
}

// The qualified-call spelling must be matched too. Kept as its own arm because
// the failure it guards against is specific and has a precedent in this series:
// a structural guard that matched one receiver spelling reported clean over the
// package's dominant idiom, which is exactly the shape of protection that is
// worse than none.
func TestConvResumeDriveGateScannerMatchesBothCallSpellings(t *testing.T) {
	for _, source := range []string{
		`package p
func launch() error {
	_, err := resumeCopilotDriveGate(h, conv, sendKeys, hint)
	if err != nil {
		return err
	}
	return LaunchDetachedTmuxSession(name, cwd, cmd)
}`,
		`package p
func launch() error {
	_, err := conv.resumeCopilotDriveGate(h, conv, sendKeys, hint)
	if err != nil {
		return err
	}
	return session.LaunchDetachedTmuxSession(name, cwd, cmd)
}`,
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "probe.go", source, 0)
		require.NoError(t, err)
		fn := parsed.Decls[0].(*ast.FuncDecl)
		use := inspectGateUse(fn.Body)
		assert.True(t, use.launches, "the launch must be seen in both spellings")
		assert.True(t, use.consultsGate, "the gate must be seen in both spellings")
	}
}
