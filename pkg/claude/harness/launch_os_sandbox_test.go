package harness

import (
	"path/filepath"
	"strings"
	"testing"
)

// The case TCL-729 is about: a Claude agent spawned under the DEFAULT `inherit`
// mode, confined by the operator's own settings.json. The recorded mode says
// nothing, so the launch boundary must record the resolved verdict — and name
// the file that decided it, which is the operator's answer to "why is this on?".
func TestResolveLaunchOSSandboxRecordsInheritedOn(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, "", home)
	if got.State != "on" {
		t.Fatalf("inherited sandbox: got state %q, want %q", got.State, "on")
	}
	if got.Source == "" {
		t.Fatal("inherited sandbox: want the deciding settings file named, got no source")
	}
}

// An `inherit` launch that nothing confines resolves to "unconfigured", NOT to
// "off": no file disabled the sandbox, nothing enabled it. The distinction is
// what lets the operator-facing copy say "nothing configures it" instead of
// claiming a file turned it off.
func TestResolveLaunchOSSandboxRecordsUnconfigured(t *testing.T) {
	home, _ := isolateClaudeSettings(t)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, "", home)
	if got.State != "unconfigured" {
		t.Fatalf("unconfigured sandbox: got state %q, want %q", got.State, "unconfigured")
	}
	if got.Source != "" {
		t.Fatalf("unconfigured sandbox: want no deciding source, got %q", got.Source)
	}
}

// An explicit `on` that enterprise managed policy overrides is the case an
// operator is most likely to get wrong: they chose the sandbox and did not get
// it. Recording the real verdict (not the request) is what lets a read surface
// contradict the mode.
func TestResolveLaunchOSSandboxRecordsManagedPolicyOverride(t *testing.T) {
	_, managed := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(managed, "managed-settings.json"), `{"sandbox":{"enabled":false}}`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, "", "")
	if got.State != "off" {
		t.Fatalf("managed override: got state %q, want %q", got.State, "off")
	}
	if got.Source == "" {
		t.Fatal("managed override: want the managed policy file named, got no source")
	}
}

// A harness whose recorded mode already IS its posture records nothing, so its
// badge keeps rendering off the mode exactly as before. Resolving Claude's
// settings.json for one of them would be worse than useless: it would report
// `on` from the operator's own ~/.claude config for an agent that never reads
// that file.
func TestResolveLaunchOSSandboxRecordsNothingForOtherHarnesses(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	for _, name := range []string{CodexName, OpenCodeName} {
		h, err := Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if got := ResolveLaunchOSSandbox(h, "", "", home); got != (LaunchOSSandbox{}) {
			t.Fatalf("%s: want no recorded verdict, got %+v", name, got)
		}
	}
	if got := ResolveLaunchOSSandbox(nil, "", "", home); got != (LaunchOSSandbox{}) {
		t.Fatalf("nil harness: want no recorded verdict, got %+v", got)
	}
}

// A settings file that OUTRANKS the deciding tier but cannot be parsed must be
// recorded as doubt. ResolveClaudeSandboxEnabled treats such a file as "this
// tier says nothing" and walks on, which is right for resolution but would make
// the recorded verdict a bare assertion of containment that a policy tclaude
// never read could contradict.
func TestResolveLaunchOSSandboxMarksAnUnreadableHigherTierUnverified(t *testing.T) {
	home, managed := isolateClaudeSettings(t)
	// Managed policy outranks even an explicit launch flag, so an unparseable
	// one leaves a sandbox-`on` launch genuinely unproven.
	writeSettings(t, filepath.Join(managed, "managed-settings.json"), `{"sandbox":`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, "", home)
	if got.State != "on" {
		t.Fatalf("unreadable managed policy: got state %q, want %q", got.State, "on")
	}
	if !got.Unverified {
		t.Fatal("an unparseable managed policy outranks this launch — the verdict must be marked unverified")
	}
}

// The same for the inherit path: a project tier that cannot be parsed sits above
// the user tier that answers, so the answer is unproven.
func TestResolveLaunchOSSandboxMarksAnUnreadableProjectTierUnverified(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	project := filepath.Join(home, "proj")
	writeSettings(t, filepath.Join(project, ".claude", "settings.json"), `{oops`)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, "", project)
	if got.State != "on" {
		t.Fatalf("unreadable project tier: got state %q, want %q", got.State, "on")
	}
	if !got.Unverified {
		t.Fatal("an unparseable higher-precedence project file must mark the verdict unverified")
	}
}

// A fully readable chain records no doubt — otherwise every ordinary launch
// would wear the hedge and the signal would mean nothing.
func TestResolveLaunchOSSandboxIsVerifiedWhenEveryTierIsReadable(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	if got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, "", home); got.Unverified {
		t.Fatalf("a readable settings chain must not be marked unverified: %+v", got)
	}
}

// The hedge must not fire for a diagnostic BELOW the tier that decided: if
// managed policy answers cleanly, nothing under it can overturn the verdict, and
// marking it unverified would be a false hedge that trains operators to ignore
// the real one.
//
// The resolver returns the moment a tier decides, so a lower tier is never even
// read — this pins that early-return property, which is what makes
// "len(Diagnostics) > 0" mean "a HIGHER-precedence file was unreadable" rather
// than merely "some file was unreadable".
func TestResolveLaunchOSSandboxDoesNotHedgeOnALowerTier(t *testing.T) {
	home, managed := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(managed, "managed-settings.json"), `{"sandbox":{"enabled":true}}`)
	// Garbage in every tier BELOW the managed policy that just answered.
	project := filepath.Join(home, "proj")
	writeSettings(t, filepath.Join(project, ".claude", "settings.json"), `{oops`)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{also oops`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, "", project)
	if got.State != "on" {
		t.Fatalf("managed policy decides: got state %q, want %q", got.State, "on")
	}
	if got.Unverified {
		t.Fatal("a decided verdict must not be hedged by unreadable files that rank BELOW the decider")
	}
}

// The gap this closes: `sandbox: on` can be chosen by an explicit flag OR by a
// spawn profile the operator never looked at (a named profile, their group's
// default, the global default). The verdict is identical either way, so a
// recorded source of "this launch" credited the containment to whoever is
// reading the badge — precisely the person who did not choose it.
func TestResolveLaunchOSSandboxNamesTheProfileThatChoseTheMode(t *testing.T) {
	home, _ := isolateClaudeSettings(t)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, `global default profile "agents"`, home)
	if got.State != "on" {
		t.Fatalf("forced on: got state %q, want %q", got.State, "on")
	}
	want := "global default profile \"agents\" (sandbox `on`)"
	if got.Source != want {
		t.Fatalf("attributed source: got %q, want %q", got.Source, want)
	}
}

// An explicit choice keeps "this launch": it IS this launch, and swapping in
// the word "explicit" would add a token without adding a fact.
func TestResolveLaunchOSSandboxLeavesAnExplicitChoiceUnattributed(t *testing.T) {
	home, _ := isolateClaudeSettings(t)

	explicit := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, SandboxChosenExplicitly, home)
	none := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, "", home)
	if explicit.Source != none.Source {
		t.Fatalf("explicit attribution changed the source: got %q, want %q", explicit.Source, none.Source)
	}
	if explicit.Source != "this launch (sandbox `on`)" {
		t.Fatalf("explicit source: got %q", explicit.Source)
	}
}

// A verdict the LAUNCH did not decide is left alone. Who chose the mode did not
// affect the outcome there, and crediting a spawn profile with a settings
// file's decision would be a fresh false attribution in place of the old one.
func TestResolveLaunchOSSandboxDoesNotAttributeASettingsDecidedVerdict(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, `global default profile "agents"`, home)
	if got.State != "on" {
		t.Fatalf("inherited: got state %q, want on", got.State)
	}
	if strings.Contains(got.Source, "agents") {
		t.Fatalf("a settings-decided verdict must not name the mode's chooser: %q", got.Source)
	}
}

// The attribution embeds an operator-authored profile NAME, and it is
// persisted, logged, and rendered. Control characters would forge line
// structure in a log; an unbounded name would ride into all three.
func TestSanitizeSandboxChosenBy(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"trims", "  explicit  ", "explicit"},
		{"collapses newlines a log would read as structure", "profile \"a\nb\"", `profile "a b"`},
		{"drops control characters", "profile \"a\x00\x07b\"", `profile "a b"`},
		{"leaves an ordinary name alone", `group default profile "team-x"`, `group default profile "team-x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSandboxChosenBy(tc.in); got != tc.want {
				t.Fatalf("sanitize(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	long := sanitizeSandboxChosenBy(`profile "` + strings.Repeat("x", 400) + `"`)
	if len([]rune(long)) != maxSandboxChosenByLen+1 {
		t.Fatalf("bounded label: got %d runes, want %d", len([]rune(long)), maxSandboxChosenByLen+1)
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatal("a clipped label must be marked, so it never reads as a complete name")
	}
}
