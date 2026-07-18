package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// narrowDefaultApprovalToCaller adapts the HARNESS DEFAULT approval posture to
// what the spawning caller can actually mint. It must be called ONLY when the
// child's posture is genuinely unset — no explicit flag and no spawn-profile
// value — because an explicitly requested posture the caller cannot grant has
// to fail loudly through spawnApprovalLineageFailure, not be silently narrowed.
//
// The rule it encodes: a DEFAULT must never be something the caller is
// forbidden to grant. Claude's default is `auto`, but an `inherit` parent is
// credited only approvalAutoBaseline (its real posture is unknowable), so
// defaulting its children to `auto` would turn every bare delegation into a
// 403 — including from the operator's own `tclaude session new` session and
// from every agent spawned before `auto` became the default, since
// approvalForRelaunch faithfully preserves their recorded `inherit`. Falling
// back to the caller's own posture reproduces the pre-`auto` behaviour for
// exactly those callers and nothing else.
//
// This only ever NARROWS: the fallback branch is reached only when the default
// was refused (i.e. it exceeds the caller), and the caller's own posture is by
// construction one it may continue. A caller whose posture is itself broader
// than the default never reaches that branch. Cross-harness fallback is not
// attempted — postures are not interchangeable across harnesses — so such a
// caller keeps the default and gets the guard's loud error.
func narrowDefaultApprovalToCaller(parentConvID, childHarness, defaultPolicy string) string {
	if parentConvID == "" || strings.TrimSpace(defaultPolicy) == "" {
		return defaultPolicy // human caller (trust root), or nothing to narrow
	}
	if spawnApprovalLineageFailure(parentConvID, childHarness, defaultPolicy, false) == nil {
		return defaultPolicy // the caller can grant the default; nothing to do
	}
	parent, err := db.FindSessionByConvID(parentConvID)
	if err != nil || parent == nil {
		return defaultPolicy // let the guard report the real problem
	}
	if harnessOrDefault(parent.Harness) != harnessOrDefault(childHarness) {
		return defaultPolicy
	}
	parentPolicy := strings.TrimSpace(parent.ApprovalPolicy)
	if parentPolicy == "" || parent.ApprovalAutoReview {
		// An unreconstructable posture, or one carrying the Codex reviewer bit
		// (not a Claude-side default we can mint) — leave it to the guard.
		return defaultPolicy
	}
	if spawnApprovalLineageFailure(parentConvID, childHarness, parentPolicy, false) != nil {
		return defaultPolicy
	}
	return parentPolicy
}

// callerNarrowedApprovalNote describes the exceptional case where the harness
// default could not be delegated by an agent caller and was reduced to that
// caller's own same-harness posture. Callers append it only when the resolved
// value actually differs from defaultPolicy, keeping ordinary human/default
// spawns quiet.
func callerNarrowedApprovalNote(policy, defaultPolicy string) string {
	return fmt.Sprintf("approval %s (harness default %s, narrowed to caller posture)", policy, defaultPolicy)
}

// spawnApprovalLineageFailure prevents an agent from minting a child with
// broader automatic command-acceptance capability than its own launch
// posture. Humans have no caller conv-id and remain the trust root.
func spawnApprovalLineageFailure(parentConvID, childHarness, childPolicy string, childAutoReview bool) *spawnFailure {
	if parentConvID == "" {
		return nil
	}
	parent, err := db.FindSessionByConvID(parentConvID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "spawn approval guard: " + err.Error()}
	}
	if parent == nil {
		return &spawnFailure{http.StatusForbidden, "approval_restricted",
			fmt.Sprintf("agent %s has no recorded launch approval posture; relaunch it before spawning children", short8(parentConvID))}
	}
	parentHarness := harnessOrDefault(parent.Harness)
	parentPolicy := strings.TrimSpace(parent.ApprovalPolicy)
	parentAutoReview := parent.ApprovalAutoReview
	if parentHarness == harness.CodexName && parentPolicy == "" {
		legacyPolicy, proven, inferErr := db.LegacyCodexApprovalForConv(parentConvID)
		if inferErr != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", "spawn approval guard: " + inferErr.Error()}
		}
		if proven {
			parentPolicy = legacyPolicy
			// Every reconstructable legacy never path either launched without a
			// reviewer or had an idle reviewer (never emits no approval request).
			parentAutoReview = false
		}
	}
	childHarness = harnessOrDefault(childHarness)
	childPolicy = strings.TrimSpace(childPolicy)
	if parentPolicy == "" {
		return &spawnFailure{http.StatusForbidden, "approval_restricted",
			fmt.Sprintf("agent %s has a legacy %s launch whose approval posture cannot be reconstructed; relaunch it with current tclaude to record the conservative untrusted posture before spawning a matching child",
				short8(parentConvID), parentHarness)}
	}
	if !harness.ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview,
		childHarness, childPolicy, childAutoReview) {
		msg := fmt.Sprintf("agent %s was launched with %s approval %q (auto-review=%t) and may not spawn a %s child with approval %q (auto-review=%t)",
			short8(parentConvID), parentHarness, parentPolicy, parentAutoReview,
			childHarness, childPolicy, childAutoReview)
		// Name the way out when there is one — a bare denial reads as a dead end.
		// The classic case is an unresolvable `inherit` child, where "pass
		// --ask-for-approval auto" is the whole fix. Note that a DEFAULTED child
		// posture no longer lands here at all: narrowDefaultApprovalToCaller
		// adapts the harness default to the caller first, so what reaches this
		// point is an explicitly requested (flag or profile) escalation.
		if hint := harness.ApprovalLineageDenialHint(parentHarness, parentPolicy, parentAutoReview,
			childHarness, childPolicy); hint != "" {
			msg += ": " + hint
		}
		return &spawnFailure{http.StatusForbidden, "approval_restricted", msg}
	}
	return nil
}
