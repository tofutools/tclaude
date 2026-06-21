package harness

import (
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
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
		Name:        CodexName,
		DisplayName: "Codex CLI",
		Spawn:       codexSpawner{},
		Ask:         codexAsker{},
		Models:      codexModels{},
		Convs:       codexConvStore{},
		Hooks:       codexHookInstaller{},
		Life:        codexLifecycle{},
		Sandbox:     codexSandbox{},
		Approval:    codexApproval{},
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
	})
}

// codexLifecycle names Codex CLI's in-pane control slash commands. Codex
// exposes `/compact` for context summarisation and `/quit` for soft-exit.
// RenameCommand is empty because Codex has no in-pane rename; titles live in
// its threads state DB, reached via ConvStore.SetTitle.
// The token is a compile-time constant — never interpolate user input into
// it (the tmux pane is an injection sink).
type codexLifecycle struct{}

func (codexLifecycle) RenameCommand() string   { return "" }
func (codexLifecycle) CompactCommand() string  { return "/compact" }
func (codexLifecycle) SoftExitCommand() string { return "/quit" }

// codexConvStore assembles conversations from Codex's split storage model.
// The methods are thin wrappers that resolve HOME and delegate to the
// interface-free helpers in codex_convstore.go (which take an explicit
// home so they unit-test against a temp HOME).
type codexConvStore struct{}

var _ ConvStore = codexConvStore{}

// ListConvs returns the Codex conversations for cwd, or — when cwd is the
// empty sentinel — every Codex conversation across all working
// directories.
func (codexConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return scanCodexEntries(home, cwd)
}

// Resolve maps an id prefix to a Codex conversation, distinguishing
// no-match / unreadable-store / ambiguous-prefix per the ConvStore
// contract.
func (codexConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return resolveCodex(home, idPrefix, cwd, global)
}

// Title returns a Codex conversation's display title, or ("", nil) for an
// unknown conv.
func (codexConvStore) Title(convID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return codexTitle(home, convID)
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
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path, err := findCodexRollout(home, convID)
	if err != nil {
		return false, err
	}
	return path != "", nil
}
