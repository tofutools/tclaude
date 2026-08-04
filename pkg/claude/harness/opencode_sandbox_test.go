package harness

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
			line := got[0]
			for _, want := range []string{"⚠", "no built-in OS sandbox", "access-control", "unsandboxed"} {
				if !strings.Contains(line, want) {
					t.Fatalf("warning %q missing %q", line, want)
				}
			}
		})
	}
}

func TestOpenCodeSandboxInfo(t *testing.T) {
	got := openCodeSandboxInfo(OpenCodeSandboxTclaudeLayer)
	if len(got) != 1 {
		t.Fatalf("got %v, want one informational boundary disclosure", got)
	}
	for _, want := range []string{
		"tool-executing server",
		"tclaude's built-in OS sandbox",
		"attach pane",
		"authenticated local control connection",
	} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("info %q missing %q", got[0], want)
		}
	}
	if strings.Contains(got[0], "tclaude-layer") {
		t.Fatalf("info %q leaks the internal implementation token", got[0])
	}
	for _, mode := range []string{OpenCodeSandboxAccessControl, OpenCodeSandboxOff, ""} {
		if got := openCodeSandboxInfo(mode); got != nil {
			t.Fatalf("mode %q: got info %v, want nil", mode, got)
		}
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

	if got := SpawnSandboxWarnings(nil, "", OpenCodeSandboxAccessControl, "", false); got != nil {
		t.Fatalf("nil harness: got %v, want nil", got)
	}

	// OpenCode routes to the access-control warning regardless of the approval
	// argument (which is a Claude-only input).
	got := SpawnSandboxWarnings(opencode, "", OpenCodeSandboxAccessControl, "", false)
	if len(got) == 0 || !strings.Contains(got[0], "OpenCode has no built-in OS sandbox") {
		t.Fatalf("opencode access-control: got %v, want the OpenCode warning", got)
	}
	if got := SpawnSandboxWarnings(opencode, "", OpenCodeSandboxOff, "", false); got != nil {
		t.Fatalf("opencode off: got %v, want nil", got)
	}
	info := SpawnSandboxInfo(opencode, OpenCodeSandboxTclaudeLayer)
	if len(info) != 1 || !strings.Contains(info[0], "built-in OS sandbox") {
		t.Fatalf("opencode tclaude-layer info: got %v, want the executor-boundary disclosure", info)
	}
	if got := SpawnSandboxInfo(claude, ClaudeSandboxInherit); got != nil {
		t.Fatalf("claude info: got %v, want nil", got)
	}

	// Claude still reaches its own TCL-586 check (the auto + inherit default with
	// no settings file that enables the sandbox is the canonical warning case).
	home, _ := isolateClaudeSettings(t)
	got = SpawnSandboxWarnings(claude, claudePermAuto, ClaudeSandboxInherit, home, false)
	if len(got) == 0 || !strings.Contains(got[0], "OS sandbox") {
		t.Fatalf("claude auto+inherit: got %v, want the TCL-586 warning", got)
	}
	// And the Claude branch must NOT emit the OpenCode line.
	if strings.Contains(got[0], "OpenCode has no built-in OS sandbox") {
		t.Fatalf("claude branch leaked the OpenCode warning: %q", got[0])
	}

	// Codex resolves autonomy and sandbox together, so no gap and no warning.
	if got := SpawnSandboxWarnings(codex, ApprovalNever, SandboxManagedProfile, "", false); got != nil {
		t.Fatalf("codex: got %v, want nil", got)
	}

	// Copilot routes to its own check, and must NOT fall through to Claude's:
	// UnsandboxedAutonomyWarnings would walk the operator's ~/.claude settings
	// for an agent that never reads them.
	copilot, ok := Get(CopilotName)
	if !ok {
		t.Fatalf("harness %q is not registered", CopilotName)
	}
	got = SpawnSandboxWarnings(copilot, CopilotApprovalYolo, CopilotSandboxInherit, home, false)
	if len(got) != 1 || !strings.Contains(got[0], "tclaude-layer") {
		t.Fatalf("copilot yolo without the outer layer: got %v, want the yolo warning", got)
	}
	if got := SpawnSandboxWarnings(copilot, CopilotApprovalYolo, CopilotSandboxOff, home, true); got != nil {
		t.Fatalf("copilot yolo under the outer layer: got %v, want nil", got)
	}
	if got := SpawnSandboxWarnings(copilot, CopilotApprovalAllowTools, CopilotSandboxInherit, home, false); got != nil {
		t.Fatalf("copilot allow-tools: got %v, want nil", got)
	}
}

func TestResolveOpenCodeSandboxImplementationMode(t *testing.T) {
	for _, mode := range []string{"", OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer} {
		got, err := ResolveOpenCodeSandboxImplementationMode(
			OpenCodeName, mode, sandboxpolicy.ImplementationTclaudeLayer)
		if err != nil || got != OpenCodeSandboxTclaudeLayer {
			t.Fatalf("outer layer + %q = %q, %v; want %q", mode, got, err, OpenCodeSandboxTclaudeLayer)
		}
	}
	if _, err := ResolveOpenCodeSandboxImplementationMode(
		OpenCodeName, OpenCodeSandboxOff, sandboxpolicy.ImplementationTclaudeLayer); err == nil {
		t.Fatal("outer layer + off must fail")
	}
	if _, err := ResolveOpenCodeSandboxImplementationMode(
		OpenCodeName, OpenCodeSandboxTclaudeLayer, sandboxpolicy.ImplementationHarnessBuiltin); err == nil {
		t.Fatal("tclaude-layer mode without the outer implementation must fail")
	}
}
