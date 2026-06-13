package harness

import (
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// codexHarnessName is the stable identifier Codex conversations are tagged
// with, in the DB `harness` column and on every SessionEntry the Codex
// ConvStore returns.
const codexHarnessName = "codex"

// init registers the OpenAI Codex CLI harness. For now it provides ONLY
// the ConvStore (read conversations from Codex's rollout files + threads
// state DB); Spawn/Models/Life are deferred to their own slices, so the
// capability helpers fold the nil sub-contracts to false and no spawn path
// can select Codex yet (session new still goes through harness.Default()).
// Registering it makes `harness.Resolve("codex")` succeed and lets the
// multi-harness conv enumeration (JOH-153) reach this store.
func init() {
	Register(&Harness{
		Name:        codexHarnessName,
		DisplayName: "Codex CLI",
		Convs:       codexConvStore{},
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
