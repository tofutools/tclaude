package harness

import (
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// CodexName is the stable identifier Codex conversations are tagged with,
// in the DB `harness` column and on every SessionEntry the Codex ConvStore
// returns. Exported so callers outside this package (e.g. the session hook
// callback) can recognise a Codex session row without re-spelling "codex".
const CodexName = "codex"

// init registers the OpenAI Codex CLI harness. It provides the ConvStore
// (read conversations from Codex's rollout files + threads state DB; see
// codex_convstore.go), the Spawner + ModelCatalog (JOH-154) that let
// `session new --harness codex` launch a Codex TUI in tmux, the Asker
// (`tclaude ask` one-shot turns via `codex exec` / the TUI — JOH-252; see
// codex_asker.go), the HookInstaller (JOH-157/158), and a Lifecycle for
// Codex's in-pane controls.
// Rename stays out-of-band: Codex has no `/rename`-style command, so
// SupportsRename folds to false and agentd routes a Codex rename through
// ConvStore.SetTitle.
func init() {
	Register(&Harness{
		Name:          CodexName,
		DisplayName:   "Codex CLI",
		Spawn:         codexSpawner{},
		Ask:           codexAsker{},
		OneShotReplay: OneShotReplayCodex,
		Models:        codexModels{},
		ModelTransport: staticModelTransport{
			provider:    "openai",
			template:    "net-openai-codex",
			baseURLHost: "api.openai.com",
			destinations: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.openai.com", Ports: []int{443}},
			},
		},
		Convs:            codexConvStore{},
		Hooks:            codexHookInstaller{},
		Life:             codexLifecycle{},
		Sandbox:          codexSandbox{},
		TclaudeLayerMode: SandboxDangerFull,
		BuiltinOSSandbox: true,
		NestedSandbox:    codexNestedSandbox{},
		Approval:         codexApproval{},
		// Codex has a guardian/reviewer subagent the experimental --auto-review
		// opt-in can route approval prompts to (approvals_reviewer=auto_review).
		// Claude Code has no such reviewer, so only Codex sets this; it is the
		// gate for --auto-review, distinct from the Approval catalog. JOH-200 pt2.
		ApprovalsReviewer:        true,
		AwaitingInputObservation: true,
		// Codex's TUI scrolls through the terminal rather than rendering its
		// own scrollback, so a tmux pane needs mouse mode on for the wheel to
		// reach history (Claude Code, which owns its scrollback, leaves this
		// off). See JOH-213 + session.ConfigureTmuxScrollback.
		TmuxScrollback: true,
		// Codex only persists+exposes its conv-id once a turn runs (JOH-205), so
		// a daemon-spawned Codex pane needs a positional first-turn prompt — and
		// that prompt carries the [system: ...] welcome (see executeSpawn /
		// buildSpawnSeedPrompt). Claude Code reports its id via the SessionStart
		// hook and leaves this false.
		SeedsFirstTurn: true,
		// Codex can use operator-arranged transports that require no IP network
		// inside the tclaude layer. Each isolated launch still needs the
		// explicit TCLAUDE_OFFLINE_MODEL=1 profile assertion; tclaude does not
		// infer this from --oss because host-loopback providers are outside a
		// freshly unshared network namespace.
		OfflineModelTransport: true,
		// Codex blocks a first launch in an unseen dir on its trust-folder
		// modal, recording the answer as a [projects."<dir>"] trust_level table
		// in ~/.codex/config.toml — seedable ahead of launch, so the trust-dir
		// opt-in applies. See codex_dir_trust.go.
		DirTrust: true,
	})
}

// codexLifecycle names Codex CLI's in-pane control slash commands. Codex
// exposes `/compact` for context summarisation and `/quit` for soft-exit.
// RenameCommand is empty because Codex has no in-pane rename; titles live in
// its threads state DB, reached via ConvStore.SetTitle. RemoteControlCommand
// is empty because Codex has no built-in remote-access feature (JOH-254).
// The tokens are compile-time constants — never interpolate user input into
// them (the tmux pane is an injection sink).
type codexLifecycle struct{}

func (codexLifecycle) RenameCommand() string        { return "" }
func (codexLifecycle) CompactCommand() string       { return "/compact" }
func (codexLifecycle) SoftExitCommand() string      { return "/quit" }
func (codexLifecycle) RemoteControlCommand() string { return "" }
func (codexLifecycle) FastModeCommand() string      { return "/fast" }

// Codex accepts /quit from its prompt without a preparatory key.
func (codexLifecycle) SoftExitPrefixKeys() []string { return nil }

// Codex CLI's keystroke-free soft exit: three ctrl-c presses, one settle apart.
// Measured in a real tmux pane against Codex 0.147.0 (TCL-1137):
//
//   - Its quit is a double ctrl-c and, unlike Claude Code, has NO tight
//     re-press window — a second press 0.5 s, 2 s or 5 s after the first all
//     exited cleanly (status 0). The first press on a live turn interrupts it
//     (writing a turn_aborted event, exactly as /quit does), so a third press
//     covers interrupt + quit; a surplus press lands on a dead pane and is
//     tolerated by the injector.
//   - It is equivalent to /quit for durable state. Codex persists its rollout
//     file INCREMENTALLY during the live session (the file is created at
//     startup and response items / event messages are appended as they occur),
//     and writes no distinct end-of-session marker — there is no Copilot-style
//     session.shutdown event that one exit path could write and the other skip.
//     So a ctrl-c exit and a /quit exit leave the same rollout on disk; if a
//     turn is in flight both abort it identically. (Byte-identical rollouts
//     after a fully completed turn could not be A/B'd in the sandbox — Codex's
//     bundled MCP boot and provider access made a clean completed turn
//     unreliable there — but the incremental-persistence structure leaves no
//     shutdown-only write for ctrl-c to lose.)
func (codexLifecycle) SignalExitKeys() []string {
	return []string{"C-c", "C-c", "C-c", "C-c"}
}

// codexConvStore assembles conversations from Codex's split storage model.
// The methods are thin wrappers that resolve Codex's effective state root and delegate to the
// interface-free helpers in codex_convstore.go (which take an explicit
// home so they unit-test against a temp HOME).
type codexConvStore struct{}

var _ ConvStore = codexConvStore{}

// ListConvs returns the Codex conversations for cwd, or — when cwd is the
// empty sentinel — every Codex conversation across all working
// directories.
func (codexConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	configDir, err := codexConfigDir()
	if err != nil {
		return nil, err
	}
	return scanCodexEntriesAt(configDir, cwd)
}

// Resolve maps an id prefix to a Codex conversation, distinguishing
// no-match / unreadable-store / ambiguous-prefix per the ConvStore
// contract.
func (codexConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	configDir, err := codexConfigDir()
	if err != nil {
		return nil, err
	}
	return resolveCodexAt(configDir, idPrefix, cwd, global)
}

// Title returns a Codex conversation's display title, or ("", nil) for an
// unknown conv.
func (codexConvStore) Title(convID string) (string, error) {
	configDir, err := codexConfigDir()
	if err != nil {
		return "", err
	}
	return codexTitleAt(configDir, convID)
}

// Exists reports whether convID still has a rollout file under
// ~/.codex/sessions. Codex's store is globally indexed by id (not cwd-
// scoped), so cwd is ignored — the same id resolves from anywhere, mirroring
// `codex resume`. A located rollout is (true, nil); none is (false, nil); a
// scan error is (false, err) so the ask caller keeps the thread on a
// transient failure rather than self-healing. `tclaude ask` uses this to
// drop a stale (terminal,cwd)→conv mapping whose Codex conversation is gone.
func (codexConvStore) Exists(convID, _ string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	configDir, err := codexConfigDir()
	if err != nil {
		return false, err
	}
	path, err := findCodexRolloutAt(configDir, convID)
	if err != nil {
		return false, err
	}
	return path != "", nil
}
