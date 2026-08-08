package agentd

import (
	"fmt"
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
	if profile, err := db.ConversationResumeProfileForConv(convID); err == nil && profile != nil {
		if h, err := harness.Resolve(profile.Harness); err == nil {
			return h
		}
		slog.Warn("harnessForConv: unknown durable conversation harness; checking live session",
			"conv", convID, "harness", profile.Harness)
	}
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

// relaunchSandboxForProfile returns the stable agent's already-selected
// sandbox mode. A nil field is unknown legacy authority and fails closed; a
// present empty string retains the historical legacy interpretation.
func relaunchSandboxForProfile(profile *db.AgentRelaunchProfile, harnessName string) (string, error) {
	if profile == nil {
		return "", fmt.Errorf("durable agent relaunch profile is missing")
	}
	if profile.HarnessBuiltinMode == nil {
		return "", fmt.Errorf("durable agent relaunch profile has unknown sandbox mode")
	}
	return relaunchSandboxForSession(&db.SessionRow{
		Harness: harnessName, HarnessBuiltinMode: *profile.HarnessBuiltinMode,
	})
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

// relaunchSandboxForSession returns the already-selected source generation's
// recorded sandbox mode. Callers must pass the same authoritative row they use
// for cwd, harness, and model inheritance; re-querying by conv-id can select a
// newer/different row and silently weaken the launch.
//
// sandboxForHarness alone returns the harness DEFAULT, which for Claude Code is
// the `inherit` sentinel. Using it for a relaunch silently downgraded an agent
// that had been launched under `sandbox on` — its OS sandbox simply stopped
// being enforced on the next resume. It also made an acknowledged break-glass
// grant impossible to relaunch, because the capability gate correctly refuses
// to re-open protected denies under a mode that cannot guarantee them.
//
// Preserving the recorded mode is the same principle the rest of relaunch
// follows: replay the decision that was made, do not re-derive a new one.
func relaunchSandboxForSession(row *db.SessionRow) (string, error) {
	if row == nil {
		return "", fmt.Errorf("relaunch source session is missing")
	}
	harnessName := strings.TrimSpace(row.Harness)
	recorded := strings.TrimSpace(row.HarnessBuiltinMode)
	if recorded == "" {
		// Legacy rows did not persist the field. Preserve the established Codex
		// managed-profile/default behavior only for that explicit legacy case.
		return sandboxForHarness(harnessName), nil
	}
	h, err := harness.Resolve(strings.TrimSpace(harnessName))
	if err != nil {
		return "", fmt.Errorf("resolve recorded harness %q: %w", harnessName, err)
	}
	if !h.SupportsSandbox() {
		return "", fmt.Errorf("harness %q has recorded sandbox mode %q but no sandbox contract", h.Name, recorded)
	}
	mode, err := h.Sandbox.ValidateMode(recorded)
	if err != nil {
		return "", fmt.Errorf("invalid recorded sandbox mode %q for %s: %w", recorded, h.Name, err)
	}
	return mode, nil
}

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

// sandboxImplementationForConv returns the sandbox implementation a conv's
// durable relaunch config records, or "" (the legacy harness-builtin default)
// when there is no readable config. It exists for the launch-adjacent callers
// that already derive a harness and a sandbox mode for a conv and now need the
// third axis to answer what wall the launch will actually build.
func sandboxImplementationForConv(convID string) string {
	config, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		return ""
	}
	return config.activeSandboxImplementation()
}

// reconstructApproval is the daemon's entry point into the one reconstruction
// rule (harness.ReconstructApprovalPolicy): a recorded posture is reproduced
// and re-validated, an absent one re-resolves under current config. Every
// daemon surface that rebuilds a launch — relaunch, clone, seance, the durable
// relaunch profile — goes through here, so none of them can pin a blank row to
// a historical value the CLI resume path would re-resolve. See TCL-990.
func reconstructApproval(harnessName, recorded string) (string, error) {
	h, err := harness.Resolve(strings.TrimSpace(harnessName))
	if err != nil {
		return "", err
	}
	policy, reresolved, err := harness.ReconstructApprovalPolicy(h, recorded)
	if err != nil {
		return "", err
	}
	if reresolved && policy != "" {
		slog.Info("relaunch: no recorded approval posture; resolved from current config",
			"harness", h.Name, "approval", policy)
	}
	return policy, nil
}

// approvalForHarness returns what an UNRECORDED approval input resolves to
// under current config for this harness — the reconstruction rule's
// absent-posture arm.
//
// approvalForRelaunch also uses it for its ERROR arms (unreadable row, pruned
// row, unknown harness, unvalidatable recorded value). Those are not the same
// thing as an absent input: an input may have been recorded and simply not
// readable. They land here anyway because the caller — clone's standalone
// export/conv branch, the only path that reaches those arms — needs a value,
// and the alternative is the historical pin this rule exists to remove. The
// managed relaunch path does not fail this way: durableRelaunchConfigForConv
// returns an error instead of a posture.
func approvalForHarness(name string) string {
	policy, err := reconstructApproval(name, "")
	if err != nil {
		return ""
	}
	return policy
}

// approvalForRelaunch reconstructs the source generation's approval INPUT. A
// recorded posture is reproduced exactly and re-validated; a blank row recorded
// no input, so it re-resolves under current config instead of being pinned to
// what it would historically have resolved to.
func approvalForRelaunch(sourceConv, harnessName string) (string, bool) {
	row, err := db.FindSessionByConvID(sourceConv)
	if err != nil {
		slog.Warn("relaunch: approval posture lookup failed; using harness default",
			"conv", sourceConv, "error", err)
		return approvalForHarness(harnessName), false
	}
	if row == nil {
		return approvalForHarness(harnessName), false
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	policy, err := reconstructApproval(harnessName, row.ApprovalPolicy)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	autoReview, err := harness.ResolveAutoReview(h, row.ApprovalAutoReview)
	if err != nil {
		return approvalForHarness(harnessName), false
	}
	return policy, autoReview
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
		// An API-connected Copilot agent is renamed by a typed call instead.
		// The gate above still ran, and deliberately: it belongs to the
		// send-keys path, which is still Copilot's default, and a title that
		// renders differently depending on which transport a conversation
		// happens to hold would be its own bug. See copilot_api_drive.go.
		if copilotAPIDriven(convID) {
			if err := renameCopilotAPISession(convID, title); err != nil {
				slog.Warn("rename: Copilot API rename failed",
					"conv", convID, "error", err)
				return false
			}
			cacheDeliveredRename(convID, title, h.Name)
			return true
		}
		if !injectSlashCommand(convID, h.Life.RenameCommand()+" "+title, "", "rename") {
			return false
		}
		cacheDeliveredRename(convID, title, h.Name)
		return true
	}

	// Out-of-band rename (direct title store): no live pane needed.
	if h.SupportsConvs() {
		if err := h.Convs.SetTitle(convID, title); err != nil {
			slog.Warn("rename: ConvStore.SetTitle failed",
				"conv", convID, "harness", h.Name, "error", err)
			return false
		}
		cacheDeliveredRename(convID, title, h.Name)
		return true
	}

	slog.Warn("rename: harness supports neither an in-pane rename nor a title store",
		"conv", convID, "harness", h.Name)
	return false
}

// cacheDeliveredRename records the accepted title before an in-pane harness
// has emitted and agentd has followed its transcript sidecar. Besides making
// cache-only UI reads immediate, this establishes the ordering needed by a
// `/clear` that arrives inside the fsnotify debounce window: identity rotation
// carries the just-delivered name without making the hook parse the transcript.
// A later authoritative transcript/native-store scan can still replace it.
func cacheDeliveredRename(convID, title, harnessName string) {
	if err := db.SetConvIndexCustomTitle(convID, title, harnessName); err != nil {
		slog.Warn("rename: failed to refresh cached title after delivery",
			"conv", convID, "harness", harnessName, "error", err)
	}
	if actor, err := db.GetAgentByConv(convID); err == nil && actor != nil {
		if err := db.SetAgentPendingName(actor.AgentID, title); err != nil {
			slog.Warn("rename: failed to refresh actor title fallback after delivery",
				"conv", convID, "agent_id", actor.AgentID, "error", err)
		}
	}
}
