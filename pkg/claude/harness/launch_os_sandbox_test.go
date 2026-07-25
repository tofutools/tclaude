package harness

import (
	"path/filepath"
	"testing"
)

// The case TCL-729 is about: a Claude agent spawned under the DEFAULT `inherit`
// mode, confined by the operator's own settings.json. The recorded mode says
// nothing, so the launch boundary must record the resolved verdict — and name
// the file that decided it, which is the operator's answer to "why is this on?".
func TestResolveLaunchOSSandboxRecordsInheritedOn(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, home)
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

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxInherit, home)
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

	got := ResolveLaunchOSSandbox(Default(), ClaudeSandboxOn, "")
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
		if got := ResolveLaunchOSSandbox(h, "", home); got != (LaunchOSSandbox{}) {
			t.Fatalf("%s: want no recorded verdict, got %+v", name, got)
		}
	}
	if got := ResolveLaunchOSSandbox(nil, "", home); got != (LaunchOSSandbox{}) {
		t.Fatalf("nil harness: want no recorded verdict, got %+v", got)
	}
}
