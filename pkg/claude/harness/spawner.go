package harness

// SpawnSpec is a harness-agnostic description of a session launch. The
// caller (e.g. `tclaude session new`) owns the env and resolves the
// resume id; the Spawner turns this into the concrete shell command the
// tmux pane runs. Fields left at their zero value are omitted from the
// command, so "unset" reliably means "let the harness use its own
// default".
type SpawnSpec struct {
	// EnvExports is a pre-built `export K=V; …` prefix prepended verbatim
	// to the command. The caller assembles it (tclaude identity env +
	// any pass-through), so the Spawner stays agnostic about which vars
	// matter to which harness.
	EnvExports string
	// ResumeID is the full conversation id to resume, or "" to start a
	// fresh session. The flag/sub-command form is harness-specific
	// (`claude --resume <id>` vs `codex resume <id>`).
	ResumeID string
	// Effort is a validated, normalized reasoning-effort token, or "" to
	// omit the flag entirely. Validate via ModelCatalog.ValidateEffort
	// before building the spec.
	Effort string
	// Model is a validated, normalized model token, or "" to omit the
	// flag entirely. Validate via ModelCatalog.ValidateModel first.
	Model string
	// ExtraArgs are post-`--` pass-through args, appended last and
	// shell-quoted individually by the Spawner.
	ExtraArgs []string
}

// Spawner builds the in-tmux launch command for a harness from a
// SpawnSpec, and names the harness binary for the pass-through path
// (`--help`/`--version`, run directly without tmux).
type Spawner interface {
	// Binary is the executable name (e.g. "claude", "codex").
	Binary() string
	// BuildCommand returns the shell command string the tmux pane runs
	// under `sh -c`. The result must be safe to hand to `sh -c`: any
	// value that could carry shell metacharacters (model aliases with
	// `[1m]` brackets, pass-through args) is shell-quoted by the
	// implementation.
	BuildCommand(spec SpawnSpec) string
}
