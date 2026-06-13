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
// `session new --harness codex` launch a Codex TUI in tmux, the
// HookInstaller (JOH-157/158), and a Lifecycle whose only in-pane command
// is the soft-exit `/quit` (JOH-160 — graceful stop for daemon-owned Codex
// agents). Rename/compact stay unsupported: Codex has no `/rename`-style
// command, so SupportsRename folds to false and agentd routes a Codex
// rename through ConvStore.SetTitle (a JOH-161 stub today); compact is out
// of JOH-160's scope.
func init() {
	Register(&Harness{
		Name:        CodexName,
		DisplayName: "Codex CLI",
		Spawn:       codexSpawner{},
		Models:      codexModels{},
		Convs:       codexConvStore{},
		Hooks:       codexHookInstaller{},
		Life:        codexLifecycle{},
		Sandbox:     codexSandbox{},
		Approval:    codexApproval{},
	})
}

// codexLifecycle names Codex CLI's in-pane control slash commands. Only the
// soft-exit is wired: Codex registers a `/quit` TUI slash command that ends
// the session in one shot (verified against openai/codex @ rust-v0.139.0 —
// slash_dispatch.rs routes Quit to request_quit_without_confirmation, so
// there is no confirm prompt and the injection mirrors Claude Code's
// `/exit`: a single command + Enter). RenameCommand and CompactCommand are
// empty: Codex has no in-pane rename (titles live in its threads state DB,
// reached via ConvStore — JOH-161), and compact is not part of JOH-160.
// The token is a compile-time constant — never interpolate user input into
// it (the tmux pane is an injection sink).
type codexLifecycle struct{}

func (codexLifecycle) RenameCommand() string   { return "" }
func (codexLifecycle) CompactCommand() string  { return "" }
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
