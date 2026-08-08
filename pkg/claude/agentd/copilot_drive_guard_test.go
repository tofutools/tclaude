package agentd

import (
	"go/ast"
	"go/parser"
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
		// The value is the SECOND argument in both compare-and-set signatures:
		// (id, value, source, expected). Anchored by position from the front
		// rather than from the back, which is what this guard did until the
		// attribution parameter was added between the value and the guard blob
		// and "the one before the blob" silently became the source.
		require.GreaterOrEqualf(t, len(call.Args), 4,
			"%s call shape changed: this guard reads the value positionally and has to "+
				"be re-derived rather than left pointing at whatever now sits there",
			sel.Sel.Name)
		value := call.Args[1]
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

