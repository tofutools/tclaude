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

// packageLevel is the attributed "function" for a reference that is not inside
// any function body — a package-level var holding a func literal, most
// realistically. Such a declaration reads exactly like ordinary code in a diff,
// so it needs a name in the report rather than being skipped.
const packageLevel = "(package-level declaration)"

// copilotAPIPostureWriters names every function that can put a Copilot drive
// posture into a durable record, and every place allowed to reference one.
//
// # Which writers, and why these
//
// Not just the two obvious setters. The record that actually ROUTES is the agent
// relaunch profile — [copilotLaunchIntentForConv] reads it first — so a guard
// that watched only the conversation-scoped setters would be watching the two
// records least able to demote an agent while ignoring the one that can. That is
// the inversion this list exists to avoid, and it was the first thing a cold
// reviewer found when the list had only the two setters on it.
//
// SetConversationResumeProfile is here because REPLACING a conversation profile
// erases the posture in it, and an erased posture reads as send-keys.
var copilotAPIPostureWriters = []struct {
	file, function, writer string
}{
	// The daemon's launch seam. Records what THIS launch chose, against the
	// conversation it launched.
	{"copilot_api_launch.go", "recordCopilotAPIPosture", "SetConversationCopilotAPI"},
	// The launched process, recording its own posture against its session row.
	{"relaunch_carryover.go", "RecordLaunchPosture", "SetSessionCopilotAPI"},
	// Enrollment composes and writes the agent profile. This is the primary
	// posture write for every spawned agent.
	{"lifecycle.go", "enrollSpawnedConv", "SetAgentRelaunchProfile"},
	// Legacy backfill for a conversation whose resume profile went missing.
	{"lifecycle.go", "recoverMissingConversationResumeProfile", "SetAgentRelaunchProfile"},
	{"lifecycle.go", "recoverMissingConversationResumeProfile", "SetConversationResumeProfile"},
}

// TestNoAutomaticRevokeOfTheCopilotAPIPosture is the mechanical statement that
// the daemon never demotes an API-driven agent to send-keys.
//
// # The decision it enforces
//
// The API drive is a CONSTRAINT, not an optimisation: an operator who turns it
// on is saying "do not put bytes in this agent's pane". So a revoke IS the
// injection sink re-opening, and one the daemon performs by itself withdraws the
// operator's opt-out by weather rather than by the person who chose it. The full
// argument, including that this is PHASE-DEPENDENT and due for revisiting once
// the drive is verified, is on [copilotAPIChannelFailed]. Read that before
// adding an entry above.
//
// # Why a test rather than the doc comment alone
//
// Because the doc comment is where the previous two versions of this rule lived,
// and prose cannot bind a call site. The failure guarded against is not somebody
// disagreeing with the decision — it is somebody writing a plausible "the channel
// is down, fall back" edit without ever meeting the decision.
//
// # It matches REFERENCES, not calls, and it walks every declaration
//
// Both choices are load-bearing and both were bought with a beating. An earlier
// version of this guard matched only *ast.SelectorExpr CALLS inside *ast.FuncDecl
// bodies, and a reviewer beat it three ways in a few minutes: taking the function
// as a value (`set := db.SetConversationCopilotAPI`), dot-importing db so the
// call is a bare identifier, and putting the revoke in a package-level var
// holding a func literal. None of those is obfuscation; the third reads as
// completely ordinary Go in a diff.
//
// So this matches any REFERENCE to a watched name, whether it is called, taken
// as a value, or passed along, and it walks GenDecls as well as function bodies.
// Referring to a posture writer and not calling it is not a thing anyone does
// innocently, so the over-match costs nothing and closes the value-taking route.
//
// # What it asserts, and in which direction
//
// UNIVERSALLY over what it sees: every reference it finds must be allow-listed.
// It fails on anything unrecognised rather than looking for a site it likes and
// passing when it finds one. An existential check ("a correct call exists")
// passes with an incorrect one sitting beside it — a presence check wearing an
// execution check's clothes. The question here is whether an UNAUTHORISED writer
// exists, and only the universal form asks it.
//
// # What it cannot see
//
// Stated plainly, because an overstated guard is worse than an honest narrow one:
// it makes the next reader stop looking.
//
//   - IT CANNOT SEE WHAT AN ALLOW-LISTED SITE DOES. The allow-list is keyed to
//     {file, function, writer}, so a revoke written INSIDE one of the listed
//     functions is invisible — and that is the most natural place for somebody to
//     put a "channel is down, fall back" edit. No call-site guard can close this;
//     it is a limit of the technique, not a gap to be patched. The listed
//     functions are small and their arguments come from their launches, which is
//     mitigation, not defence.
//   - It matches by NAME, resolved syntactically rather than by type. A new
//     posture writer under a different name is invisible until it is added above.
//     Resolving to types.Object via go/packages would fix this and the name
//     collisions below; it was judged too heavy for what it buys here.
//   - It walks two directories, NOT two packages: `.` and `../session`, top level
//     only. agentd's own subpackages (dashboard/, dashsnap/, jstest/, starters/)
//     are not walked, and neither is any third package.
//   - It constrains references, not behaviour, so it says nothing about ROUTING.
//     A revoke that writes no record and instead teaches [copilotAPIDriven] to
//     consult the in-memory observation would be a revoke in every sense an
//     operator cares about, and this guard would be green. Prose on
//     [copilotAPIChannelFailed] is all that refuses that path.
func TestNoAutomaticRevokeOfTheCopilotAPIPosture(t *testing.T) {
	watched := map[string]bool{}
	for _, site := range copilotAPIPostureWriters {
		watched[site.writer] = true
	}

	type reference struct{ file, function, writer string }
	var found []reference

	record := func(file, function string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			switch typed := n.(type) {
			case *ast.SelectorExpr:
				if watched[typed.Sel.Name] {
					found = append(found, reference{file, function, typed.Sel.Name})
					// Do not descend: the selector's own X is not a reference.
					return false
				}
			case *ast.Ident:
				// A bare identifier, which is what a dot-import produces.
				if watched[typed.Name] {
					found = append(found, reference{file, function, typed.Name})
				}
			}
			return true
		})
	}

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
				switch typed := decl.(type) {
				case *ast.FuncDecl:
					if typed.Body != nil {
						record(name, typed.Name.Name, typed.Body)
					}
				case *ast.GenDecl:
					// Package-level vars and consts. A func literal living here
					// is invisible to a FuncDecl-only walk, and reads in a diff
					// exactly like any other code.
					record(name, packageLevel, typed)
				}
			}
		}
	}

	allowed := map[reference]bool{}
	for _, site := range copilotAPIPostureWriters {
		allowed[reference{site.file, site.function, site.writer}] = true
	}

	// The universal arm. Every reference found must be one the list vouches for.
	for _, site := range found {
		if allowed[site] {
			continue
		}
		// A rename of an allow-listed function is reported by the positive
		// control below with an accurate diagnosis. Saying "this is a REVOKE"
		// about it as well would be a false accusation over a refactor, so the
		// accusation is withheld when this writer's expected home has moved.
		var homeMoved bool
		for _, want := range copilotAPIPostureWriters {
			if want.writer != site.writer {
				continue
			}
			if !allowed[reference{want.file, want.function, want.writer}] {
				continue
			}
			var stillThere bool
			for _, f := range found {
				if f == (reference{want.file, want.function, want.writer}) {
					stillThere = true
				}
			}
			if !stillThere {
				homeMoved = true
			}
		}
		if homeMoved {
			continue
		}
		assert.Failf(t, "an unvouched-for reference to a Copilot posture writer",
			"%s references %s in %s, and writing a posture there is a REVOKE of "+
				"the agent's Copilot drive — the daemon does not do that. An agent "+
				"whose channel failed has not un-chosen the API drive; the remedy is "+
				"the operator's relaunch, which retries it. If this is deliberate, "+
				"the decision on copilotAPIChannelFailed has to be reopened first, "+
				"not worked around here: it is phase-dependent and may well be ready "+
				"to change, but it changes in the doc comment and in this list "+
				"together",
			site.function, site.writer, site.file)
	}

	// The positive control. Every assertion above is over a collected set, so a
	// rename or a move would empty it and leave the guard passing vacuously.
	for _, want := range copilotAPIPostureWriters {
		site := reference{want.file, want.function, want.writer}
		assert.Containsf(t, found, site,
			"%s no longer references %s in %s. Either that posture write is gone — "+
				"which is its own bug, since a conversation with no recorded posture "+
				"routes as send-keys — or it moved and this guard is now watching "+
				"nothing",
			want.function, want.writer, want.file)
	}
}
