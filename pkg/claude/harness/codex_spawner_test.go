package harness

import (
	"strings"
	"testing"
)

func codexBuild(env, resume, effort, model string, extra []string) string {
	return codexSpawner{}.BuildCommand(SpawnSpec{
		EnvExports: env,
		ResumeID:   resume,
		Effort:     effort,
		Model:      model,
		ExtraArgs:  extra,
	})
}

// TestCodexSpawner_New covers a fresh Codex session: bare `codex`, with an
// optional `--model`, env exports prepended, and the binary name.
func TestCodexSpawner_New(t *testing.T) {
	if got := codexBuild("", "", "", "", nil); got != "codex" {
		t.Fatalf("bare new session must be exactly %q, got %q", "codex", got)
	}

	got := codexBuild("", "", "", "gpt-5-codex", nil)
	if !strings.Contains(got, "--model gpt-5-codex") {
		t.Fatalf("set model must append --model, got %q", got)
	}
	if strings.Contains(got, "resume") {
		t.Fatalf("a fresh session must NOT use the resume subcommand, got %q", got)
	}

	got = codexBuild("export TCLAUDE_SESSION_ID=abc; ", "", "", "", nil)
	if !strings.HasPrefix(got, "export TCLAUDE_SESSION_ID=abc; codex") {
		t.Fatalf("env exports must precede the codex binary, got %q", got)
	}

	if bin := (codexSpawner{}).Binary(); bin != "codex" {
		t.Fatalf("codex binary = %q, want codex", bin)
	}
}

// TestCodexSpawner_Resume covers resume: `codex resume <id>` (a subcommand,
// not a --resume flag), optionally with --model, coexisting with passthrough.
func TestCodexSpawner_Resume(t *testing.T) {
	got := codexBuild("", "sess-123", "", "", nil)
	if !strings.Contains(got, "codex resume sess-123") {
		t.Fatalf("resume must use the `resume <id>` subcommand, got %q", got)
	}
	if strings.Contains(got, "--resume") {
		t.Fatalf("codex resume is a subcommand, never a --resume flag, got %q", got)
	}

	got = codexBuild("", "sess-123", "", "gpt-5", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "resume sess-123") || !strings.Contains(got, "--model gpt-5") {
		t.Fatalf("resume + model must coexist, got %q", got)
	}
	if !strings.Contains(got, "'bar baz'") && !strings.Contains(got, `"bar baz"`) {
		t.Fatalf("passthrough args must be shell-quoted, got %q", got)
	}
}

// TestCodexModels covers the minimal catalog: model pass-through, effort
// rejected-with-guidance, empty values stay empty.
func TestCodexModels(t *testing.T) {
	c := codexModels{}

	if got, err := c.ValidateModel("  gpt-5-codex "); err != nil || got != "gpt-5-codex" {
		t.Fatalf("ValidateModel should trim + pass through, got (%q, %v)", got, err)
	}
	if got, err := c.ValidateModel(""); err != nil || got != "" {
		t.Fatalf("empty model stays empty, got (%q, %v)", got, err)
	}

	if got, err := c.ValidateEffort(""); err != nil || got != "" {
		t.Fatalf("empty effort is allowed, got (%q, %v)", got, err)
	}
	if _, err := c.ValidateEffort("high"); err == nil {
		t.Fatalf("a non-empty effort must error until the codex reasoning mapping is wired")
	}

	if len(c.Models()) != 0 || len(c.EffortLevels()) != 0 {
		t.Fatalf("codex catalog should not curate model/effort suggestions yet")
	}
}

// TestCodexHarness_Registered pins the descriptor: codex resolves, carries
// a Spawner + ModelCatalog + ConvStore, and reports no in-pane lifecycle
// commands (so agentd routes its rename through ConvStore.SetTitle).
func TestCodexHarness_Registered(t *testing.T) {
	h, err := Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}
	if h.Spawn == nil || h.Models == nil || h.Convs == nil {
		t.Fatalf("codex descriptor missing a contract: %+v", h)
	}
	if h.SupportsRename() || h.SupportsCompact() || h.SupportsSoftExit() {
		t.Fatalf("codex has no in-pane lifecycle commands yet")
	}
	if h.Spawn.Binary() != "codex" {
		t.Fatalf("codex binary = %q", h.Spawn.Binary())
	}
}
