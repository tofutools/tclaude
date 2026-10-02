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

// The first Gemini wave was the MINIMUM BAR from docs/adding-a-harness.md: a
// Spawner, a ModelCatalog and the lifecycle tokens, plus LaunchEnrollment. The
// second adds the cold ConvStore and the Ask surface, both read from the CLI's
// own storage and headless code paths; the third adds hooks. Every other
// contract stays nil until a later wave backs it.
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

		// The cold conversation store reads Gemini's own per-project chat
		// files (see gemini_convstore.go for the layout and the "what counts
		// as a conversation" rule it mirrors from the CLI). Gemini has no
		// title store, so SetTitle writes tclaude's conv_index overlay —
		// which is also what makes rename deliverable (CanRename) without an
		// in-pane command.
		Convs: geminiConvStore{},

		// One-shot `tclaude ask`, buffered only. Headless `--prompt=` writes
		// the answer to stdout and turns every ask_user approval into a deny,
		// and `--session-id` pins a fresh ask's id up front. StreamAsker is
		// deliberately not implemented: `--output-format stream-json` exists,
		// but parsing it is its own contract. See gemini_asker.go.
		Ask: geminiAsker{},

		// Live status through Gemini's settings.json hooks (see
		// gemini_hooks.go). The callback maps Gemini's event names onto
		// tclaude's vocabulary; nothing here needs a translator per field.
		Hooks: geminiHookInstaller{},

		// Gemini fires SessionEnd from its exit cleanup and explicitly does
		// not wait for it (hookSystem: "best effort"), and a killed process
		// never fires it at all. Exit detection stays with the reaper.
		SessionEndBestEffort: true,

		// The interactive app marks its config initialized BEFORE it awaits
		// the SessionStart hook (AppContainer.tsx), and the `-i` first turn is
		// submitted as soon as the config is initialized — so the launch
		// prompt's BeforeAgent can reach tclaude ahead of SessionStart.
		SessionStartAfterPrompt: true,

		// Folder trust is on by default and an untrusted folder parks the
		// pane on a dialog, switches settings hooks off and makes headless
		// runs fail. `--trust-dir` seeds trustedFolders.json (see
		// gemini_dir_trust.go).
		DirTrust: true,

		// Gemini's own sandbox has a real per-launch lever (GEMINI_SANDBOX
		// outranks the flag and settings.json), so `off` is enforced, not
		// asserted; tclaude-layer launches use it so tclaude's outer wall is
		// the single boundary. See gemini_sandbox.go.
		Sandbox:          geminiSandbox{},
		TclaudeLayerMode: GeminiSandboxOff,

		// `--session-id <id>` starts a NEW session under a caller-chosen id
		// (packages/cli/src/config/config.ts; gemini.tsx resolveSessionId
		// refuses an id that already exists rather than resuming it), and
		// `-i <prompt>` submits the first turn at launch. That is the Claude
		// Code shape: the conv-id is known before the pane starts, so the
		// daemon can enroll the agent first. SeedsFirstTurn stays false — the
		// id does not depend on a turn having run.
		//
		// Unlike Claude Code and Copilot there is no launch-time NAME flag, so
		// `session new` records a fresh launch's name in tclaude's own
		// conversation index instead (which the ConvStore overlays as the
		// custom title). `session new` also mints the id for a fresh launch
		// that was not handed one, so even a plain interactive session is
		// known by its conversation id from the start, before any hook fires.
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
// therefore has no in-pane path; it is delivered out of band through the
// ConvStore's tclaude-side title overlay (see CanRename).
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
