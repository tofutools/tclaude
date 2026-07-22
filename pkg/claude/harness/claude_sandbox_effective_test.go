package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateClaudeSettings points both machine-global settings roots at temp dirs
// so a test never reads (or is influenced by) the real host's Claude Code
// configuration. It returns the fake home and the fake managed-settings root.
func isolateClaudeSettings(t *testing.T) (home, managed string) {
	t.Helper()
	home = t.TempDir()
	managed = t.TempDir()
	t.Setenv("HOME", home)
	previous := claudeManagedSettingsRoot
	claudeManagedSettingsRoot = func() string { return managed }
	t.Cleanup(func() { claudeManagedSettingsRoot = previous })
	return home, managed
}

func writeSettings(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveClaudeSandboxEnabledExplicitModesSkipSettings(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	// A user file saying the opposite must not win: `on`/`off` emit a
	// `--settings` block, which outranks every user/project file.
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":false}}`)

	if got := ResolveClaudeSandboxEnabled(ClaudeSandboxOn, home); got.State != ClaudeSandboxStateOn {
		t.Fatalf("sandbox on: got state %v, want on", got.State)
	}
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)
	if got := ResolveClaudeSandboxEnabled(ClaudeSandboxOff, home); got.State != ClaudeSandboxStateOff {
		t.Fatalf("sandbox off: got state %v, want off", got.State)
	}
}

func TestResolveClaudeSandboxEnabledInheritReadsPrecedenceChain(t *testing.T) {
	home, managed := isolateClaudeSettings(t)
	project := filepath.Join(home, "work", "repo")
	if err := os.MkdirAll(filepath.Join(project, "sub", "dir"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	// Nothing configured anywhere: not disabled by a file, just never enabled.
	got := ResolveClaudeSandboxEnabled(ClaudeSandboxInherit, project)
	if got.State != ClaudeSandboxStateUnknown || got.Source != "" {
		t.Fatalf("empty chain: got %v / %q, want unconfigured / \"\"", got.State, got.Source)
	}

	// The user file is the weakest tier, but it is still an answer.
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)
	if got := ResolveClaudeSandboxEnabled("", project); got.State != ClaudeSandboxStateOn {
		t.Fatalf("user tier: got state %v, want on", got.State)
	}

	// A project file outranks the user file — and is found from a SUBdirectory,
	// which is how an agent launched in a package dir sees its repo's settings.
	writeSettings(t, filepath.Join(project, ".claude", "settings.json"), `{"sandbox":{"enabled":false}}`)
	got = ResolveClaudeSandboxEnabled("", filepath.Join(project, "sub", "dir"))
	if got.State != ClaudeSandboxStateOff {
		t.Fatalf("project tier: got state %v, want off", got.State)
	}
	if !strings.Contains(got.Source, filepath.Join("repo", ".claude", "settings.json")) {
		t.Fatalf("project tier: got source %q, want the project settings file", got.Source)
	}

	// settings.local.json outranks the shared project file.
	writeSettings(t, filepath.Join(project, ".claude", "settings.local.json"), `{"sandbox":{"enabled":true}}`)
	if got := ResolveClaudeSandboxEnabled("", project); got.State != ClaudeSandboxStateOn {
		t.Fatalf("project-local tier: got state %v, want on", got.State)
	}

	// Managed policy settings outrank everything.
	writeSettings(t, filepath.Join(managed, "managed-settings.json"), `{"sandbox":{"enabled":false}}`)
	if got := ResolveClaudeSandboxEnabled("", project); got.State != ClaudeSandboxStateOff {
		t.Fatalf("managed tier: got state %v, want off", got.State)
	}
}

func TestResolveClaudeSandboxEnabledSkipsSilentTiers(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	project := filepath.Join(home, "repo")
	// A file that exists but says nothing about the sandbox must not shadow the
	// tier below it — otherwise any project with a settings.json would read as
	// unconfigured regardless of the operator's own global posture.
	writeSettings(t, filepath.Join(project, ".claude", "settings.json"), `{"model":"opus"}`)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)

	if got := ResolveClaudeSandboxEnabled("", project); got.State != ClaudeSandboxStateOn {
		t.Fatalf("got state %v, want on (user tier should decide)", got.State)
	}
}

func TestResolveClaudeSandboxEnabledReportsUnparseableFile(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox": {`)

	got := ResolveClaudeSandboxEnabled("", home)
	if got.State != ClaudeSandboxStateUnknown {
		t.Fatalf("got state %v, want unconfigured", got.State)
	}
	if len(got.Diagnostics) != 1 || !strings.Contains(got.Diagnostics[0], "Could not parse") {
		t.Fatalf("got diagnostics %v, want one parse diagnostic", got.Diagnostics)
	}
	// A parser diagnostic can quote the source; settings.json also holds keys
	// that are none of an agent's business, so the message must not echo it.
	if strings.Contains(got.Diagnostics[0], `{"sandbox"`) {
		t.Fatalf("diagnostic leaked file content: %q", got.Diagnostics[0])
	}
}

func TestResolveClaudeSandboxEnabledIgnoresRelativeCwd(t *testing.T) {
	home, _ := isolateClaudeSettings(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"sandbox":{"enabled":true}}`)
	// A relative cwd must not turn into a walk from the daemon's own process
	// directory. The user tier still answers.
	if got := ResolveClaudeSandboxEnabled("", "some/relative/path"); got.State != ClaudeSandboxStateOn {
		t.Fatalf("got state %v, want on", got.State)
	}
}

func TestUnsandboxedAutonomyWarnings(t *testing.T) {
	claude, ok := Get(DefaultName)
	if !ok {
		t.Fatalf("harness %q is not registered", DefaultName)
	}
	codex, ok := Get(CodexName)
	if !ok {
		t.Fatalf("harness %q is not registered", CodexName)
	}

	for _, tc := range []struct {
		name          string
		harness       *Harness
		approval      string
		sandbox       string
		userSandbox   string // "" = no user settings file at all
		wantWarning   bool
		wantSubstring string
	}{{
		name: "auto with no sandbox configured is the TCL-586 case",
		harness: claude, approval: claudePermAuto, sandbox: ClaudeSandboxInherit,
		wantWarning: true, wantSubstring: "no Claude Code settings file tclaude can see enables",
	}, {
		name: "auto with the operator's sandbox enabled is fine",
		harness: claude, approval: claudePermAuto, sandbox: ClaudeSandboxInherit,
		userSandbox: `{"sandbox":{"enabled":true}}`, wantWarning: false,
	}, {
		name: "auto with sandbox forced on is fine",
		harness: claude, approval: claudePermAuto, sandbox: ClaudeSandboxOn, wantWarning: false,
	}, {
		name: "auto with sandbox forced off names the launch, not the settings",
		harness: claude, approval: claudePermAuto, sandbox: ClaudeSandboxOff,
		userSandbox: `{"sandbox":{"enabled":true}}`,
		wantWarning: true, wantSubstring: "this launch forces the OS sandbox off",
	}, {
		name: "settings that disable the sandbox are named as the cause",
		harness: claude, approval: claudePermAuto, sandbox: ClaudeSandboxInherit,
		userSandbox: `{"sandbox":{"enabled":false}}`,
		wantWarning: true, wantSubstring: "turned off by ~/.claude/settings.json",
	}, {
		name: "bypassPermissions does not claim a classifier it does not have",
		harness: claude, approval: claudePermBypass, sandbox: ClaudeSandboxInherit,
		wantWarning: true, wantSubstring: "nothing at all stands between it",
	}, {
		// acceptEdits holds approvalAutoEdits only: its unattended writes stay
		// in the working directory, so it is not the pairing this warns about.
		name: "acceptEdits does not warn",
		harness: claude, approval: claudePermAccept, sandbox: ClaudeSandboxInherit, wantWarning: false,
	}, {
		name: "plan does not warn",
		harness: claude, approval: claudePermPlan, sandbox: ClaudeSandboxInherit, wantWarning: false,
	}, {
		// inherit is unknowable; warning on it would fire for every launch that
		// asked for exactly the operator's own settings.
		name: "inherit does not warn",
		harness: claude, approval: claudePermInherit, sandbox: ClaudeSandboxInherit, wantWarning: false,
	}, {
		// Codex resolves autonomy and sandbox together at spawn, so it can never
		// reach this state and must not be second-guessed by a Claude-shaped check.
		name: "codex never warns",
		harness: codex, approval: ApprovalNever, sandbox: SandboxManagedProfile, wantWarning: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := isolateClaudeSettings(t)
			if tc.userSandbox != "" {
				writeSettings(t, filepath.Join(home, ".claude", "settings.json"), tc.userSandbox)
			}
			got := UnsandboxedAutonomyWarnings(tc.harness, tc.approval, tc.sandbox, home)
			if tc.wantWarning != (len(got) > 0) {
				t.Fatalf("got %v, wantWarning=%v", got, tc.wantWarning)
			}
			if tc.wantSubstring != "" && !strings.Contains(got[0], tc.wantSubstring) {
				t.Fatalf("got %q, want it to contain %q", got[0], tc.wantSubstring)
			}
			if tc.wantWarning {
				// Every warning must be actionable: name the fix, not just the risk.
				if !strings.Contains(got[0], "sandbox \"on\"") || !strings.Contains(got[0], "install-sandbox-hardening") {
					t.Fatalf("warning is not actionable: %q", got[0])
				}
			}
		})
	}
}

func TestUnsandboxedAutonomyWarningsNilHarness(t *testing.T) {
	if got := UnsandboxedAutonomyWarnings(nil, claudePermAuto, ClaudeSandboxInherit, ""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
