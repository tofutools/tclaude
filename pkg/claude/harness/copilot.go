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
// commands — plus, since TCL-972, hooks and, since TCL-976, a cold ConvStore,
// both of which the fixture lab promoted from "undocumented" to "observed from
// the real binary" — plus, since TCL-978, Sandbox and ModelTransport, and,
// since TCL-973, directory trust and the approval catalog, both promoted on the
// same terms from real-pty measurements: the startup trust modal and the
// permission matrix — plus, since TCL-994, the one-shot Ask surface, promoted on
// the same evidence terms from the headless `-p` form. ToolGovernance is still
// left nil for a later, fixture-backed wave (TCL-965 phases 2-5).
func init() {
	Register(&Harness{
		Name:        CopilotName,
		DisplayName: "GitHub Copilot CLI",
		Spawn:       copilotSpawner{},
		Models:      copilotModels{},
		Life:        copilotLifecycle{},

		// TCL-994: the one-shot `tclaude ask` surface, buffered only. The
		// headless `-p` form was measured to put the answer ALONE on stdout, to
		// continue an exact conversation under `--resume=<uuid>`, and to land a
		// fresh `--session-id` conversation in the ConvStore above — so ask
		// threads are listable and resumable like any other conversation.
		//
		// StreamAsker is deliberately NOT implemented, so SupportsAskStream stays
		// false and `tclaude ask` keeps its buffered path. Copilot does have an
		// incremental surface (`--output-format json` JSONL), but rendering it as
		// live text is a second contract — an event-stream wire format tclaude
		// would have to parse — and this wave contracts only what it measured.
		Ask: copilotAsker{},

		// The cold conversation store reads only Copilot's own per-session
		// files under <COPILOT_HOME>/session-state — see copilot_convstore.go
		// for why that needs no SQLite access at all.
		Convs: copilotConvStore{},

		// TCL-978 promotes the sandbox contracts out of the
		// documentation-only wave, on the same terms as Hooks and Convs below:
		// every claim is backed by the pinned binary or its shipped runtime,
		// not by the published docs, which describe neither.
		//
		// Sandbox is an ASSERT-off catalog rather than a launch flag. Copilot
		// 1.0.77 has no environment variable for its own (experimental, MXC)
		// command sandbox, and its hidden `--sandbox`/`--no-sandbox` flags are
		// honoured only under `--experimental` — the same switch that lets the
		// pane revoke the posture — so tclaude cannot force it either way
		// without handing the pane that lever. What
		// the catalog buys is the ability to REFUSE a tclaude-layer launch that
		// would otherwise stack two claimed boundaries; see copilot_sandbox.go.
		Sandbox: copilotSandbox{},

		// The posture tclaude-layer launches under: Copilot's own wall
		// asserted off, so tclaude's outer layer is the single boundary.
		TclaudeLayerMode: CopilotSandboxOff,

		// BuiltinOSSandbox stays FALSE, and the distinction matters. Copilot
		// does ship a real OS sandbox (MXC over bubblewrap/Seatbelt), but this
		// flag means "the harness owns an OS-enforced sandbox BEHIND ITS
		// Sandbox CATALOG", and this catalog's modes do not select it — they
		// only assert it is off. Setting it true would advertise a boundary
		// tclaude has no lever for. Enabling Copilot's own sandbox is TCL-977.
		BuiltinOSSandbox: false,

		// The filtered-network model route: the default first-party GitHub
		// Copilot service only, with every route-moving input refused rather
		// than followed. See copilot_model_transport.go for where the hosts
		// come from.
		ModelTransport: copilotModelTransport{},

		// Hooks are the first contract to graduate out of the
		// documentation-only wave above, because they are the first one a
		// real binary could be made to prove. copilot_hooks.go records what
		// the pinned 1.0.77 CLI actually does: a tclaude-owned drop-in file
		// under COPILOT_HOME fires, and registering Claude Code's event names
		// makes Copilot emit Claude Code's payload — so live status needs an
		// installer and nothing else. Everything still nil below stays nil.
		Hooks: copilotHookInstaller{},

		// TCL-973: Copilot has the same startup folder-trust modal Codex and
		// Claude Code do, and it is STRICTLY EARLIER than theirs in
		// consequence — measured on a real pty with a fresh COPILOT_HOME, the
		// pane parks on it before the CLI contacts the provider at all, so an
		// unattended agent never reaches its first turn.
		//
		// The flag is set here because the contract behind it is now wired:
		// copilot_dir_trust.go seeds `trustedFolders` in COPILOT_HOME's
		// config.json, which is the ONLY input measured to clear the modal
		// short of COPILOT_ALLOW_ALL=true (a blanket tool/path/URL promotion
		// tclaude will not make on an operator's behalf). No launch flag
		// clears it. What this flag governs is what a spawn can START; what it
		// is then allowed to DO is the separate approval axis below. Neither
		// axis reaches Ask: a headless `-p` capture answers questions rather
		// than starting a pane, and it neither meets the trust modal nor emits a
		// permission flag — see copilot_asker.go.
		DirTrust: true,

		// TCL-973's other half, and the other side of that START/DO split.
		// Promoted on the same evidence terms: every flag the catalog
		// renders was measured against the pinned 1.0.77 binary, and
		// the plan's proposed default is not in it — `--deny-tool 'url()'` was
		// DISPROVEN (rejected at parse, exit 1). Two tokens only, `inherit` and
		// `allow-tools`; see copilot_approval.go for what each one closes and,
		// more importantly, what it does not.
		//
		// Ask, AskTimeout, ToolGovernance and ApprovalsReviewer stay nil, and
		// `--no-ask-user` belonging to this catalog is not an argument for
		// changing that. AskTimeout contracts an idle TIMEOUT after which an
		// unanswered question auto-continues with its default answer; Copilot
		// has no such setting — the flag removes the ask_user tool outright, so
		// there is no dialog left to time out. Advertising AskTimeout would
		// require inventing a translation, which is the one thing this adapter
		// has consistently refused to do.
		Approval: copilotApproval{},

		// Copilot's SessionEnd is not proof of an exit: observed only on clean
		// runs, impossible on a SIGKILL, and at-least-once rather than
		// exactly-once. Without this, every SessionEnd would declare a live
		// pane dead — see the field's doc comment.
		SessionEndBestEffort: true,

		// Copilot announces the session AFTER the prompt: the recorded event
		// order is UserPromptSubmit, UserPromptTransformed, SessionStart, …
		// (copilotfixture/testdata/<version>/hooks). Every other harness does
		// the opposite, and the status machine's SessionStart handling assumed
		// it, so this flag is what stops a late SessionStart from reporting a
		// busy agent as idle for the rest of its first turn.
		SessionStartAfterPrompt: true,

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

		// BuiltinOSSandbox stays false, and TCL-977 is the evaluation that
		// decided so rather than the absence of one. Copilot really does own an
		// OS sandbox for shell commands, which is exactly why the refusal needs
		// to say WHICH property is missing — see copilot_sandbox_native.go.
		BuiltinOSSandboxAbsenceReason: CopilotBuiltinOSSandboxAbsenceReason,
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

// Copilot's TUI only accepts a slash command when it is not mid-turn, so the
// soft exit is preceded by a cancel. Measured against 1.0.77 in a real tmux
// pane (see copilotfixture): mid-turn or while a tool runs, C-c reports
// "Operation cancelled by user" and returns the TUI to its input prompt, from
// which /exit exits 0; with a permission dialog open C-c ABORTS the request
// (the pending command never runs) rather than accepting its default entry;
// on an idle pane it is a no-op, and on a pane holding a half-typed line it
// clears the buffer — which incidentally removes the "<junk>/exit submitted as
// one prompt" failure the soft-exit retry exists to recover from.
//
// Escape is deliberately NOT used: the CLI holds a lone ESC byte waiting for
// the rest of a possible escape sequence, so a trailing Escape is never
// delivered at all.
func (copilotLifecycle) SoftExitPrefixKeys() []string { return []string{"C-c"} }
