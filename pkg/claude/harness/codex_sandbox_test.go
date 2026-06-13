package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexSandbox_DefaultMode pins the secure default: a tclaude-spawned
// Codex agent runs under workspace-write (writes confined to cwd+/tmp+
// $TMPDIR, $HOME read-only, network denied), never a full-access mode.
func TestCodexSandbox_DefaultMode(t *testing.T) {
	if got := (codexSandbox{}).DefaultMode(); got != SandboxWorkspaceWrite {
		t.Fatalf("codex default sandbox = %q, want %q", got, SandboxWorkspaceWrite)
	}
}

// TestCodexSandbox_ValidateMode accepts the three real Codex modes (and ""
// = caller substitutes the default) and rejects anything else with a
// message naming the valid set.
func TestCodexSandbox_ValidateMode(t *testing.T) {
	for _, ok := range []string{"", SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFull, "  workspace-write  "} {
		got, err := (codexSandbox{}).ValidateMode(ok)
		if err != nil {
			t.Errorf("ValidateMode(%q) errored: %v", ok, err)
		}
		if got != strings.TrimSpace(ok) {
			t.Errorf("ValidateMode(%q) = %q, want trimmed %q", ok, got, strings.TrimSpace(ok))
		}
	}
	if _, err := (codexSandbox{}).ValidateMode("yolo"); err == nil {
		t.Fatalf("ValidateMode(yolo) must error")
	}
}

// TestResolveSandboxMode covers the single spawn-boundary entry point:
// Codex defaults an empty request to workspace-write and validates an
// explicit one; a harness without a launch sandbox flag (Claude Code)
// resolves empty to "" and rejects any explicit mode.
func TestResolveSandboxMode(t *testing.T) {
	codex, err := Resolve(CodexName)
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}
	claude := Default()

	// Codex: unset → secure default.
	if got, err := ResolveSandboxMode(codex, ""); err != nil || got != SandboxWorkspaceWrite {
		t.Fatalf("ResolveSandboxMode(codex, \"\") = %q,%v; want %q,nil", got, err, SandboxWorkspaceWrite)
	}
	// Codex: explicit (incl. the opt-out) validated + passed through.
	if got, err := ResolveSandboxMode(codex, SandboxDangerFull); err != nil || got != SandboxDangerFull {
		t.Fatalf("ResolveSandboxMode(codex, danger) = %q,%v; want %q,nil", got, err, SandboxDangerFull)
	}
	// Codex: junk → error.
	if _, err := ResolveSandboxMode(codex, "nope"); err == nil {
		t.Fatalf("ResolveSandboxMode(codex, nope) must error")
	}
	// Claude: unset → "" (omit; its sandbox is settings.json-driven).
	if got, err := ResolveSandboxMode(claude, ""); err != nil || got != "" {
		t.Fatalf("ResolveSandboxMode(claude, \"\") = %q,%v; want \"\",nil", got, err)
	}
	// Claude: explicit mode → error (no launch sandbox flag).
	if _, err := ResolveSandboxMode(claude, SandboxWorkspaceWrite); err == nil {
		t.Fatalf("ResolveSandboxMode(claude, workspace-write) must error — claude has no --sandbox")
	}
}

// TestValidateSandboxMode is the no-default variant the direct `session new`
// path uses: it must NOT inject the harness default (the human's own
// session keeps their config.toml sandbox_mode unless they pass --sandbox),
// but still validate an explicit value and reject a mode for a flagless
// harness.
func TestValidateSandboxMode(t *testing.T) {
	codex, err := Resolve(CodexName)
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}
	claude := Default()

	// Codex: unset stays "" (NO default — the key difference from Resolve).
	if got, err := ValidateSandboxMode(codex, ""); err != nil || got != "" {
		t.Fatalf("ValidateSandboxMode(codex, \"\") = %q,%v; want \"\",nil (must not default)", got, err)
	}
	// Codex: explicit validated + passed through.
	if got, err := ValidateSandboxMode(codex, SandboxReadOnly); err != nil || got != SandboxReadOnly {
		t.Fatalf("ValidateSandboxMode(codex, read-only) = %q,%v; want %q,nil", got, err, SandboxReadOnly)
	}
	// Codex: junk → error.
	if _, err := ValidateSandboxMode(codex, "nope"); err == nil {
		t.Fatalf("ValidateSandboxMode(codex, nope) must error")
	}
	// Claude: unset → ""; explicit → error.
	if got, err := ValidateSandboxMode(claude, ""); err != nil || got != "" {
		t.Fatalf("ValidateSandboxMode(claude, \"\") = %q,%v; want \"\",nil", got, err)
	}
	if _, err := ValidateSandboxMode(claude, SandboxWorkspaceWrite); err == nil {
		t.Fatalf("ValidateSandboxMode(claude, workspace-write) must error")
	}
}

// TestCodexSandboxCwdConflict pins the cwd-safety guard: a writable Codex
// sandbox (workspace-write) confines writes to the cwd subtree, so a cwd
// at/above $HOME (or at/above a protected state dir) exposes those dirs
// and must be refused; a project subdirectory, a read-only sandbox, and
// the danger-full-access opt-out never conflict.
func TestCodexSandboxCwdConflict(t *testing.T) {
	home := "/home/dev"
	cases := []struct {
		mode, cwd string
		want      bool
	}{
		{SandboxWorkspaceWrite, home, true},                             // cwd == $HOME
		{SandboxWorkspaceWrite, "/home", true},                          // ancestor of $HOME
		{SandboxWorkspaceWrite, "/", true},                              // root
		{SandboxWorkspaceWrite, filepath.Join(home, ".tclaude"), true},  // the protected dir itself
		{SandboxWorkspaceWrite, filepath.Join(home, ".codex"), true},    // codex state home
		{SandboxWorkspaceWrite, filepath.Join(home, "projects"), false}, // a normal project root
		{SandboxWorkspaceWrite, "/home/dev-other", false},               // sibling, not a prefix match
		{SandboxWorkspaceWrite, filepath.Join(home, "projects", "x"), false},
		{SandboxReadOnly, home, false},   // read-only can't write
		{SandboxDangerFull, home, false}, // explicit opt-out
		{SandboxWorkspaceWrite, "", false},
	}
	for _, c := range cases {
		if got := CodexSandboxCwdConflict(c.mode, c.cwd, home); got != c.want {
			t.Errorf("CodexSandboxCwdConflict(%q, %q, %q) = %v, want %v", c.mode, c.cwd, home, got, c.want)
		}
	}
}

// TestCodexSandboxCwdConflict_Symlink pins the symlink hardening: a cwd that
// is a symlink resolving into $HOME (Codex confines writes to the *resolved*
// real path) must conflict, even though a textual comparison of the unresolved
// link path would step out of $HOME and read as safe.
func TestCodexSandboxCwdConflict_Symlink(t *testing.T) {
	home := t.TempDir()
	// A symlink that resolves to $HOME itself: workspace-write rooted here
	// would make $HOME (hence the protected dirs under it) writable.
	link := filepath.Join(t.TempDir(), "cwd-link")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	// Sanity: the unresolved link is NOT a textual prefix of home, so without
	// EvalSymlinks the guard would (wrongly) return false.
	if got := CodexSandboxCwdConflict(SandboxWorkspaceWrite, link, home); !got {
		t.Fatalf("symlinked-into-$HOME cwd must conflict, got false (link=%q home=%q)", link, home)
	}
	// A symlink resolving to a normal project subdir under $HOME stays safe.
	proj := filepath.Join(home, "projects", "foo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	projLink := filepath.Join(t.TempDir(), "proj-link")
	if err := os.Symlink(proj, projLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := CodexSandboxCwdConflict(SandboxWorkspaceWrite, projLink, home); got {
		t.Fatalf("symlink to a project subdir must NOT conflict, got true (link=%q)", projLink)
	}
}

// TestCodexSandboxCwdConflict_AsymmetricExistence is the deterministic
// regression for the macOS-autofs failure: cwd is an ancestor of $HOME, the
// shared prefix is reached through a symlink, and $HOME's leaf does NOT yet
// exist. Resolving only the whole path leaves cwd (fully resolved) and home
// (un-resolved literal) in divergent real trees, so the ancestor check
// wrongly reads "safe". The longest-existing-prefix resolution keeps both in
// the same tree, so the conflict is caught — on every platform, not just one
// where /home happens to be a mount.
func TestCodexSandboxCwdConflict_AsymmetricExistence(t *testing.T) {
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	// cwd = <link>/home (exists through the symlink); home = <link>/home/dev
	// (the "dev" leaf is never created). cwd is the parent of home → a
	// workspace-write rooted at cwd makes home (and its protected dirs)
	// writable, so this must conflict.
	cwd := filepath.Join(linkRoot, "home")
	home := filepath.Join(linkRoot, "home", "dev")
	if got := CodexSandboxCwdConflict(SandboxWorkspaceWrite, cwd, home); !got {
		t.Fatalf("ancestor-of-$HOME cwd via symlink with non-existent home leaf must conflict, got false (cwd=%q home=%q)", cwd, home)
	}
}

// TestCodexSpawner_SandboxFlag verifies the emitted Codex args carry
// `--sandbox <mode>` when a mode is set (on both fresh + resume) and omit
// it when unset — the JOH-192 acceptance at the literal arg surface.
func TestCodexSpawner_SandboxFlag(t *testing.T) {
	// Unset → no flag.
	if got := (codexSpawner{}).BuildCommand(SpawnSpec{}); strings.Contains(got, "--sandbox") {
		t.Fatalf("unset sandbox must omit --sandbox, got %q", got)
	}
	// Fresh spawn with a mode.
	got := (codexSpawner{}).BuildCommand(SpawnSpec{SandboxMode: SandboxWorkspaceWrite})
	if !strings.Contains(got, "--sandbox workspace-write") {
		t.Fatalf("fresh spawn must emit `--sandbox workspace-write`, got %q", got)
	}
	// Resume with a mode (shared global flag).
	gotR := (codexSpawner{}).BuildCommand(SpawnSpec{ResumeID: "abc-123", SandboxMode: SandboxDangerFull})
	if !strings.Contains(gotR, "resume abc-123") || !strings.Contains(gotR, "--sandbox danger-full-access") {
		t.Fatalf("resume must carry the resume subcommand + `--sandbox danger-full-access`, got %q", gotR)
	}
}
