package harness

// AskSpec is a harness-agnostic description of a one-shot "ask" turn — the
// primitive behind `tclaude ask` (project tclaude-ask, JOH-250). Unlike a
// SpawnSpec (which describes a long-lived tmux-pane session), an ask runs a
// single question against the harness as a FOREGROUND child of the caller's
// shell, attached to the caller's terminal, and either streams its answer
// interactively (the agent can ask the human back, do work, etc.) or prints
// a captured answer and exits (`-p`, for `git diff | ai "…"` / `$(ai "…")`).
//
// The conversation is persisted and resumed per (terminal, cwd) so repeated
// questions in the same place continue one thread — so an AskSpec carries
// EITHER a ResumeID (continue) XOR a SessionID (mint a fresh conv with a
// caller-chosen id, the same `--session-id` trick the spawn path uses to
// know the conv-id before launch). Fields left at their zero value are
// omitted, so "unset" reliably means "let the harness use its own default".
type AskSpec struct {
	// ResumeID continues an existing conversation (Claude Code's
	// `--resume <id>`). Mutually exclusive with SessionID.
	ResumeID string
	// SessionID pins the conv-id of a FRESH ask thread (Claude Code's
	// `--session-id <uuid>`), so the caller can record the (terminal,cwd)→
	// conv mapping it just minted. Must be a valid UUID. Mutually exclusive
	// with ResumeID.
	SessionID string
	// Model is a validated, normalized model token, or "" to omit the flag
	// (let the harness use the user's configured default). Validate via
	// ModelCatalog.ValidateModel first.
	Model string
	// Effort is a validated, normalized reasoning-effort level, or "" to
	// omit the flag (let the harness use its own default). Validate via
	// ModelCatalog.ValidateEffort first. `tclaude ask` resolves it from
	// the per-call --effort flag, the config ask profile, then the
	// fast-by-default constant (JOH-253).
	Effort string
	// Print selects non-interactive capture mode (Claude Code's `-p`): the
	// harness prints its answer to stdout and exits, taking no further input.
	// false runs the harness interactively, attached to the caller's TTY.
	Print bool
	// Prompt is the question, already assembled by the caller (it folds in
	// any piped stdin payload). The Asker emits it as the harness's single
	// positional prompt argument, shell-quoted, so it is never split into
	// stray flags/words.
	Prompt string
}

// Asker builds the argv that `tclaude ask` execs to put a single question to
// a harness. It is a capability-segregated contract like Spawner: a harness
// that cannot answer ad-hoc questions leaves Harness.Ask nil and callers gate
// on SupportsAsk.
//
// Unlike Spawner.BuildCommand (which returns a `sh -c` STRING for a tmux
// pane), an ask is exec'd directly with no shell, so this returns an ARGV
// ([]string): argv[0] is the binary, the rest are already-separated args.
// That keeps the question safe without shell-quoting — it is one element of
// the slice, never concatenated into a command line.
type Asker interface {
	// BuildAskArgv returns the argv (binary + args) to exec for one ask turn.
	BuildAskArgv(spec AskSpec) []string

	// PreMintsConvID reports whether this harness lets the caller pin a
	// FRESH ask's conv-id up front (Claude Code's `--session-id <uuid>`, fed
	// via AskSpec.SessionID), so `tclaude ask` can record the
	// (terminal,cwd)→conv mapping immediately. Codex generates its conv-id at
	// the first turn and only exposes it afterwards (rollout file / threads
	// store — JOH-205), so it returns false and ignores AskSpec.SessionID; the
	// ask flow then discovers the id post-run from the harness's ConvStore and
	// records the mapping after the fact. See Harness.PreMintsAskConvID.
	PreMintsConvID() bool
}
