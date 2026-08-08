package agentd

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TCL-1082 structural guards.
//
// # What these are for, and what they deliberately are not
//
// A guard written by the author of the code, in the same session, from the same
// model of the problem, does not exercise the case the implementation forgot —
// it is the same forgetting twice. So these do not try to certify that the drive
// switch is correct. Each one pins a property whose violation would be
// PLAUSIBLE, LOCAL and SILENT: a future edit that looks like a tidy-up, compiles,
// passes the behavioural tests, and quietly removes something the behavioural
// tests cannot see the absence of.
//
// Where a guard cannot see something, it says so in its own doc comment rather
// than leaving the gap for a reader to discover.

// TestTheDaemonDownFallbackNeverEscalates is the one structural property worth
// asserting about the CLI, because violating it is a one-word edit.
//
// The daemon-down path may de-escalate and must never escalate: recording "api"
// with no daemon claims a channel nothing can create, and an API-driven
// conversation HOLDS its mail rather than falling back to keystrokes, so the
// agent would silently stop receiving messages. A behavioural test covers the
// refusal today (TestSetDriveDaemonDown_EscalationIsRefused); what it cannot
// catch is a later refactor that keeps the refusal reachable while ALSO giving
// the direct writer an escalating call — two ways in, one of them tested.
//
// So this asserts the narrower and more durable thing: the direct-write function
// never asks for the API drive. Every compare-and-set it performs passes a
// literal false.
//
// # What this guard cannot see
//
// It reads the literal argument at the call site. A refactor that computes the
// value into a variable first — `value := drive == setDriveAPI`, then passes
// `value` — is invisible here and would need the behavioural arm to catch it.
// That is stated rather than hidden, because a guard whose gaps are unstated
// becomes a certificate.
func TestTheDaemonDownFallbackNeverEscalates(t *testing.T) {
	const path = "../agent/set_drive.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	require.NoError(t, err, "parse %s", path)

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if decl, ok := n.(*ast.FuncDecl); ok && decl.Name.Name == "runSetDriveDirect" {
			fn = decl
		}
		return true
	})
	require.NotNil(t, fn, "runSetDriveDirect not found; if it was renamed, this guard "+
		"must follow it rather than being deleted")

	calls := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "CompareAndSet") {
			return true
		}
		calls++
		// The value argument is the one before the guard blob in both
		// compare-and-set signatures.
		require.GreaterOrEqual(t, len(call.Args), 2, "%s call shape changed", sel.Sel.Name)
		value := call.Args[len(call.Args)-2]
		ident, ok := value.(*ast.Ident)
		assert.Truef(t, ok && ident.Name == "false",
			"%s in the daemon-down path must pass a literal false; the fallback may "+
				"de-escalate and must never escalate", sel.Sel.Name)
		return true
	})
	assert.NotZero(t, calls,
		"no compare-and-set found in the daemon-down path — either the fallback stopped "+
			"writing (in which case the rollback no longer works with the daemon down) or "+
			"it started writing some other way this guard cannot see")
}

// TestTheReconnectSweepSkipOnlyReadsAnExplicitFalse guards the condition added
// for this ticket at its most dangerous edge.
//
// The sweep declines to adopt a channel when an operator recorded an explicit
// off. Scoping that to a record that SAYS false is what keeps it from becoming
// "stop closing the injection sink": an absent record and a read failure both
// mean unknown, and both must keep adopting. A plausible simplification —
// dropping the `Record != None` half, or inverting the error check — turns a
// respect-the-operator condition into a silent refusal to reconnect legacy
// conversations, whose only symptom is agents that stop receiving mail.
//
// # What this guard cannot see
//
// It pins the SHAPE of the condition, not its effect. The behavioural pair
// (TestAPinnedDriveIsNotReacquiredByTheReconnectSweep and
// TestAnUnrecordedDriveIsStillAdoptedAfterThePinCondition) is what proves the
// two states actually diverge; this exists because those two cases are easy to
// leave passing while the condition drifts to cover a third.
func TestTheReconnectSweepSkipOnlyReadsAnExplicitFalse(t *testing.T) {
	const path = "copilot_api_reconnect.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	require.NoError(t, err, "parse %s", path)

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if decl, ok := n.(*ast.FuncDecl); ok && decl.Name.Name == "reconcileCopilotAPISessions" {
			fn = decl
		}
		return true
	})
	require.NotNil(t, fn, "reconcileCopilotAPISessions not found")

	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Init == nil {
			return true
		}
		cond := exprText(fset, stmt.Cond)
		if !strings.Contains(cond, "CopilotDriveRecordNone") {
			return true
		}
		found = true
		assert.Contains(t, cond, "err == nil",
			"a read failure means UNKNOWN and must keep adopting; without this half a "+
				"database hiccup would stand the sweep down and look like a decision")
		assert.Contains(t, cond, "!target.Value",
			"the skip must require a record that says FALSE, not merely a record")
		return true
	})
	assert.True(t, found,
		"the reconnect sweep no longer tests the recorded drive before adopting; an "+
			"operator's durable off would be re-acquired at the next daemon restart, "+
			"which is the defect this condition exists to prevent")
}

// exprText renders an expression back to source for a substring assertion.
func exprText(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	return b.String()
}
