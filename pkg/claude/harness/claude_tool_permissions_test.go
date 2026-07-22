package harness

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A leaf deny with nothing reopened beneath it is the case the gap actually
// bit: the OS sandbox confined Bash while the built-in Read tool could still
// open the path. Both surfaces must now carry it.
func TestClaudeToolPermissionDenyMirrorsLeafDeny(t *testing.T) {
	rules, skipped := claudeToolPermissionDenyRules(nil, nil, []string{"/home/op/.ssh"}, nil)
	if len(skipped) != 0 {
		t.Fatalf("leaf deny should be mirrorable, skipped %v", skipped)
	}
	for _, want := range []string{"Read(//home/op/.ssh/**)", "Edit(//home/op/.ssh/**)"} {
		if !slices.Contains(rules, want) {
			t.Errorf("missing rule %q in %v", want, rules)
		}
	}
}

// The doubled leading slash is the whole ballgame: a single slash anchors at
// the settings source (for a --settings payload, the session cwd), so a
// single-slash rule would silently police the wrong subtree.
func TestClaudeToolPermissionPatternIsFilesystemAbsolute(t *testing.T) {
	got := claudeAbsolutePermissionPattern("/home/op/.aws")
	if got != "//home/op/.aws/**" {
		t.Fatalf("pattern = %q, want //home/op/.aws/**", got)
	}
	if strings.HasPrefix(got, "///") || !strings.HasPrefix(got, "//") {
		t.Fatalf("pattern %q must have exactly two leading slashes", got)
	}
}

// Claude Code evaluates deny before allow and ignores specificity, so a deny
// cannot carry allowlist exceptions. Mirroring a deny that has a reopen beneath
// it would deny the built-in tools the reopened path — including the agent's
// own workspace under a `deny ~` posture — with no way to carve it back out.
// Such a deny must be skipped, not emitted.
func TestClaudeToolPermissionDenySkipsReopenUnderDeny(t *testing.T) {
	rules, skipped := claudeToolPermissionDenyRules(
		[]string{"/home/op/go"},
		[]string{"/home/op/git/project"},
		[]string{"/home/op"}, nil,
	)
	if len(rules) != 0 {
		t.Fatalf("deny with reopens beneath must not be mirrored, got %v", rules)
	}
	if !slices.Contains(skipped, "/home/op") {
		t.Fatalf("skipped = %v, want it to name /home/op", skipped)
	}
}

// A profile can mix both shapes. The unmirrorable home deny must not suppress
// the leaf deny that is perfectly representable.
func TestClaudeToolPermissionDenyMixedShapes(t *testing.T) {
	rules, skipped := claudeToolPermissionDenyRules(
		[]string{"/home/op/go"},
		nil,
		[]string{"/home/op", "/etc/secrets"}, nil,
	)
	if !slices.Contains(rules, "Read(//etc/secrets/**)") {
		t.Errorf("representable leaf deny dropped; rules = %v", rules)
	}
	if slices.Contains(rules, "Read(//home/op/**)") {
		t.Errorf("home deny with a reopen beneath it must not be mirrored; rules = %v", rules)
	}
	if !slices.Contains(skipped, "/home/op") || slices.Contains(skipped, "/etc/secrets") {
		t.Errorf("skipped = %v, want only /home/op", skipped)
	}
}

// An acknowledged break-glass path has its OS-sandbox deny suppressed so the
// grant can take effect. Re-denying it on the tool surface would defeat the
// acknowledgement on exactly the tools an operator debugging tclaude needs.
func TestClaudeSettingsBreakGlassNotReDeniedOnToolSurface(t *testing.T) {
	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:               ClaudeSandboxOn,
		SandboxDenyDirs:           []string{"/home/op/.tclaude/data"},
		SandboxBreakGlassReadDirs: []string{"/home/op/.tclaude/data"},
	})
	if strings.Contains(settings, "Read(//home/op/.tclaude/data/**)") {
		t.Fatalf("acknowledged break-glass path re-denied on the tool surface: %s", settings)
	}
}

// The rendered payload must actually carry both surfaces, and the tool rules
// must land under permissions.deny where Claude Code reads them.
func TestClaudeSettingsRendersBothSurfaces(t *testing.T) {
	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:     ClaudeSandboxOn,
		SandboxDenyDirs: []string{"/home/op/.ssh"},
	})
	var decoded struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead []string `json:"denyRead"`
			} `json:"filesystem"`
		} `json:"sandbox"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(settings), &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", settings, err)
	}
	if !slices.Contains(decoded.Sandbox.Filesystem.DenyRead, "/home/op/.ssh") {
		t.Errorf("OS-sandbox deny missing: %s", settings)
	}
	if !slices.Contains(decoded.Permissions.Deny, "Read(//home/op/.ssh/**)") {
		t.Errorf("tool-permission deny missing: %s", settings)
	}
}

// Sandbox `off` is an explicit "unconfine this session". It already drops the
// OS-sandbox denies; the tool surface must not quietly re-impose them, or off
// would stop meaning off.
func TestClaudeSettingsSandboxOffEmitsNoToolDeny(t *testing.T) {
	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:     ClaudeSandboxOff,
		SandboxDenyDirs: []string{"/home/op/.ssh"},
	})
	if strings.Contains(settings, "permissions") {
		t.Fatalf("sandbox off must emit no tool-permission rules: %s", settings)
	}
}
