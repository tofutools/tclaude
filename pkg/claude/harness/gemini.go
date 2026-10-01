package harness

// GeminiName is the stable identifier persisted for Google Gemini CLI sessions
// and accepted by `--harness gemini`.
const GeminiName = "gemini"

// GeminiPinnedVersion is the Gemini CLI release every claim in this adapter was
// checked against. No Gemini account was available when the adapter was
// written, so nothing here was observed from a model turn. The evidence is
// two things that need no account: the published npm package itself (its
// `--help` output) and the upstream source tree at the matching tag
// (github.com/google-gemini/gemini-cli, v0.62.0). Behavior that only a live
// turn could prove is called out as such where it matters.
const GeminiPinnedVersion = "0.62.0"

// The first Gemini wave is the MINIMUM BAR from docs/adding-a-harness.md: a
// Spawner, a ModelCatalog and the lifecycle tokens, plus LaunchEnrollment.
// Every other contract stays nil until a later wave backs it.
//
// The bar is the same one the Copilot adapter started from, and for the same
// reason: a launch flag read from the CLI's own argument parser is a contract,
// while a storage layout, a hook payload or an approval semantics claim needs
// more than a flag list before tclaude may advertise it. Callers gate on the
// Supports* helpers and degrade cleanly when a contract is absent; they cannot
// detect one that is present and wrong.
func init() {
	Register(&Harness{
		Name:        GeminiName,
		DisplayName: "Gemini CLI",
		Spawn:       geminiSpawner{},
		Models:      geminiModels{},
		Life:        geminiLifecycle{},

		// `--session-id <id>` starts a NEW session under a caller-chosen id
		// (packages/cli/src/config/config.ts; gemini.tsx resolveSessionId
		// refuses an id that already exists rather than resuming it), and
		// `-i <prompt>` submits the first turn at launch. That is the Claude
		// Code shape: the conv-id is known before the pane starts, so the
		// daemon can enroll the agent first. SeedsFirstTurn stays false — the
		// id does not depend on a turn having run.
		//
		// Unlike Claude Code and Copilot there is no launch-time NAME flag, so
		// a spawn's name is carried by tclaude's own conversation index
		// instead; see geminiSpawner.BuildCommand.
		LaunchEnrollment: true,

		// Gemini CLI is an Ink TUI that renders its own scroll-back (it has a
		// dedicated copy mode and mouse handling), so tmux mouse mode would
		// fight it exactly as it would for Claude Code.
		TmuxScrollback: false,
	})
}

// geminiLifecycle names Gemini CLI's in-pane control commands. They are
// compile-time constants because tclaude types them into a tmux pane (an
// injection sink) — never interpolate user input into them. Every token was
// read from the built-in command table in packages/cli/src/ui/commands.
type geminiLifecycle struct{}

// Gemini CLI has no rename command and no user-settable session title. The
// session's only label is `summary`, which Gemini generates itself. Rename
// therefore has no in-pane path; it is delivered out of band once a ConvStore
// exists (see CanRename).
func (geminiLifecycle) RenameCommand() string { return "" }

// `/compress` summarizes the chat history in place (compressCommand.ts; its
// altNames include `compact`). The canonical name is used rather than the
// alias so a future CLI dropping the alias cannot silently break compaction.
func (geminiLifecycle) CompactCommand() string { return "/compress" }

// `/quit` exits the CLI (quitCommand.ts; altNames `exit`).
func (geminiLifecycle) SoftExitCommand() string { return "/quit" }

// Gemini CLI has no hosted remote-access relay. (Its `/ide` integration and
// `--acp` mode are local transports, not a remote-control toggle.)
func (geminiLifecycle) RemoteControlCommand() string { return "" }
func (geminiLifecycle) FastModeCommand() string      { return "" }

// A typed `/quit` is only read from the input prompt, and Ctrl+C is the key
// that gets the TUI there from any state: AppContainer's QUIT binding cancels
// an ongoing request on every press, and the input prompt's own Ctrl+C binding
// clears a half-typed line. One press is therefore enough to put the typed
// command on an empty prompt; it is not enough to quit (that takes a second
// press inside the 3 s window, see SignalExitKeys).
func (geminiLifecycle) SoftExitPrefixKeys() []string { return []string{"C-c"} }

// Gemini CLI's keystroke-free soft exit, read from AppContainer.tsx rather
// than measured in a pane (no account was available to start one): every
// Ctrl+C cancels the in-flight request and counts toward a repeated-press
// window of WARNING_PROMPT_DURATION_MS (3 s); the second press inside that
// window runs `/quit` through the CLI's normal exit path.
//
// Escape leads for the same reason it does for Claude Code: it dismisses an
// open dialog (a tool-confirmation prompt included) without selecting its
// default entry, so the Ctrl+C presses that follow land on the main app rather
// than on a dialog. The third Ctrl+C is margin for a press that lands after
// the window lapses under load; a surplus press on a pane that already exited
// is tolerated by the injector.
func (geminiLifecycle) SignalExitKeys() []string {
	return []string{"Escape", "C-c", "C-c", "C-c"}
}
