package harness

// CopilotName is the stable identifier persisted for GitHub Copilot CLI
// sessions and accepted by `--harness copilot`.
const CopilotName = "copilot"

// The first Copilot wave is deliberately the MINIMUM BAR from
// docs/adding-a-harness.md: a Spawner, a ModelCatalog and the lifecycle
// tokens. Every other contract stays nil.
//
// That is not an oversight. Copilot CLI was not installed in the development
// environment this adapter was written in, so the only evidence available was
// the official GitHub documentation (the command reference's command-line
// options table, its slash-command table and its supported-model list). A
// launch flag is a documented, stable contract; a session-state layout, a hook
// payload, a sandbox guarantee or an approval semantics claim is not — those
// need fixture-backed verification against a real binary before tclaude may
// advertise them. Declaring a contract tclaude cannot honor is worse than
// declaring none: callers gate on the Supports* helpers and degrade cleanly
// when a contract is absent, but they cannot detect one that lies.
//
// So this descriptor claims exactly what the documented CLI surface proves:
// launch, exact resume, model/effort selection, a pre-minted conversation id,
// a launch-time name, an initial submitted prompt, and three in-pane control
// commands. ConvStore, Hooks, Sandbox, Approval, ToolGovernance,
// ModelTransport, DirTrust and Ask are left nil for a later, fixture-backed
// wave (TCL-965 phases 2-5).
func init() {
	Register(&Harness{
		Name:        CopilotName,
		DisplayName: "GitHub Copilot CLI",
		Spawn:       copilotSpawner{},
		Models:      copilotModels{},
		Life:        copilotLifecycle{},

		// Copilot's conv-id is knowable before the pane starts: `--session-id
		// <uuid>` creates the session under a caller-chosen id, and `--name` /
		// `-i <prompt>` carry the title and the first turn as launch args. That
		// is the Claude Code shape, not the Codex one, so SeedsFirstTurn stays
		// false — the id does not depend on a turn having run.
		LaunchEnrollment: true,

		// Copilot renders its own scroll-back: mouse support is ON by default
		// and the CLI captures the wheel to scroll its own timeline. Enabling
		// tmux mouse mode would fight it, exactly as it would for Claude Code.
		// tclaude must not "fix" this with `--mouse=off` either — the docs state
		// an explicit `--mouse` value is PERSISTED to the user's configuration,
		// and a per-spawn flag that silently rewrites the operator's config is
		// not a per-spawn flag.
		TmuxScrollback: false,
	})
}

// copilotLifecycle names the in-pane control commands from Copilot CLI's
// documented slash-command table. They are compile-time constants because
// tclaude types them into a tmux pane (an injection sink) — never interpolate
// user input into them.
type copilotLifecycle struct{}

// `/rename [NAME]` renames the current session (alias for `/session rename`).
func (copilotLifecycle) RenameCommand() string { return "/rename" }

// `/compact [FOCUS-INSTRUCTIONS]` summarizes the conversation history.
func (copilotLifecycle) CompactCommand() string { return "/compact" }

// `/exit` closes the current session; with only tclaude's single session open
// it quits the CLI. Callers keep their hard-kill fallback for a pane that does
// not exit on its own.
func (copilotLifecycle) SoftExitCommand() string { return "/exit" }

// Copilot's remote access is `/remote [on|off]` — a DIRECTIONAL command, while
// RemoteControlCommand is contracted as a single token that flips the current
// state. Returning "/remote" would make tclaude's toggle send a bare status
// query and then report a state change that never happened, so remote control
// stays unsupported until the lifecycle contract itself grows a direction.
func (copilotLifecycle) RemoteControlCommand() string { return "" }
