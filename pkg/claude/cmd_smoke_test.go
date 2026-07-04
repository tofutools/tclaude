package claude

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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

// TestCommandTreeHelpDoesNotPanic walks the entire cobra command/subcommand
// tree and runs `--help` at every node, asserting that none panics.
//
// TestCommandTreeConstructs already catches construction-time panics (e.g. the
// duplicate-shorthand collision that shipped in #783). This is the
// complementary, stronger guard: it exercises the real path a user hits —
// argument traversal, flag parsing, and help-template rendering at every node —
// so a panic that only surfaces while resolving or rendering a specific
// subcommand's help fails here instead of at the user's terminal. It is also
// the plain "the CLI answers `--help` everywhere without dying" contract.
//
// `--help` is side-effect-free: cobra returns flag.ErrHelp as soon as it sees
// the help flag (command.go, before any PersistentPreRun hook runs), so no
// node's RunFunc, config.Load, or logging setup is invoked — only the help
// template is rendered. Each node is its own subtest that recovers, so one run
// reports every broken node — named by its command path — rather than stopping
// at the first.
func TestCommandTreeHelpDoesNotPanic(t *testing.T) {
	var root *cobra.Command
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("building the command tree panicked: %v", r)
			}
		}()
		root = Cmd()
	}()

	// Collect every node's argument path: the subcommand names from the root
	// down (the root itself is the empty path). Snapshot before executing —
	// cobra lazily grafts its own `help`/`completion` commands onto the tree on
	// the first Execute, and we only want to walk tclaude's real commands.
	var paths [][]string
	var collect func(cmd *cobra.Command, path []string)
	collect = func(cmd *cobra.Command, path []string) {
		paths = append(paths, append([]string(nil), path...))
		for _, child := range cmd.Commands() {
			collect(child, append(append([]string(nil), path...), child.Name()))
		}
	}
	collect(root, nil)

	// One subtest per node, so a failure names the exact command path (in `go
	// test` output, IDE test trees, and CI annotations) instead of hiding in an
	// aggregated list. Subtests run sequentially and a failing one doesn't abort
	// its siblings, so a single run still reports every broken node.
	for _, path := range paths {
		name := "root"
		if len(path) > 0 {
			name = strings.Join(path, "/")
		}
		t.Run(name, func(t *testing.T) {
			cmdline := "claude " + strings.Join(path, " ") + " --help"
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("`%s` panicked: %v", cmdline, r)
				}
			}()
			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(append(append([]string(nil), path...), "--help"))
			// The side-effect-free property above holds only while every node
			// parses flags normally: cobra reads the help flag via GetBool, so a
			// node with DisableFlagParsing=true would fall through the ErrHelp
			// short-circuit into PersistentPreRun (and its RunFunc). No node sets
			// that today; if one ever does, gate it out of this walk.
			//
			// A returned error is not itself a failure (--help yields none); we
			// assert only that Execute renders help without panicking.
			_ = root.Execute()
			if buf.Len() == 0 {
				t.Fatalf("`%s` rendered no output", cmdline)
			}
		})
	}
}
