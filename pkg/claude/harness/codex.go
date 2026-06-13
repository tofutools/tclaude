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
// codex_convstore.go) plus the Spawner + ModelCatalog (JOH-154) that let
// `session new --harness codex` launch a Codex TUI in tmux. Life (in-pane
// lifecycle slash commands) is still nil — Codex has no `/rename`-style
// commands, so SupportsRename/Compact/SoftExit fold to false and agentd
// routes a Codex rename through ConvStore.SetTitle (a JOH-161 stub today).
func init() {
	Register(&Harness{
		Name:        CodexName,
		DisplayName: "Codex CLI",
		Spawn:       codexSpawner{},
		Models:      codexModels{},
		Convs:       codexConvStore{},
		Hooks:       codexHookInstaller{},
	})
}

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
