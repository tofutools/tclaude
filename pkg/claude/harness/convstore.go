package harness

import (
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// ConvRef is the minimal handle ConvStore.Resolve returns — just enough
// to locate and resume a conversation. Callers that need richer metadata
// (title, prompt, branch) go through ListConvs / Title.
type ConvRef struct {
	// ConvID is the full conversation id.
	ConvID string
	// ProjectPath is the REAL working directory the conversation belongs
	// to (the `--cd` / `--resume` target), not an encoded directory name.
	// For CC it's the cwd stamped onto the conv's turns; for Codex it's
	// SessionMeta.cwd / threads.cwd.
	ProjectPath string
	// Harness is which harness owns the conversation ("claude", "codex").
	Harness string
}

// ConvStore assembles conversation metadata from a harness's FULL storage
// model — never "parse the one file". Claude Code keeps everything
// (title/summary/cwd/branch) in a single cwd-indexed `.jsonl`; Codex
// splits it across date-indexed rollout files plus a sidecar state DB
// (~/.codex/state_5.sqlite `threads`). Each harness satisfies these the
// way its own model dictates; the result is always a convops.SessionEntry
// so every downstream reader (conv ls, search, dashboard) stays
// harness-agnostic.
//
// This slice is READ-ONLY. The write counterpart — SetTitle (CC injects
// `/rename` via the send-keys path; Codex writes threads.title directly)
// — is deferred to the Lifecycle/send-keys PR, where agentd stays the
// rename orchestrator gated on the Lifecycle capability flag and the
// injection sink gets its own cold review.
type ConvStore interface {
	// ListConvs returns the conversations for a working directory. An
	// empty cwd is the documented sentinel for "all conversations across
	// all working directories" (CC reads the whole conv_index cache;
	// Codex scans globally and skips the SessionMeta.cwd filter) — kept
	// as a sentinel rather than a second method to keep the surface
	// small.
	ListConvs(cwd string) ([]convops.SessionEntry, error)

	// Resolve maps a (possibly short) conversation-id prefix to a conv.
	// global widens the search beyond cwd's project.
	//
	// Returns (nil, nil) when nothing matches; (nil, err) when the lookup
	// fails (scan/IO error) OR the prefix is ambiguous (>1 match). These
	// are deliberately NOT collapsed into "not found": Codex's
	// date-indexed scan can fail, and a short prefix can hit several
	// rollouts — a caller must be able to tell "no such conv" from "be
	// more specific" / "the store is unreadable".
	Resolve(idPrefix, cwd string, global bool) (*ConvRef, error)

	// Title returns the conversation's display title from the harness's
	// title store: CC's customTitle turn (falling back to summary / first
	// prompt); Codex's threads.title (falling back to a title derived
	// from the first user message). An unknown conv yields ("", nil), not
	// an error.
	Title(convID string) (string, error)
}
