package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexAgentProfileContent pins the managed profile's TOML shape — the
// exact form verified end-to-end against codex-cli 0.139.0 (extends
// :workspace, network enabled, the agentd socket allowlisted, and
// default_permissions so `codex -p <name>` activates it).
func TestCodexAgentProfileContent(t *testing.T) {
	sock := "/home/dev/.tclaude/agentd.sock"
	got, err := codexAgentProfileContent(sock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		`default_permissions = "tclaude-agent"`,
		`[permissions.tclaude-agent]`,
		`extends = ":workspace"`,
		`[permissions.tclaude-agent.network]`,
		`enabled = true`,
		`[permissions.tclaude-agent.network.unix_sockets]`,
		`"/home/dev/.tclaude/agentd.sock" = "allow"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("profile content missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCodexAgentProfileContent_RejectsUnsafePath: a non-absolute path or one
// carrying a TOML-key-breaking character is refused rather than allowed to
// corrupt the file.
func TestCodexAgentProfileContent_RejectsUnsafePath(t *testing.T) {
	for _, bad := range []string{
		"relative/agentd.sock",           // not absolute
		`/home/d"v/.tclaude/agentd.sock`, // embedded double-quote
		"/home/dev/\t/agentd.sock",       // control char
		`/home/dev\agentd.sock`,          // backslash
	} {
		if _, err := codexAgentProfileContent(bad); err == nil {
			t.Fatalf("expected rejection of unsafe socket path %q", bad)
		}
	}
}

// TestValidateCodexProfileName: empty passes through (omit the flag), a simple
// identifier is accepted, and anything with path/shell/TOML metacharacters is
// rejected at the boundary where the human-facing --permission-profile enters.
func TestValidateCodexProfileName(t *testing.T) {
	if got, err := ValidateCodexProfileName("  "); err != nil || got != "" {
		t.Fatalf("blank must pass through as \"\", got %q err %v", got, err)
	}
	if got, err := ValidateCodexProfileName(" tclaude-agent "); err != nil || got != "tclaude-agent" {
		t.Fatalf("valid name must trim+pass, got %q err %v", got, err)
	}
	for _, bad := range []string{"../evil", "a b", "a/b", `a"b`, "a.b", "a;b"} {
		if _, err := ValidateCodexProfileName(bad); err == nil {
			t.Fatalf("expected rejection of profile name %q", bad)
		}
	}
}

// TestEnsureCodexAgentProfile writes the managed profile under a temp
// CODEX_HOME, then asserts it lands at <home>/tclaude-agent.config.toml with
// the canonical content, is idempotent (a second call leaves identical bytes),
// and self-heals (a corrupted file is rewritten).
func TestEnsureCodexAgentProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	sock := "/home/dev/.tclaude/agentd.sock"
	path, err := ensureCodexAgentProfile(sock)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := filepath.Join(home, "tclaude-agent.config.toml")
	if path != want {
		t.Fatalf("profile path = %q, want %q", path, want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content, _ := codexAgentProfileContent(sock)
	if string(first) != content {
		t.Fatalf("written content mismatch\n--- got ---\n%s\n--- want ---\n%s", first, content)
	}

	// Idempotent: second call leaves identical bytes.
	if _, err := ensureCodexAgentProfile(sock); err != nil {
		t.Fatalf("ensure (2nd): %v", err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != content {
		t.Fatalf("second ensure changed content")
	}

	// Self-healing: a hand-corrupted file is rewritten to canonical content.
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := ensureCodexAgentProfile(sock); err != nil {
		t.Fatalf("ensure (heal): %v", err)
	}
	healed, _ := os.ReadFile(path)
	if string(healed) != content {
		t.Fatalf("ensure did not self-heal a corrupted profile file")
	}
}
