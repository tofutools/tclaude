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
// grant can take effect. A profile deny that is an ANCESTOR of it (the only
// shape possible — ordinary denies cannot name a protected path) must not be
// mirrored, or the mirrored Read deny would re-cover the acknowledged path and
// defeat the acknowledgement on exactly the tools an operator debugging tclaude
// needs. Workspace deliberately outside the denied home so no ordinary reopen
// also marks the deny — the break-glass reopen is what must carry it.
func TestClaudeToolPermissionDenyBreakGlassAncestorSkipped(t *testing.T) {
	rules, skipped := claudeToolPermissionDenyRules(
		nil, nil,
		[]string{"/home/op"},
		[]string{"/home/op/.tclaude/data"},
	)
	if slices.Contains(rules, "Read(//home/op/**)") {
		t.Fatalf("ancestor deny of a break-glass path must not be mirrored; rules = %v", rules)
	}
	if !slices.Contains(skipped, "/home/op") {
		t.Fatalf("skipped = %v, want it to name /home/op", skipped)
	}
}

// End-to-end through the renderer: the same shape must not surface a tool-deny.
func TestClaudeSettingsBreakGlassAncestorNotReDenied(t *testing.T) {
	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:               ClaudeSandboxOn,
		SandboxDenyDirs:           []string{"/home/op"},
		SandboxReadDirs:           []string{"/srv/repo"},
		SandboxBreakGlassReadDirs: []string{"/home/op/.tclaude/data"},
	})
	if strings.Contains(settings, "Read(//home/op/**)") {
		t.Fatalf("break-glass ancestor re-denied on the tool surface: %s", settings)
	}
}

// A directory name containing gitignore metacharacters must be escaped, or the
// mirrored rule matches the wrong paths and silently leaves the built-in tools
// un-denied for the literal directory Bash IS confined to — the exact bug this
// change closes, reintroduced.
func TestClaudeToolPermissionPatternEscapesGlobs(t *testing.T) {
	got := claudeAbsolutePermissionPattern("/home/op/we[ir]d")
	want := `//home/op/we\[ir\]d/**`
	if got != want {
		t.Fatalf("pattern = %q, want %q", got, want)
	}
	// The trailing recursive glob must stay live.
	if !strings.HasSuffix(got, "/**") {
		t.Fatalf("pattern %q lost its recursive suffix", got)
	}
	// A star inside the path must be escaped, not left as an operator.
	if star := claudeAbsolutePermissionPattern("/home/op/a*b"); star != `//home/op/a\*b/**` {
		t.Fatalf("star not escaped: %q", star)
	}
}

// A root deny must not degenerate to the brittle `///**` (which is prone to a
// known Claude Code root-pattern edge case); it must be the documented
// whole-filesystem `//**`. Root is skipped in practice — the workspace reopen
// always sits beneath it — but the pattern builder must still be correct.
func TestClaudeToolPermissionPatternRoot(t *testing.T) {
	if got := claudeAbsolutePermissionPattern("/"); got != "//**" {
		t.Fatalf("root pattern = %q, want //**", got)
	}
}

// inherit emits the OS-sandbox denies (inert unless the operator's own settings
// enable the sandbox) AND the tool-permission denies. The tool layer is
// independent of sandbox.enabled by design, so a profile's `deny ~/.ssh` should
// bind the built-in Read/Edit tools under inherit exactly as under on — the
// whole point of the fix. permissions.deny is monotonic (it only restricts), so
// honoring an explicit operator deny on both surfaces cannot make the session
// less safe, and does not touch whether the sandbox is enabled — which is all
// inherit promises to leave alone. Under inherit the reopen-under-deny gate
// already refuses any launch whose profile reopens beneath a deny, so only leaf
// denies reach here.
func TestClaudeSettingsInheritMirrorsLeafDeny(t *testing.T) {
	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:     ClaudeSandboxInherit,
		SandboxDenyDirs: []string{"/home/op/.ssh"},
	})
	if !strings.Contains(settings, `Read(//home/op/.ssh/**)`) {
		t.Fatalf("inherit did not mirror the leaf deny to the tool surface: %s", settings)
	}
	// inherit must not force the sandbox on — that is the part of the operator's
	// config it promises not to change.
	if strings.Contains(settings, `"enabled":true`) {
		t.Fatalf("inherit forced sandbox enabled: %s", settings)
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
