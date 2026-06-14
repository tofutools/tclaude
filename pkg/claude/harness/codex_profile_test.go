package harness

import (
	"os"
	"os/exec"
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
	got, err := codexAgentProfileContent(sock, "")
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

// TestCodexAgentProfileContent_WithGitCommonDir pins the repo-scoped grant
// that lets a sandboxed Codex worker commit from a linked worktree without
// making the rest of $HOME writable.
func TestCodexAgentProfileContent_WithGitCommonDir(t *testing.T) {
	got, err := codexAgentProfileContent(
		"/home/dev/.tclaude/agentd.sock",
		"/home/dev/git/project/.git",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		`[permissions.tclaude-agent.filesystem]`,
		`"/home/dev/git/project/.git" = "write"`,
		`[permissions.tclaude-agent.network]`,
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
		if _, err := codexAgentProfileContent(bad, ""); err == nil {
			t.Fatalf("expected rejection of unsafe socket path %q", bad)
		}
	}
	for _, bad := range []string{
		"relative/.git",            // not absolute
		`/home/d"v/project/.git`,   // embedded double-quote
		"/home/dev/project/\t.git", // control char
		`/home/dev/project\.git`,   // backslash
	} {
		if _, err := codexAgentProfileContent("/home/dev/.tclaude/agentd.sock", bad); err == nil {
			t.Fatalf("expected rejection of unsafe git common dir %q", bad)
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
	path, err := ensureCodexAgentProfile(sock, "")
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
	content, _ := codexAgentProfileContent(sock, "")
	if string(first) != content {
		t.Fatalf("written content mismatch\n--- got ---\n%s\n--- want ---\n%s", first, content)
	}

	// Idempotent: second call leaves identical bytes.
	if _, err := ensureCodexAgentProfile(sock, ""); err != nil {
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
	if _, err := ensureCodexAgentProfile(sock, ""); err != nil {
		t.Fatalf("ensure (heal): %v", err)
	}
	healed, _ := os.ReadFile(path)
	if string(healed) != content {
		t.Fatalf("ensure did not self-heal a corrupted profile file")
	}
}

// TestCodexGitCommonDir_LinkedWorktree pins the linked-worktree case that
// breaks `git commit` under Codex's default protected-.git sandboxing: the
// writable path must be the repository's common .git dir, not the per-worktree
// metadata dir.
func TestCodexGitCommonDir_LinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	// On macOS t.TempDir() hands back a /var/folders/... path, but /var is a
	// symlink to /private/var; git's rev-parse resolves that symlink in the
	// path it prints. Resolve the root here too so the expected path is built
	// in the same (resolved) form git returns — otherwise got=/private/var/…
	// vs want=/var/… mismatches on macOS only.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "wt")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(repo, "add", "README.md")
	runGit(repo, "-c", "user.name=tclaude", "-c", "user.email=tclaude@example.invalid",
		"commit", "-m", "init")
	runGit(repo, "worktree", "add", "-b", "wt", wt)

	got, err := codexGitCommonDir(wt)
	if err != nil {
		t.Fatalf("codexGitCommonDir: %v", err)
	}
	want := filepath.Join(repo, ".git")
	if got != want {
		t.Fatalf("git common dir = %q, want %q", got, want)
	}
}

// TestCodexAgentProfileStatus covers the read-only `setup --check` helper:
// missing before install, present+current after EnsureCodexAgentProfile, and
// present-but-not-current when the file is corrupted (without writing).
func TestCodexAgentProfileStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	want := filepath.Join(home, "tclaude-agent.config.toml")

	// Missing → not present, not current, no error.
	path, present, current, err := CodexAgentProfileStatus()
	if err != nil || present || current || path != want {
		t.Fatalf("missing: got path=%q present=%v current=%v err=%v", path, present, current, err)
	}

	// After ensure → present + current.
	if _, err := EnsureCodexAgentProfile(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, present, current, err := CodexAgentProfileStatus(); err != nil || !present || !current {
		t.Fatalf("installed: present=%v current=%v err=%v", present, current, err)
	}

	// Corrupted → present but NOT current; the check must not rewrite it.
	if err := os.WriteFile(want, []byte("garbage\n"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, present, current, err := CodexAgentProfileStatus(); err != nil || !present || current {
		t.Fatalf("stale: present=%v current=%v err=%v", present, current, err)
	}
	if cur, _ := os.ReadFile(want); string(cur) != "garbage\n" {
		t.Fatalf("status() must be read-only, but it rewrote the file")
	}
}
