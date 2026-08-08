package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copilotAPIPostureWriters names every function allowed to write a Copilot
// drive posture, and the writer it is allowed to call.
//
// The list is the artefact. Two entries, both at LAUNCH time, and that is the
// whole point: a posture is written when a launch decides one, and at no other
// moment. Anything that appears here later is a revoke, whatever it is called.
var copilotAPIPostureWriters = []struct {
	dir, file, function, writer string
}{
	// The daemon's launch seam. Records what THIS launch chose, against the
	// conversation it launched.
	{".", "copilot_api_launch.go", "recordCopilotAPIPosture", "SetConversationCopilotAPI"},
	// The launched process, recording its own posture against its session row.
	{"../session", "relaunch_carryover.go", "RecordLaunchPosture", "SetSessionCopilotAPI"},
}

// TestNoAutomaticRevokeOfTheCopilotAPIPosture is the mechanical statement that
// the daemon never demotes an API-driven agent to send-keys.
//
// # The decision it enforces
//
// The API drive is a CONSTRAINT, not an optimisation: an operator who turns it
// on is saying "do not put bytes in this agent's pane". So a revoke IS the
// injection sink re-opening, and one the daemon performs by itself withdraws the
// operator's opt-out by weather rather than by the person who chose it. The
// drive is also unverified in real use, which sharpens it — a silent demote
// makes the drive's failures unobservable exactly when they are most
// informative. The full argument, including the fact that this is
// PHASE-DEPENDENT and due for revisiting once the drive is verified, is on
// [copilotAPIChannelFailed]. Read that before adding an entry above.
//
// # Why a test rather than the doc comment alone
//
// Because the doc comment is where the previous two versions of this rule lived,
// and prose cannot bind a call site. The failure being guarded against is not
// somebody disagreeing with the decision — it is somebody writing a plausible
// "the channel is down, fall back" edit without ever meeting the decision, which
// is a two-line change that reads as a bug fix and silently ends the
// conversation this rule came out of.
//
// # What it asserts, and in which direction
//
// UNIVERSALLY: every call site of either posture writer, anywhere in the daemon
// or the session package, must be one of the launch-time sites listed above. It
// collects call sites and fails on any it does not recognise, rather than
// looking for a site it likes and passing when it finds one. That direction is
// deliberate and was learned the hard way in this package: an existential check
// ("a correct call exists") passes with an incorrect call sitting beside it, so
// it is a presence check wearing an execution check's clothes. The question here
// is whether an UNAUTHORISED writer exists, and only the universal form asks it.
//
// # What it cannot see
//
// Stated rather than left for a reader to assume the guard is total:
//
//   - It walks two packages. A posture writer called from a third package is
//     invisible to it. The positive control below limits the damage — it fails
//     loudly if a listed site stops existing — but nothing here notices a new
//     package appearing.
//   - It constrains WHERE the writers are called, not what they are called with.
//     An allow-listed site that computed `false` from a failed channel would
//     pass. That is a narrower hole than it sounds, because both listed sites
//     take their value from the launch's own arguments, but it is a hole.
//   - It says nothing about ROUTING. A future revoke that never writes a record
//     and instead makes [copilotAPIDriven] consult the in-memory observation
//     would be a revoke in every sense that matters to an operator, and this
//     guard would be green. That path is refused in prose on
//     [copilotAPIChannelFailed], and prose is all that refuses it.
func TestNoAutomaticRevokeOfTheCopilotAPIPosture(t *testing.T) {
	writers := map[string]bool{}
	for _, site := range copilotAPIPostureWriters {
		writers[site.writer] = true
	}

	type callSite struct{ file, function, writer string }
	var found []callSite

	for _, dir := range []string{".", "../session"} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") {
				continue
			}
			source, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err)
			parsed, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
			require.NoError(t, err)

			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !writers[selector.Sel.Name] {
						return true
					}
					found = append(found, callSite{
						file: name, function: fn.Name.Name, writer: selector.Sel.Name,
					})
					return true
				})
			}
		}
	}

	allowed := map[callSite]bool{}
	for _, site := range copilotAPIPostureWriters {
		allowed[callSite{site.file, site.function, site.writer}] = true
	}

	// The universal arm. Every site found must be one the list vouches for.
	for _, site := range found {
		assert.Truef(t, allowed[site],
			"%s calls %s in %s, and that is a REVOKE of the agent's Copilot drive "+
				"posture — the daemon does not do that. An agent whose channel failed "+
				"has not un-chosen the API drive; the remedy is the operator's relaunch, "+
				"which retries it. If this is deliberate, the decision on "+
				"copilotAPIChannelFailed has to be reopened first, not worked around "+
				"here: it is phase-dependent and may well be ready to change, but it "+
				"changes in the doc comment and in this list together",
			site.function, site.writer, site.file)
	}

	// The positive control. Every assertion above is over a collected set, so a
	// rename or a move would empty it and leave the guard passing vacuously.
	for _, want := range copilotAPIPostureWriters {
		site := callSite{want.file, want.function, want.writer}
		assert.Containsf(t, found, site,
			"%s no longer calls %s in %s. Either the launch-time posture write is "+
				"gone — which is its own bug, since a conversation with no recorded "+
				"posture routes as send-keys — or it moved and this guard is now "+
				"watching nothing",
			want.function, want.writer, want.file)
	}
}
