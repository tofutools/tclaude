package harness

import (
	"strings"
	"testing"
)

// build is a tiny helper that runs the claude harness's command builder
// for the given spec fields, mirroring the old session.buildClaudeCmd
// signature so these acceptance checks read the same as before the seam.
func build(env, resume, effort, model string, extra []string) string {
	return claudeSpawner{}.BuildCommand(SpawnSpec{
		EnvExports: env,
		ResumeID:   resume,
		Effort:     effort,
		Model:      model,
		ExtraArgs:  extra,
	})
}

// TestClaudeSpawner_Model is the acceptance check for the regular
// `tclaude session new` surface: an unset model must NOT add --model to
// the claude invocation (claude keeps its own default), and an explicit
// alias must append `--model <alias>`.
func TestClaudeSpawner_Model(t *testing.T) {
	// Unset → no --model anywhere in the command.
	if got := build("", "", "", "", nil); strings.Contains(got, "--model") {
		t.Fatalf("unset model must omit --model, got %q", got)
	}

	// Set → `--model <alias>` appended.
	if got := build("", "", "", "opus", nil); !strings.Contains(got, "--model opus") {
		t.Fatalf("set model must append --model opus, got %q", got)
	}

	// The [1m] aliases contain sh glob characters; the command is run
	// via `sh -c`, so they must arrive quoted.
	got := build("", "", "", "sonnet[1m]", nil)
	if !strings.Contains(got, `--model 'sonnet[1m]'`) && !strings.Contains(got, `--model "sonnet[1m]"`) {
		t.Fatalf("[1m] model must be shell-quoted, got %q", got)
	}

	// Coexists with --resume, --effort and post-`--` passthrough args.
	got = build("", "conv-123", "max", "fable", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
	if !strings.Contains(got, "--model fable") {
		t.Fatalf("expected --model fable, got %q", got)
	}
}

// TestClaudeSpawner_Effort is the acceptance check for the regular
// `tclaude session new` surface: an unset effort must NOT add --effort to
// the claude invocation (claude keeps its own default), and an explicit
// level must append `--effort <level>` verbatim.
func TestClaudeSpawner_Effort(t *testing.T) {
	// Unset → no --effort anywhere in the command.
	if got := build("", "", "", "", nil); strings.Contains(got, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %q", got)
	}

	// Set → `--effort <level>` appended.
	if got := build("", "", "high", "", nil); !strings.Contains(got, "--effort high") {
		t.Fatalf("set effort must append --effort high, got %q", got)
	}

	// Coexists with --resume and post-`--` passthrough args.
	got := build("", "conv-123", "max", "", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
}

// TestClaudeSpawner_EnvAndBinary covers the env-export prefix and the
// pass-through binary name — the two remaining moving parts of the spawn
// command.
func TestClaudeSpawner_EnvAndBinary(t *testing.T) {
	got := build("export TCLAUDE_SESSION_ID=abc; ", "", "", "", nil)
	if !strings.HasPrefix(got, "export TCLAUDE_SESSION_ID=abc; claude") {
		t.Fatalf("env exports must precede the claude binary, got %q", got)
	}
	if bin := (claudeSpawner{}).Binary(); bin != "claude" {
		t.Fatalf("claude binary = %q, want claude", bin)
	}
}

// TestClaudeModels_Delegation checks the catalog forwards to the curated
// clcommon validators and that the list getters return non-empty copies.
func TestClaudeModels_Delegation(t *testing.T) {
	c := claudeModels{}

	if _, err := c.ValidateModel("opus"); err != nil {
		t.Fatalf("ValidateModel(opus) unexpected error: %v", err)
	}
	if _, err := c.ValidateModel("definitely-not-a-model"); err == nil {
		t.Fatalf("ValidateModel(bogus) should error")
	}
	if norm, _ := c.ValidateEffort("  HIGH "); norm != "high" {
		t.Fatalf("ValidateEffort normalisation = %q, want high", norm)
	}
	if got, _ := c.ValidateModel(""); got != "" {
		t.Fatalf("empty model must stay empty, got %q", got)
	}

	if len(c.Models()) == 0 {
		t.Fatalf("Models() returned empty list")
	}
	if len(c.EffortLevels()) == 0 {
		t.Fatalf("EffortLevels() returned empty list")
	}
	// The getter must hand back a copy — mutating it must not corrupt the
	// shared source list.
	models := c.Models()
	models[0] = "MUTATED"
	if c.Models()[0] == "MUTATED" {
		t.Fatalf("Models() leaked the shared backing slice")
	}
}

// TestClaudeSpawner_LaunchEnrollment covers the launch-enrollment flags the
// daemon's efficient spawn path relies on: a preset conv-id (--session-id), a
// launch display name (--name), and a positional first-turn prompt. Each is a
// fresh-launch-only flag, and each is shell-quoted because it reaches `sh -c`.
func TestClaudeSpawner_LaunchEnrollment(t *testing.T) {
	spec := SpawnSpec{
		SessionID:     "2567b392-357b-4d6c-9a59-74fd23424cda",
		Name:          "worker bee",
		InitialPrompt: "[system: spawned by the human; read inbox #7]",
	}
	got := claudeSpawner{}.BuildCommand(spec)

	if !strings.Contains(got, "--session-id 2567b392-357b-4d6c-9a59-74fd23424cda") {
		t.Fatalf("expected --session-id, got %q", got)
	}
	// The name has a space, so it must be quoted.
	if !strings.Contains(got, `--name 'worker bee'`) {
		t.Fatalf("expected quoted --name, got %q", got)
	}
	// The welcome carries shell metacharacters ([], #, ;), so the whole
	// positional prompt must arrive as one quoted arg at the end.
	if !strings.Contains(got, `'[system: spawned by the human; read inbox #7]'`) {
		t.Fatalf("expected quoted positional prompt, got %q", got)
	}

	// On a --resume the preset id + positional prompt are omitted (the
	// conversation already has an id and history); --name still applies.
	r := claudeSpawner{}.BuildCommand(SpawnSpec{
		ResumeID:      "conv-9",
		SessionID:     "2567b392-357b-4d6c-9a59-74fd23424cda",
		Name:          "worker",
		InitialPrompt: "hello",
	})
	if strings.Contains(r, "--session-id") {
		t.Fatalf("a resume must not emit --session-id, got %q", r)
	}
	if strings.Contains(r, "hello") {
		t.Fatalf("a resume must not emit a positional prompt, got %q", r)
	}
	if !strings.Contains(r, "--resume conv-9") || !strings.Contains(r, "--name worker") {
		t.Fatalf("resume must keep --resume and --name, got %q", r)
	}

	// The default (claude) harness advertises the capability; an unset spec
	// emits none of the flags.
	if !Default().SupportsLaunchEnrollment() {
		t.Fatalf("claude must support launch enrollment")
	}
	bare := claudeSpawner{}.BuildCommand(SpawnSpec{})
	if strings.Contains(bare, "--session-id") || strings.Contains(bare, "--name") {
		t.Fatalf("an empty spec must omit launch-enrollment flags, got %q", bare)
	}
}

// TestClaudeLifecycle_Tokens pins the CC slash-command tokens so the
// capability flags report supported and the injection call sites keep
// typing the exact commands CC understands.
func TestClaudeLifecycle_Tokens(t *testing.T) {
	h := Default()
	if h.Life.RenameCommand() != "/rename" {
		t.Fatalf("rename token = %q, want /rename", h.Life.RenameCommand())
	}
	if h.Life.CompactCommand() != "/compact" {
		t.Fatalf("compact token = %q, want /compact", h.Life.CompactCommand())
	}
	if h.Life.SoftExitCommand() != "/exit" {
		t.Fatalf("soft-exit token = %q, want /exit", h.Life.SoftExitCommand())
	}
	if !h.SupportsRename() || !h.SupportsCompact() || !h.SupportsSoftExit() {
		t.Fatalf("claude must support rename/compact/soft-exit")
	}
}
