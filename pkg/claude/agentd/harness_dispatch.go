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

// sandboxForHarness returns the launch containment token the daemon re-applies
// when it relaunches an existing conv (resume / clone / reincarnate). The
// mode/profile is not persisted per-conv, so a Codex agent must be
// re-defaulted to tclaude's managed permission profile on relaunch rather than
// coming back unsandboxed or with a plain workspace-write sandbox that cannot
// reach the agentd socket. A harness with no launch sandbox/profile flag
// (Claude Code) or an unknown tag yields "" (omit the flag). A $HOME cwd is
// still re-checked by the forked `tclaude session new`'s own guard, so this
// needn't repeat it.
//
// Deliberate: this always re-defaults to the secure mode and does NOT
// preserve a per-conv override — an agent originally spawned with an explicit
// danger-full-access comes back sandboxed. That is the fail-closed direction
// (a relaunch tightening, never loosening, the sandbox), so it is intentional,
// not a dropped-state bug. Per-conv mode persistence, if ever wanted, would be
// a separate feature.
func sandboxForHarness(name string) string {
	if strings.TrimSpace(name) == harness.CodexName {
		return harness.SandboxManagedProfile
	}
	if h, err := harness.Resolve(strings.TrimSpace(name)); err == nil && h.SupportsSandbox() {
		// Validate the harness default before threading it. Claude Code's default
		// is the first-class `inherit` sentinel — carried verbatim now (the
		// tri-state fix), it emits no `--settings` sandbox block at spawn (see
		// claudeSandboxBlock) so an un-overridden Claude agent stays on the
		// operator's own settings.json across clone/reincarnate. Codex's
		// managed-profile default validates to itself, unchanged.
		if mode, verr := h.Sandbox.ValidateMode(h.Sandbox.DefaultMode()); verr == nil {
			return mode
		}
	}
	return ""
}

// approvalForHarness returns the safe launch default used for legacy rows that
// have no recorded posture. Current relaunches use approvalForRelaunch to
// preserve the source generation exactly.
func approvalForHarness(name string) string {
	if h, err := harness.Resolve(strings.TrimSpace(name)); err == nil && h.SupportsApproval() {
		// Validate the harness default before threading it. Claude Code's default
		// is the first-class `inherit` sentinel — carried verbatim now (the
		// tri-state fix), it emits no `--permission-mode` at spawn (see
		// claudeApprovalValue) so an un-overridden Claude agent keeps its
		// settings.json posture across clone/reincarnate. Codex's `never` default
		// validates to itself.
		if pol, verr := h.Approval.ValidatePolicy(h.Approval.DefaultPolicy()); verr == nil {
			return pol
		}
	}
	return ""
}

// approvalForRelaunch preserves the source generation's recorded authority.
// Legacy rows fall back to the harness default; current rows carry both
// approval inputs so the relaunched process and the authorization record stay
// identical.
func approvalForRelaunch(sourceConv, harnessName string) (string, bool) {
	row, err := db.FindSessionByConvID(sourceConv)
	if err != nil {
		slog.Warn("relaunch: approval posture lookup failed; using harness default",
			"conv", sourceConv, "error", err)
		return approvalForHarness(harnessName), false
	}
	if row == nil || strings.TrimSpace(row.ApprovalPolicy) == "" {
		return approvalForHarness(harnessName), false
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	policy, err := harness.ValidateApprovalPolicy(h, row.ApprovalPolicy)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	autoReview, err := harness.ResolveAutoReview(h, row.ApprovalAutoReview)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	return policy, autoReview
}

// remoteControlForRelaunch resolves whether a relaunch (resume / reincarnate /
// clone) should re-arm Claude Code's built-in Remote Access on the new pane,
// carried from the SOURCE conversation's persisted best-known state (JOH-256).
// Like approval posture, remote-control is persisted per-conv, so this
// carries the source's actual flag rather than a harness default: an agent the
// operator armed for phone access must survive the handoff, the operator-decided
// carry-over semantics for all three paths (JOH-261).
//
// True only when the source was armed AND the harness can honour it. The
// persisted flag is itself the gate — only a Claude Code conv can ever record
// remote_control=1, since the toggle/spawn that set it is gated on
// CanRemoteControl — so a Codex source is false by construction; the explicit
// capability check is defence in depth so a stale flag could never thread
// `--remote-control` onto a harness that would reject it. A lookup error
// degrades to false (no re-arm), logged, never fatal.
func remoteControlForRelaunch(sourceConv, harnessName string) bool {
	on, err := db.RemoteControlForConv(sourceConv)
	if err != nil {
		slog.Warn("relaunch: remote-control lookup failed; not re-arming",
			"conv", sourceConv, "error", err)
		return false
	}
	if !on {
		return false
	}
	h, _ := harness.Resolve(strings.TrimSpace(harnessName))
	return h.CanRemoteControl()
}

// askTimeoutForRelaunch resolves the AskUserQuestion idle-timeout a relaunch
// (resume / clone / reincarnate) threads onto the new pane, carried from the
// SOURCE conversation's persisted value (schema v97). Like approval posture,
// the operator wants a per-agent timeout PRESERVED across the handoff: a
// reincarnated agentic worker set to auto-continue at 5m must come back on 5m,
// not revert to global settings.json. So this reads the source's actual recorded
// value rather than a harness default (the same preserve semantics as
// remoteControlForRelaunch).
//
// "" (omit the flag) when the source recorded no timeout — a pre-column row, an
// un-chosen timeout, or a non-Claude source; a lookup error degrades to "" and
// is logged, never fatal. A stale value can never mis-thread onto a harness that
// would reject it: the forked `tclaude session new` re-validates it per-harness
// (a Codex relaunch would 400 an ask-timeout), and a Codex source records "" by
// construction (its spawn path rejects the flag), so this yields "" for Codex.
func askTimeoutForRelaunch(sourceConv string) string {
	v, err := db.AskTimeoutForConv(sourceConv)
	if err != nil {
		slog.Warn("relaunch: ask-timeout lookup failed; not preserving",
			"conv", sourceConv, "error", err)
		return ""
	}
	return v
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
		return injectSlashCommand(convID, h.Life.RenameCommand()+" "+title, "", "rename")
	}

	// Out-of-band rename (direct title store): no live pane needed.
	if h.SupportsConvs() {
		if err := h.Convs.SetTitle(convID, title); err != nil {
			slog.Warn("rename: ConvStore.SetTitle failed",
				"conv", convID, "harness", h.Name, "error", err)
			return false
		}
		if err := db.SetConvIndexCustomTitle(convID, title, h.Name); err != nil {
			slog.Warn("rename: failed to refresh cached title after ConvStore.SetTitle",
				"conv", convID, "harness", h.Name, "error", err)
		}
		return true
	}

	slog.Warn("rename: harness supports neither an in-pane rename nor a title store",
		"conv", convID, "harness", h.Name)
	return false
}
