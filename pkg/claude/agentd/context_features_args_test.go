package agentd

import (
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The flow tests assert at the Spawner boundary, which records SpawnArgs BEFORE
// argv construction — so they cannot see whether the argv actually carries the
// flag. These cover that last hop for both forked commands.
//
// The resume case is the load-bearing one: `sessionResumeArgs` is what clone /
// reincarnate / dashboard-resume all fork through, so an argv that drops the flag
// silently un-trims the agent no matter how correctly the map was resolved
// upstream.

// flagValue returns the argument following name, or "" when name is absent.
func flagValue(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestSessionArgs_ContextFeaturesOmittedByDefault(t *testing.T) {
	cases := map[string][]string{
		"new":    sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}),
	}
	for name, args := range cases {
		if slices.Contains(args, "--context-features") {
			t.Fatalf("%s: an untrimmed launch must omit --context-features entirely, got %v", name, args)
		}
	}
	// An explicitly empty map is the same argv as nil here. There is no flag
	// spelling for "trim nothing" that differs from omitting it, because the
	// forked `session new` has no profile tier stack of its own to override — the
	// daemon already resolved the answer.
	empty := sessionNewArgs(clcommon.SpawnArgs{
		Label: "lbl", Cwd: "/tmp/x", ContextFeatures: map[string]string{},
	})
	if slices.Contains(empty, "--context-features") {
		t.Fatalf("an empty trim map must omit the flag, got %v", empty)
	}
}

func TestSessionArgs_ContextFeaturesCarried(t *testing.T) {
	trims := map[string]string{"bundled-skills": "off", "artifact": "on"}
	cases := map[string][]string{
		"new": sessionNewArgs(clcommon.SpawnArgs{
			Label: "lbl", Cwd: "/tmp/x", ContextFeatures: trims,
		}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Cwd: "/tmp/x", ContextFeatures: trims,
		}),
	}
	for name, args := range cases {
		got := flagValue(args, "--context-features")
		if got == "" {
			t.Fatalf("%s: a trimmed launch must carry --context-features, got %v", name, args)
		}
		// The whole map rides as ONE argv element. exec.Command takes a slice with
		// no shell in between, so the ", " separator is safe — but it must not be
		// split across arguments, or the forked flag parse would see only the first
		// pair.
		if want := "artifact=on, bundled-skills=off"; got != want {
			t.Errorf("%s: --context-features value = %q, want %q (one argv element, sorted)",
				name, got, want)
		}
		// And the forked `session new` must be able to parse exactly what it got.
		parsed, err := harness.ParseContextFeatures(got)
		if err != nil {
			t.Fatalf("%s: the emitted value must round-trip through ParseContextFeatures: %v", name, err)
		}
		if len(parsed) != len(trims) {
			t.Errorf("%s: round-trip lost entries: %v", name, parsed)
		}
		for slug, state := range trims {
			if parsed[slug] != state {
				t.Errorf("%s: round-trip changed %s: got %q want %q", name, slug, parsed[slug], state)
			}
		}
	}
}
