package common

// SpawnArgs carries the parameters for a detached `tclaude session new`
// invocation — the agentd Spawner seam (`Spawner.SpawnNew`/`SpawnResume`, the
// `SpawnDetachedTclaude*` facades, and the `sessionNewArgs`/`sessionResumeArgs`
// builders). It collapses what had grown to 8–9 positional args into named
// fields, so the next knob is a new field rather than another positional
// (JOH-203).
//
// One struct serves both the fresh-spawn and resume paths; the few fields that
// apply to only one are documented as such. It lives in this shared package —
// not next to the Spawner interface in agentd — because the testharness
// Spawner mock (simSpawner) must name the same parameter type to satisfy the
// interface, yet cannot import agentd (agentd's internal tests import
// testharness, which would be a cycle). Both agentd and testharness already
// import this package, so it is the cycle-free home.
//
// Field semantics carried over from the old positional signature:
//   - An empty string / false omits the corresponding `tclaude session new`
//     flag entirely, leaving the harness on its own default.
//   - Every value reaching a live spawn was validated at the spawn boundary
//     (handleGroupSpawn / the `agent spawn` CLI); the forked `tclaude session
//     new` re-validates.
type SpawnArgs struct {
	// Label is the tclaude-side session ID for a fresh spawn (SpawnNew): the
	// stable key the hook callback tracks conv-id rotations against, and the
	// row key in SQLite. It must be unique in the sessions table. Unused by
	// SpawnResume (a resume mints its own fresh label).
	Label string

	// ConvID is the conversation to relaunch (SpawnResume). Unused by SpawnNew.
	ConvID string

	// SessionID is a caller-chosen conversation id for a fresh spawn (SpawnNew),
	// forwarded as `tclaude session new --session-id <uuid>` so Claude Code uses
	// it (`claude --session-id`) and tclaude knows the conv-id before the pane
	// starts. The daemon sets it on the launch-enrollment path so the agent is
	// enrolled and named via launch args instead of post-connect tmux injection.
	// "" mints the harness's own id at launch. A valid UUID when set; mutually
	// exclusive with ConvID (a resume already knows its id). Unused by SpawnResume.
	SessionID string

	// Name is the display name for a fresh spawn (SpawnNew), forwarded as
	// `tclaude session new --name <name>` so Claude Code applies it at launch
	// (`claude --name`, recorded as a custom-title turn). "" omits it. Unused by
	// SpawnResume.
	Name string

	// InitialPrompt is the first-turn prompt the launched harness submits to
	// itself, forwarded as `tclaude session new --initial-prompt <text>` →
	// the harness's positional [prompt]. On the launch-enrollment path it
	// carries the agent's welcome turn so it need not be injected over tmux.
	// "" omits it. Fresh-spawn only (SpawnNew); a resume takes no launch prompt.
	InitialPrompt string

	// Cwd is the working directory to launch in; "" omits -C.
	Cwd string

	// Effort is the reasoning-effort flag; "" omits --effort. Resume surfaces
	// pass the predecessor's inherited effort so the agent stays on it
	// (`claude --resume` does not restore it on its own).
	Effort string

	// Model is the model flag; "" omits --model so the harness resolves its own
	// default. Resume surfaces pass the predecessor's inherited model for the
	// same reason as Effort.
	Model string

	// Harness is the harness name to launch ("claude", "codex"); "" or "claude"
	// omits --harness and spawns Claude Code, keeping an untagged spawn's argv
	// exactly as before harness support (JOH-160).
	Harness string

	// Sandbox is the launch-time OS-sandbox mode for harnesses that take one
	// (Codex's --sandbox, or the managed-profile pseudo-mode); "" omits it. The
	// daemon resolves it to the harness's secure default before spawning so a
	// daemon-owned Codex agent runs sandboxed by default (JOH-192/JOH-207).
	Sandbox string

	// AskUserQuestionTimeout is the per-session Claude Code AskUserQuestion
	// idle-timeout override (never|60s|5m|10m), forwarded as `tclaude session
	// new --ask-user-question-timeout <v>`; "" omits it. A Claude-Code-only
	// settings.json override delivered via `--settings`; harnesses with no
	// AskUserQuestion dialog (Codex) ignore it. Enabling auto-continue is an
	// explicit per-agent / per-profile opt-in, so the daemon never defaults it.
	AskUserQuestionTimeout string

	// Approval is the launch-time approval policy for harnesses that take one
	// (Codex's --ask-for-approval); "" omits it. The daemon resolves it to the
	// harness's non-escalating default so a detached Codex agent never
	// deadlocks on an approval prompt no one can answer (JOH-200).
	Approval string

	// AutoReview opts the spawn into the harness's guardian subagent (Codex's
	// `-c approvals_reviewer=auto_review`); false (the default) leaves the human
	// as reviewer. Experimental/undocumented-upstream opt-in, only ever true via
	// an explicit request — relaunch paths (resume/clone/reincarnate) pass false
	// (JOH-200 part 2).
	AutoReview bool

	// TrustDir pre-trusts the launch dir for Codex (SpawnNew only): the forked
	// `tclaude session new --trust-dir` writes the trust entry into
	// ~/.codex/config.toml before launch so a detached pane doesn't freeze on the
	// trust-folder modal (JOH-205). false (the default) leaves the modal in place.
	// Fresh-spawn-only — resume paths leave it false (a resumed conv's dir was
	// already its own at first launch), so SpawnResume ignores it.
	TrustDir bool

	// RemoteControl arms the harness's built-in Remote Access at launch
	// (Claude Code's --remote-control), forwarded as `tclaude session new
	// --remote-control` so the agent is reachable from claude.ai/code + the
	// Claude app from its first turn (JOH-258). false (the default) omits it.
	// Harnesses with no Remote Access (Codex) ignore it. The daemon also tags
	// sessions.remote_control=1 out-of-band once the row materialises, so the
	// toggle's direction logic + dashboard indicator start armed.
	RemoteControl bool
}
