package harness

import (
	"slices"
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

// TestCodexSpawner_BypassHookTrust covers the headless escape hatch: the
// `--dangerously-bypass-hook-trust` flag is emitted only when the toggle is
// set (default off), on both fresh and resume invocations.
func TestCodexSpawner_BypassHookTrust(t *testing.T) {
	const flag = "--dangerously-bypass-hook-trust"

	// Default off: the flag must never appear.
	if got := codexBuild("", "", "", "", nil); strings.Contains(got, flag) {
		t.Fatalf("bypass-hook-trust must default off, got %q", got)
	}

	// Fresh session with the toggle on.
	got := codexSpawner{}.BuildCommand(SpawnSpec{BypassHookTrust: true})
	if !strings.Contains(got, flag) {
		t.Fatalf("toggle on (fresh) must emit %s, got %q", flag, got)
	}

	// Resume with the toggle on: flag coexists with the resume subcommand
	// and --model.
	got = codexSpawner{}.BuildCommand(SpawnSpec{ResumeID: "sess-1", Model: "gpt-5", BypassHookTrust: true})
	if !strings.Contains(got, flag) || !strings.Contains(got, "resume sess-1") || !strings.Contains(got, "--model gpt-5") {
		t.Fatalf("toggle on (resume) must emit %s alongside resume + model, got %q", flag, got)
	}
}

func TestCodexSpawner_PinsSandboxEnvironmentForToolCommands(t *testing.T) {
	got := codexSpawner{}.BuildCommand(SpawnSpec{ShellEnvironment: map[string]string{
		"GOTMPDIR": "/private/go tmp",
		"GOBIN":    "/private/gobin",
	}})

	gobin := `shell_environment_policy.set.GOBIN="/private/gobin"`
	gotmp := `shell_environment_policy.set.GOTMPDIR="/private/go tmp"`
	if !strings.Contains(got, gobin) || !strings.Contains(got, gotmp) {
		t.Fatalf("sandbox environment must be pinned through Codex's shell policy, got %q", got)
	}
	if strings.Index(got, gobin) > strings.Index(got, gotmp) {
		t.Fatalf("shell environment overrides must be emitted deterministically, got %q", got)
	}

	got = codexSpawner{}.BuildCommand(SpawnSpec{ShellEnvironment: map[string]string{
		"COMPLEX": "quote\" slash\\ newline\n escape\x1b",
	}})
	if !strings.Contains(got, `shell_environment_policy.set.COMPLEX="quote\" slash\\ newline\n escape\u001B"`) {
		t.Fatalf("shell environment override must be valid escaped TOML, got %q", got)
	}
}

// TestCodexSpawner_InitialPrompt covers the JOH-205 first-turn seed: a fresh
// launch appends InitialPrompt as the trailing positional [PROMPT], shell-
// quoted as a single arg, so Codex self-submits its first turn (materialising
// the conv-id without a human keystroke). A resume never emits it — the
// conv-id is already known — and an empty prompt is omitted entirely.
func TestCodexSpawner_InitialPrompt(t *testing.T) {
	// Empty: a bare fresh launch stays exactly "codex" — no dangling positional.
	empty := codexSpawner{}.BuildCommand(SpawnSpec{InitialPrompt: ""})
	if empty != "codex" {
		t.Fatalf("empty initial prompt must be omitted (bare %q), got %q", "codex", empty)
	}

	// Fresh launch: the seed is the trailing, shell-quoted positional, and it
	// coexists with flags (here --model) without disturbing them.
	got := codexSpawner{}.BuildCommand(SpawnSpec{InitialPrompt: "read your inbox", Model: "gpt-5"})
	if !strings.HasSuffix(got, " 'read your inbox'") && !strings.HasSuffix(got, ` "read your inbox"`) {
		t.Fatalf("fresh launch must append the seed as the trailing shell-quoted positional, got %q", got)
	}
	if !strings.Contains(got, "--model gpt-5") {
		t.Fatalf("seed prompt must coexist with flags, got %q", got)
	}

	// Resume: the conv-id is already known, so no first-turn kick is emitted.
	got = codexSpawner{}.BuildCommand(SpawnSpec{ResumeID: "sess-1", InitialPrompt: "read your inbox"})
	if strings.Contains(got, "read your inbox") {
		t.Fatalf("resume must NOT emit an initial-prompt seed, got %q", got)
	}
}

// TestCodexModels covers the catalog (JOH-155): pass-through of a Codex
// model, rejection of a Claude Code slug, and tclaude effort levels
// accepted (validated like CC; mapped to Codex reasoning by the spawner).
func TestCodexModels(t *testing.T) {
	c := codexModels{}

	if got, err := c.ValidateModel("  gpt-5-codex "); err != nil || got != "gpt-5-codex" {
		t.Fatalf("ValidateModel should trim + pass through a codex model, got (%q, %v)", got, err)
	}
	if got, err := c.ValidateModel(""); err != nil || got != "" {
		t.Fatalf("empty model stays empty, got (%q, %v)", got, err)
	}
	// A Claude Code slug/ID is rejected for the codex harness.
	for _, cc := range []string{"opus", "Opus", "sonnet", "claude-fable-5", "fable[1m]"} {
		if _, err := c.ValidateModel(cc); err == nil {
			t.Fatalf("ValidateModel(%q) must reject a Claude Code model for codex", cc)
		}
	}

	if got, err := c.ValidateEffort(""); err != nil || got != "" {
		t.Fatalf("empty effort is allowed, got (%q, %v)", got, err)
	}
	if got, err := c.ValidateEffort("  High "); err != nil || got != "high" {
		t.Fatalf("ValidateEffort should accept + normalise a tclaude level, got (%q, %v)", got, err)
	}
	if _, err := c.ValidateEffort("bogus"); err == nil {
		t.Fatalf("an unknown effort level must error")
	}
	if len(c.EffortLevels()) == 0 {
		t.Fatalf("codex now exposes tclaude's effort levels")
	}
	wantModels := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark",
	}
	if got := c.Models(); !slices.Equal(got, wantModels) {
		t.Fatalf("Models() = %v, want %v", got, wantModels)
	}
	// The returned catalog must not expose the package-level slice to callers.
	got := c.Models()
	got[0] = "mutated"
	if c.Models()[0] != wantModels[0] {
		t.Fatal("Models() must return a defensive copy")
	}
}

// TestCodexReasoningEffort pins the tclaude-effort → Codex-reasoning map.
func TestCodexReasoningEffort(t *testing.T) {
	for in, want := range map[string]string{
		"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "xhigh",
	} {
		if got := codexReasoningEffort("", in); got != want {
			t.Fatalf("codexReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
	if got := codexReasoningEffort("gpt-5.6-terra", "max"); got != "max" {
		t.Fatalf("codexReasoningEffort(GPT-5.6, max) = %q, want max", got)
	}
	if got := codexReasoningEffort("gpt-5.6", "max"); got != "max" {
		t.Fatalf("codexReasoningEffort(gpt-5.6 alias, max) = %q, want max", got)
	}
}

// TestCodexSpawner_Effort covers the reasoning-effort config emission:
// unset → no -c flag; set → `-c model_reasoning_effort="<mapped>"`, with
// max mapping to xhigh.
func TestCodexSpawner_Effort(t *testing.T) {
	if got := codexBuild("", "", "", "", nil); strings.Contains(got, "model_reasoning_effort") {
		t.Fatalf("unset effort must omit the reasoning config, got %q", got)
	}
	got := codexBuild("", "", "high", "", nil)
	if !strings.Contains(got, `model_reasoning_effort="high"`) {
		t.Fatalf("effort high must emit model_reasoning_effort=\"high\", got %q", got)
	}
	if !strings.Contains(got, "-c ") {
		t.Fatalf("reasoning effort must be passed via -c, got %q", got)
	}
	got = codexBuild("", "", "max", "", nil)
	if !strings.Contains(got, `model_reasoning_effort="xhigh"`) {
		t.Fatalf("effort max must map to xhigh, got %q", got)
	}
	got = codexBuild("", "", "max", "gpt-5.6-sol", nil)
	if !strings.Contains(got, `model_reasoning_effort="max"`) {
		t.Fatalf("GPT-5.6 max effort must remain distinct, got %q", got)
	}
}

// TestCodexHarness_Registered pins the descriptor: codex resolves, carries
// a Spawner + ModelCatalog + ConvStore, supports `/compact` and `/quit`, and
// leaves rename out-of-band through ConvStore.SetTitle.
func TestCodexHarness_Registered(t *testing.T) {
	h, err := Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}
	if h.Spawn == nil || h.Models == nil || h.Convs == nil {
		t.Fatalf("codex descriptor missing a contract: %+v", h)
	}
	if h.SupportsRename() {
		t.Fatalf("codex has no in-pane rename command")
	}
	if !h.SupportsCompact() {
		t.Fatalf("codex must support compact (/compact)")
	}
	if !h.SupportsSoftExit() {
		t.Fatalf("codex must support soft-exit (/quit) for graceful stop")
	}
	if got := h.Life.CompactCommand(); got != "/compact" {
		t.Fatalf("codex compact command = %q, want /compact", got)
	}
	if got := h.Life.SoftExitCommand(); got != "/quit" {
		t.Fatalf("codex soft-exit command = %q, want /quit", got)
	}
	if h.SupportsRemoteControl() || h.CanRemoteControl() {
		t.Fatalf("codex must NOT support remote control (no built-in remote access)")
	}
	if got := h.Life.RemoteControlCommand(); got != "" {
		t.Fatalf("codex remote-control command = %q, want empty", got)
	}
	if h.Spawn.Binary() != "codex" {
		t.Fatalf("codex binary = %q", h.Spawn.Binary())
	}
}
