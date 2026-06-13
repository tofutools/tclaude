package harness

import "fmt"

// SetTitle is the write counterpart to codexConvStore's reads. Codex's
// title lives in the threads state DB (~/.codex/state_5.sqlite, threads.title),
// so a rename here is a direct row write — NOT an in-pane slash injection
// (Codex has no TUI rename command). agentd's rename dispatch routes a
// Codex conversation here because the Codex harness exposes no
// Lifecycle.RenameCommand.
//
// It is a STUB for now and deliberately errors rather than writing: the
// reads open the state DB read-only (mode=ro) on purpose, and a write to
// the user's LIVE Codex state DB is unexercised until a Codex agent is
// actually renameable (M2/M4). The real writer lands in JOH-161, where it
// is exercised + testable; until then a Codex rename fails gracefully,
// which is correct — no Codex session can be renamed yet.
//
// Lives in its own file (not codex.go / codex_convstore.go) so this write
// stub stays separable from the Codex reader the parser slice (JOH-152)
// owns.
func (codexConvStore) SetTitle(convID, title string) error {
	return fmt.Errorf("codex: out-of-band rename not yet wired (JOH-161)")
}
