package harness

import (
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
