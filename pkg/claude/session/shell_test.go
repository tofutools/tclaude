package session

import (
	"strings"
	"testing"
)

// TestRunNewShell_RejectsCodingHarnessFlags pins that every coding-harness-only
// NewParams field is rejected up front (rejectShellUnsupportedFlags runs
// before GuardAgainstNestedSpawn/tmux), so a stray flag combo errors cleanly
// instead of silently doing nothing once a shell session is running.
func TestRunNewShell_RejectsCodingHarnessFlags(t *testing.T) {
	cases := []struct {
		name   string
		params NewParams
		want   string
	}{
		{"resume", NewParams{Resume: "abc123"}, "--resume"},
		{"model", NewParams{Model: "opus"}, "--model"},
		{"effort", NewParams{Effort: "high"}, "--effort"},
		{"sandbox", NewParams{Sandbox: "workspace-write"}, "--sandbox"},
		{"permission-profile", NewParams{PermissionProfile: "tclaude-agent"}, "--permission-profile"},
		{"approval", NewParams{Approval: "never"}, "--ask-for-approval"},
		{"auto-review", NewParams{AutoReview: true}, "--auto-review"},
		{"trust-dir", NewParams{TrustDir: true}, "--trust-dir"},
		{"remote-control", NewParams{RemoteControl: true}, "--remote-control"},
		{"wait-for-rate-limit", NewParams{WaitForRateLimit: true}, "--wait-for-rate-limit"},
		{"join-group", NewParams{JoinGroup: "mygroup"}, "--join-group"},
		{"name", NewParams{Name: "my session"}, "--name"},
		{"initial-prompt", NewParams{InitialPrompt: "hi"}, "--initial-prompt"},
		{"session-id", NewParams{SessionID: "550e8400-e29b-41d4-a716-446655440000"}, "--session-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.params
			p.Harness = ShellHarnessName
			err := RunNew(&p)
			if err == nil {
				t.Fatalf("%s must be rejected for --harness shell", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error should name %s, got: %v", tc.want, err)
			}
		})
	}
}

// TestRunNew_ShellFlagIsHarnessShellAlias pins that --shell is equivalent to
// --harness shell (same rejection behavior on a coding-harness-only flag),
// and that combining --shell with a conflicting explicit --harness errors
// instead of silently picking one.
func TestRunNew_ShellFlagIsHarnessShellAlias(t *testing.T) {
	t.Run("alias behaves like --harness shell", func(t *testing.T) {
		err := RunNew(&NewParams{Shell: true, Model: "opus"})
		if err == nil {
			t.Fatalf("--shell --model must be rejected, same as --harness shell --model")
		}
		if !strings.Contains(err.Error(), "--model") {
			t.Fatalf("error should name --model, got: %v", err)
		}
	})
	t.Run("redundant with --harness shell", func(t *testing.T) {
		err := RunNew(&NewParams{Shell: true, Harness: ShellHarnessName, Model: "opus"})
		if err == nil || !strings.Contains(err.Error(), "--model") {
			t.Fatalf("expected the same --model rejection, got: %v", err)
		}
	})
	t.Run("conflicts with a different --harness", func(t *testing.T) {
		err := RunNew(&NewParams{Shell: true, Harness: "codex"})
		if err == nil {
			t.Fatalf("--shell --harness codex must error")
		}
		if !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("error should explain the conflict, got: %v", err)
		}
	})
}

func TestShellBinary(t *testing.T) {
	t.Run("uses $SHELL when set", func(t *testing.T) {
		t.Setenv("SHELL", "/usr/bin/zsh")
		if got := shellBinary(); got != "/usr/bin/zsh" {
			t.Fatalf("shellBinary() = %q, want /usr/bin/zsh", got)
		}
	})
	t.Run("falls back to /bin/sh when unset", func(t *testing.T) {
		t.Setenv("SHELL", "")
		if got := shellBinary(); got != "/bin/sh" {
			t.Fatalf("shellBinary() = %q, want /bin/sh", got)
		}
	})
}
