package harness

import "io"

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
	// built-in default constant (JOH-253).
	Effort string
	// Print selects non-interactive capture mode (Claude Code's `-p`): the
	// harness prints its answer to stdout and exits, taking no further input.
	// false runs the harness interactively, attached to the caller's TTY.
	Print bool
	// Stream asks a print-mode turn to emit a machine-readable event stream
	// instead of one buffered answer, so a human watching a TTY sees the answer
	// build up live rather than appearing all at once at the end (Claude Code's
	// `--output-format stream-json` buffers nothing; the default `text` format
	// waits for the whole turn). Only meaningful with Print; a streaming Asker
	// (StreamAsker) builds the right flags and provides the StreamFilter that
	// turns that event stream back into clean incremental text. `tclaude ask`
	// sets it only when stdout is a real terminal (a pipe/capture reads the
	// answer whole regardless, so it keeps the simpler buffered path). Harnesses
	// that don't implement StreamAsker ignore it.
	Stream bool
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

	// NoisyCaptureStderr reports whether, in capture/print mode, this harness
	// writes a verbose human transcript to STDERR (session banner, hook
	// lifecycle lines, token counts) separate from the clean answer it writes
	// to stdout. When true, `tclaude ask` hides that stderr by default so a
	// captured answer is just the answer — surfacing it only on `--verbose` or
	// when the run fails (so a real error is never swallowed). Claude Code's
	// `-p` keeps stderr quiet already, so it returns false. Only consulted in
	// print mode; an interactive turn always passes stderr through.
	NoisyCaptureStderr() bool
}

// StreamAsker is an optional Asker capability: a harness that can emit a
// machine-readable EVENT STREAM in print mode (rather than one buffered answer)
// implements it so `tclaude ask` can render the answer incrementally to a TTY.
// Harnesses whose print mode already streams plain text — or that only buffer —
// leave it unimplemented; callers gate on Harness.SupportsAskStream and fall
// back to the plain buffered path.
//
// The two halves are deliberately coupled in one contract because they must
// agree: BuildAskArgv (given an AskSpec with Stream=true) emits the flags that
// turn on the event stream, and StreamFilter knows how to read exactly that
// stream back. Keeping both behind the harness means the generic ask flow never
// learns a harness's event-stream wire format.
type StreamAsker interface {
	Asker

	// StreamFilter wraps the caller's real stdout w and returns a writer that
	// `tclaude ask` makes the harness process's stdout. As the harness writes
	// its raw event stream into the returned writer, the filter parses it and
	// writes only the assistant's clean, incremental VISIBLE text to w — no
	// JSON, no reasoning/thinking, no tool-use chatter — so a captured answer
	// (`x=$(tclaude ask …)`) would still be just the answer, and a human
	// watching sees it stream.
	//
	// The returned writer may also implement AskStreamFlusher; `tclaude ask`
	// calls Flush once after the process exits so the filter can emit any
	// trailing text the stream implied but never streamed as deltas (a final
	// result or an error message) and a terminating newline.
	StreamFilter(w io.Writer) io.Writer
}

// AskStreamFlusher is the optional flush half of a StreamFilter's returned
// writer. `tclaude ask` type-asserts for it and calls Flush exactly once, after
// the harness process exits, regardless of whether the run succeeded — so the
// filter gets a chance to surface a buffered final answer/error and end the
// line cleanly. A filter that needs no end-of-stream work simply omits it.
type AskStreamFlusher interface {
	Flush() error
}
