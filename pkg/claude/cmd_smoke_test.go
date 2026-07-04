package claude

import "testing"

// TestCommandTreeConstructs builds the entire cobra command tree and asserts it
// does not panic.
//
// boa's short-flag enricher assigns each flag the first free single-letter
// short in struct-field order. A flag with no explicit short, declared BEFORE
// another flag whose EXPLICIT short is the same letter, therefore steals that
// letter — and pflag panics on the duplicate shorthand when the second flag
// registers, at command-construction time. That panic aborts EVERY tclaude
// invocation (main → Cmd() builds the whole tree), yet it is invisible to the
// rest of the suite because no other test constructs the tree — a real
// regression (a `--task` declared before `--timeout -t`) shipped to main behind
// green CI exactly this way.
//
// This test closes that gap cheaply: constructing Cmd() walks every
// subcommand's flag registration, so any duplicate-shorthand collision — or any
// other construction-time panic — fails here instead of at the user's terminal.
func TestCommandTreeConstructs(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("building the command tree panicked (duplicate flag shorthand?): %v", r)
		}
	}()
	if Cmd() == nil {
		t.Fatal("Cmd() returned nil")
	}
}
