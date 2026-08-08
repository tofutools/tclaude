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
	// The OPERATOR's durable drive switch (TCL-1082), which is categorically not
	// the write this guard forbids and does not reopen the decision on
	// [copilotAPIChannelFailed]. That decision is about the DAEMON revoking from
	// an OBSERVATION — a channel that failed to come up is not an agent that
	// un-chose the drive, and weather must not withdraw a person's opt-out. These
	// two writes have the opposite author: a person, through an authoring surface
	// built for the purpose, arriving as a decision rather than as evidence. The
	// daemon still never demotes an agent by itself.
	//
	// Both sites also do what that decision's own closing sentence asks of anyone
	// building a durable revoke — reach for the agent profile DELIBERATELY, or not
	// at all. The agent profile is written when a stable agent exists, because it
	// is what routes; the conversation fallback is written only when there is no
	// agent row at all (a clone, or a direct `session new`), which is the one
	// shape where it is the sole holder rather than the inert-or-surprising record
	// described there.
	// SeedAgentRelaunchProfileIfEmpty is WATCHED as well as vouched. It is a new
	// posture writer under a new name, and this guard's own limits say a writer it
	// does not name is invisible to it — so adding the call site without adding
	// the writer would have left the next one free. It writes a whole profile, so
	// it can set a posture the same way SetAgentRelaunchProfile can.
	{"copilot_drive_assign.go", "writeCopilotDrive", "SeedAgentRelaunchProfileIfEmpty"},
	{"copilot_drive_assign.go", "writeCopilotDrive", "SetConversationCopilotAPI"},
	// The two compare-and-set writers, watched for the reason stated just above
	// and then not applied far enough. A targeted `json_set` of `$.copilot_api` is
	// a posture write in every sense this guard cares about, and it is now THE
	// SHORTEST WAY TO WRITE ONE — shorter than the whole-blob route the list
	// already watched. A daemon-authored "the channel never came up, stop
	// pretending" edit would be a single call to one of these in a bootstrap
	// failure path, with CI green, because a writer this guard does not name is
	// invisible to it. A guard that watches the long way into a sink and not the
	// short way converts "nobody checked" into "something checked and was
	// satisfied", which is worse than no guard.
	//
	// Found by a cold reviewer with a two-arm probe: a package-level reference to
	// a WATCHED writer reddened and named its site, while the same reference to
	// CompareAndSetAgentCopilotAPI passed silently.
	//
	// AND THE BLIND SPOT GREW WHEN THEY WERE ADDED. The vouch is keyed to
	// {file, function, writer}, so the second CompareAndSetAgentCopilotAPI call
	// site inside writeCopilotDrive is covered for free by the entry written for
	// the first — and any future one would be too. That is the "cannot see what an
	// allow-listed site DOES" limit below, now applied to two more writers inside
	// the same function. There is no better option with a syntactic guard and the
	// function is small, so this is a cost of the fix rather than an objection to
	// it; it is written down so the next reader inherits the date rather than
	// rediscovering the shape.
	{"copilot_drive_assign.go", "writeCopilotDrive", "CompareAndSetAgentCopilotAPI"},
	{"copilot_drive_assign.go", "writeCopilotDrive", "CompareAndSetConversationCopilotAPI"},
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
// The rule is about the AUTHOR of the write, not about the write. Since TCL-1082
// an operator can turn the drive off durably through `tclaude agent set-drive`,
// and those writes are allow-listed above without weakening anything here: the
// sentence this test enforces is that the daemon never demotes an agent BY
// ITSELF. A person deciding to is the remedy that decision assumes exists, not an
// exception to it. So a new entry is still the wrong move for anything the daemon
// concludes on its own — the question to ask of a candidate entry is who authored
// the change, not whether the code path looks similar.
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
//     collisions below; it was judged too heavy for what it buys here. THIS LIMIT
//     HAS ALREADY BITTEN ONCE: the two compare-and-set writers were added to the
//     package and referenced three times before anyone added them here, in the
//     same change that wrote the paragraph above explaining why that must not
//     happen. Treat "a new writer needs a new entry" as a step in adding one, not
//     as advice.
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
			// REPORTED, not accused. The suppression above is per-WRITER, not
			// per-SITE, so one renamed home silences every unvouched reference to
			// that writer — including a genuine revoke sitting beside the rename.
			// Measured by a cold reviewer: with a home unfindable AND an unvouched
			// package-level reference present, the run produced exactly one red
			// ("the list is stale") and no mention of the reference at all. Red
			// count true, red-to-subject mapping wrong.
			//
			// Withholding the ACCUSATION is still right — calling a refactor a
			// revoke is a false accusation — but withholding the FACT is not.
			// Correct per-site scoping is TCL-1116; this is the half that removes
			// the silence.
			assert.Failf(t, "an unvouched reference this guard is not accusing",
				"%s references %s in %s, and no allow-list entry covers it. The "+
					"accusation is withheld because %s's vouched home is itself "+
					"missing, which usually means a rename rather than a revoke — but "+
					"NOT ALWAYS, and this guard cannot tell the two apart. Re-vouch the "+
					"writer's new home, re-run, and read what this says then: if the "+
					"reference is still here it is unexplained by any refactor",
				site.function, site.writer, site.file, site.writer)
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
