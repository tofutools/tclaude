package agentd

import (
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// harnessForConv resolves the harness a conversation's live session runs
// under, defaulting to the Claude Code harness when the harness is unknown
// or unregistered. Every in-pane slash injection in agentd sources its
// command tokens (/rename, /compact, /exit) from this harness's Lifecycle,
// so a pane is never typed a command the harness can't parse. Today every
// session is tagged "claude", so this is the CC harness and the tokens are
// the same literals as before the seam.
func harnessForConv(convID string) *harness.Harness {
	if rows, err := db.FindSessionsByConvID(convID); err == nil {
		for _, s := range rows {
			if s.Harness == "" {
				continue
			}
			if h, err := harness.Resolve(s.Harness); err == nil {
				return h
			}
			// An unknown tag (a harness this build doesn't register) falls
			// through to the default rather than failing the operation.
			slog.Warn("harnessForConv: unknown harness tag; defaulting to claude",
				"conv", convID, "harness", s.Harness)
			break
		}
	}
	return harness.Default()
}

// resolveSpawnHarness resolves a requested harness name for a daemon
// spawn, trimming surrounding whitespace first. It delegates to
// harness.ResolveSpawnable, so an empty name is the default (Claude Code)
// and an unknown or not-yet-spawnable harness is an error the spawn
// boundary surfaces as a 400 — rather than a silent failure once the
// forked session exits. The returned harness's Models is guaranteed
// non-nil so the caller can validate effort/model through it.
func resolveSpawnHarness(name string) (*harness.Harness, error) {
	return harness.ResolveSpawnable(strings.TrimSpace(name))
}

// harnessNativeTitle returns a conversation's current title from its
// harness's NATIVE title store, for harnesses that keep titles outside the
// Claude-Code conv_index / `.jsonl` (Codex's threads.title). The bool is
// false for the default (Claude Code) harness — whose title the callers
// already read through agent.FreshConvRow* / DisplayTitle — so a CC caller
// keeps its existing path byte-for-byte unchanged. An unreadable / empty
// native title also folds to ("", false), degrading to the caller's
// fallback rather than failing the lifecycle op.
//
// This is the read half of "title carry-over via SetTitle/Title": when a
// Codex agent is reincarnated or cloned, its predecessor title lives in
// threads.title, not the conv_index the CC path reads, so the carry must
// source it through the harness ConvStore.
func harnessNativeTitle(convID string) (string, bool) {
	h := harnessForConv(convID)
	if h.Name == harness.DefaultName || !h.SupportsConvs() {
		return "", false
	}
	title, err := h.Convs.Title(convID)
	if err != nil || title == "" {
		return "", false
	}
	return title, true
}

// sandboxForHarness returns the launch sandbox mode the daemon re-applies
// when it relaunches an existing conv (resume / clone / reincarnate). The
// mode is not persisted per-conv, so a Codex agent that was spawned
// sandboxed must be re-defaulted to its secure mode (workspace-write) on
// relaunch rather than coming back unsandboxed; a harness with no launch
// sandbox flag (Claude Code) or an unknown tag yields "" (omit the flag).
// A $HOME cwd is still re-checked by the forked `tclaude session new`'s own
// guard, so this needn't repeat it.
//
// Deliberate: this always re-defaults to the secure mode and does NOT
// preserve a per-conv override — an agent originally spawned with an explicit
// danger-full-access comes back sandboxed. That is the fail-closed direction
// (a relaunch tightening, never loosening, the sandbox), so it is intentional,
// not a dropped-state bug. Per-conv mode persistence, if ever wanted, would be
// a separate feature.
func sandboxForHarness(name string) string {
	if h, err := harness.Resolve(strings.TrimSpace(name)); err == nil && h.SupportsSandbox() {
		return h.Sandbox.DefaultMode()
	}
	return ""
}

// approvalForHarness returns the launch approval policy the daemon re-applies
// when it relaunches an existing conv (resume / clone / reincarnate). Like
// sandboxForHarness, the policy is not persisted per-conv, so a Codex agent is
// re-defaulted to its non-escalating mode (never) on relaunch rather than
// coming back with an escalating policy that would deadlock the detached,
// unattended pane; a harness with no launch approval flag (Claude Code) or an
// unknown tag yields "" (omit the flag). Same fail-safe-direction rationale as
// sandboxForHarness: a relaunch always returns to the non-deadlocking default.
// See JOH-200.
func approvalForHarness(name string) string {
	if h, err := harness.Resolve(strings.TrimSpace(name)); err == nil && h.SupportsApproval() {
		return h.Approval.DefaultPolicy()
	}
	return ""
}

// deliverRename renames a conversation the way its harness dictates and
// reports whether delivery succeeded. A harness with an in-pane rename
// command (Claude Code's /rename) gets it injected into the live pane; one
// without (e.g. Codex, which has no TUI rename command) is renamed
// out-of-band through its ConvStore.SetTitle.
//
// On the in-pane injection path the title becomes literal send-keys
// input, so deliverRename charset-gates it here as a last line of defense
// (isValidRenameSink) — a length-exempt, charset-only check that rejects
// any rune tmux would treat as a premature Enter. This makes the
// injection path safe for ALL callers regardless of whether each one
// pre-validates: the user-facing endpoints (handlers/lifecycle/clone)
// already pass titles through the stricter isValidRenameTitle (a charset
// superset of this gate, plus a 64-char cap) so they are unaffected,
// while the reincarnate carry titles — which exceed that cap once the
// `-x` / `-r-N` suffix is appended and were previously injected with no
// gate at all (JOH-177) — are now sanitized without being over-rejected.
//
// The out-of-band SetTitle path is a direct title-store write, not a
// send-keys stream, so it is not a keystroke sink and is not gated here.
func deliverRename(convID, title string) bool {
	h := harnessForConv(convID)

	// Slash-injection rename (Claude Code): type `<rename-cmd> <title>`
	// into the live pane. RenameCommand is a compile-time constant, never
	// caller input, so it adds no injection surface — but the title is
	// caller-derived, so it must clear the send-keys charset gate first.
	if h.SupportsRename() {
		if !isValidRenameSink(title) {
			slog.Warn("rename: title rejected by send-keys charset gate; skipping injection",
				"conv", convID, "harness", h.Name)
			return false
		}
		return injectSlashCommand(convID, h.Life.RenameCommand()+" "+title, "")
	}

	// Out-of-band rename (direct title store): no live pane needed.
	if h.SupportsConvs() {
		if err := h.Convs.SetTitle(convID, title); err != nil {
			slog.Warn("rename: ConvStore.SetTitle failed",
				"conv", convID, "harness", h.Name, "error", err)
			return false
		}
		return true
	}

	slog.Warn("rename: harness supports neither an in-pane rename nor a title store",
		"conv", convID, "harness", h.Name)
	return false
}
