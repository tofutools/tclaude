package agentd

import (
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// The flow tests assert at the Spawner boundary, which records SpawnArgs
// BEFORE argv construction — so they cannot see whether the argv actually
// carries the flag. These cover that last hop for both forked commands.
//
// The resume case is the one that regressed in review: `sessionResumeArgs` is
// what clone / reincarnate / dashboard-resume all fork through, and an argv
// that drops the flag silently reverts an operator's opt-in no matter how
// correctly the posture was resolved upstream.

func TestSessionArgs_AutoMemoryOmittedByDefault(t *testing.T) {
	cases := map[string][]string{
		"new":    sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}),
	}
	for name, args := range cases {
		if slices.Contains(args, "--auto-memory") {
			t.Fatalf("%s: the default posture must omit --auto-memory (the launch then injects the disable), got %v", name, args)
		}
	}
}

func TestSessionArgs_AutoMemoryCarriedWhenOptedIn(t *testing.T) {
	cases := map[string][]string{
		"new": sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x", AutoMemory: true}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Cwd: "/tmp/x", AutoMemory: true,
		}),
	}
	for name, args := range cases {
		if !slices.Contains(args, "--auto-memory") {
			t.Fatalf("%s: an opted-in launch must carry --auto-memory, got %v", name, args)
		}
	}
}

func TestSessionArgs_ContextWindowMaxCarried(t *testing.T) {
	const max = "272000"
	for name, args := range map[string][]string{
		"new": sessionNewArgs(clcommon.SpawnArgs{
			Label: "lbl", Cwd: "/tmp/x", ContextWindowMax: 272000,
		}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Cwd: "/tmp/x", ContextWindowMax: 272000,
		}),
	} {
		i := slices.Index(args, "--context-window-max")
		if i < 0 || i+1 >= len(args) || args[i+1] != max {
			t.Fatalf("%s: configured context max must carry its canonical token count, got %v", name, args)
		}
	}
	for name, args := range map[string][]string{
		"new":    sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}),
	} {
		if slices.Contains(args, "--context-window-max") {
			t.Fatalf("%s: unset context max must omit the flag, got %v", name, args)
		}
	}
}
