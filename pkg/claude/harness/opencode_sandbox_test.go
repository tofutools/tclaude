package harness

import (
	"strings"
	"testing"
)

func TestOpenCodeSandboxWarnings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		wantWarning bool
	}{{
		name: "access-control looks like a sandbox but is not, so it warns",
		mode: OpenCodeSandboxAccessControl, wantWarning: true,
	}, {
		// A blank spawn resolves to access-control (the DefaultMode), which is
		// exactly the posture the warning must reach — the mode a user gets
		// without choosing one.
		name: "the default mode is access-control and warns",
		mode: openCodeSandbox{}.DefaultMode(), wantWarning: true,
	}, {
		// off already carries its own ⚠ in ModeHelp and is an explicit opt-out,
		// so there is no false sense of security to correct.
		name: "off does not warn",
		mode: OpenCodeSandboxOff, wantWarning: false,
	}, {
		name: "unknown mode does not warn",
		mode: "danger-full-access", wantWarning: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := openCodeSandboxWarnings(tc.mode)
			if tc.wantWarning != (len(got) > 0) {
				t.Fatalf("got %v, wantWarning=%v", got, tc.wantWarning)
			}
			if !tc.wantWarning {
				return
			}
			// The warning must name what is not confined and be honest that no
			// real fix is available yet — the whole point of the line.
			line := got[0]
			for _, want := range []string{"⚠", "no built-in OS sandbox", "access-control", "unsandboxed"} {
				if !strings.Contains(line, want) {
					t.Fatalf("warning %q missing %q", line, want)
				}
			}
		})
	}
}

// SpawnSandboxWarnings is the harness-neutral entry point. It must dispatch to
// the OpenCode check for OpenCode, fall through to the Claude TCL-586 check for
// Claude Code, and stay silent for Codex and a nil harness.
func TestSpawnSandboxWarningsDispatch(t *testing.T) {
	opencode, ok := Get(OpenCodeName)
	if !ok {
		t.Fatalf("harness %q is not registered", OpenCodeName)
	}
	claude, ok := Get(DefaultName)
	if !ok {
		t.Fatalf("harness %q is not registered", DefaultName)
	}
	codex, ok := Get(CodexName)
	if !ok {
		t.Fatalf("harness %q is not registered", CodexName)
	}

	if got := SpawnSandboxWarnings(nil, "", OpenCodeSandboxAccessControl, ""); got != nil {
		t.Fatalf("nil harness: got %v, want nil", got)
	}

	// OpenCode routes to the access-control warning regardless of the approval
	// argument (which is a Claude-only input).
	got := SpawnSandboxWarnings(opencode, "", OpenCodeSandboxAccessControl, "")
	if len(got) == 0 || !strings.Contains(got[0], "OpenCode has no built-in OS sandbox") {
		t.Fatalf("opencode access-control: got %v, want the OpenCode warning", got)
	}
	if got := SpawnSandboxWarnings(opencode, "", OpenCodeSandboxOff, ""); got != nil {
		t.Fatalf("opencode off: got %v, want nil", got)
	}

	// Claude still reaches its own TCL-586 check (the auto + inherit default with
	// no settings file that enables the sandbox is the canonical warning case).
	home, _ := isolateClaudeSettings(t)
	got = SpawnSandboxWarnings(claude, claudePermAuto, ClaudeSandboxInherit, home)
	if len(got) == 0 || !strings.Contains(got[0], "OS sandbox") {
		t.Fatalf("claude auto+inherit: got %v, want the TCL-586 warning", got)
	}
	// And the Claude branch must NOT emit the OpenCode line.
	if strings.Contains(got[0], "OpenCode has no built-in OS sandbox") {
		t.Fatalf("claude branch leaked the OpenCode warning: %q", got[0])
	}

	// Codex resolves autonomy and sandbox together, so no gap and no warning.
	if got := SpawnSandboxWarnings(codex, ApprovalNever, SandboxManagedProfile, ""); got != nil {
		t.Fatalf("codex: got %v, want nil", got)
	}
}
