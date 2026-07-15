package agentd

import (
	"strings"
	"testing"
)

// JOH-358: every layered dialog binds <ESC> to exit — with a discard
// confirmation when an editable form has unsaved edits. The dismiss wiring
// lives entirely in JS (bindBackdropDiscard / bindManageOverlayDismiss calls
// plus the per-open inline handlers) with no Go path exercising it in the
// browser, so a rename or a dropped bind call silently breaks Escape for a
// whole dialog. Asserting on the embedded concatenation catches a drop at
// `go test ./...`.
func TestDashboardHTML_EscDismissWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contain %q (%s)", needle, why)
		}
	}

	// shell-state.js owns the shared "Discard input?" confirmation API, while
	// refresh.js retains the compatibility export used by legacy and Preact
	// form-dismiss paths. Keeping the copy at the shell-state boundary gives
	// every caller the same confirmation without a second DOM owner.
	must("export function shellConfirmDiscard() {", "shell state exposes the shared discard-confirm API")
	must("shellConfirmDiscard as confirmDiscard,", "refresh aliases the shell-state API for existing callers")
	must("return shellConfirm({", "discard confirmation routes through shell-owned confirmation state")
	must("title: 'Discard input?',", "the shared discard-confirm title remains stable")
	must("okLabel: 'Discard',", "the shared discard action remains explicit")
	mustNot("export function confirmDiscard() {", "refresh no longer owns a second discard-confirm implementation")
	must("if (dirty && !(await confirmDiscard())) return;", "a dirty form confirms before an accidental close")

	// member-editor-island.js owns a controlled dirty draft. The shared Preact
	// overlay confirms accidental close, preserves the legacy backdrop-drag
	// gesture guard, and yields Escape by painted overlay order to both the
	// stacked permission editor and the discard confirm.
	must("dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}", "the member editor confirms dirty accidental closes")
	must("guardBackdropDrag=${true}", "the member editor preserves selection-drag backdrop safety")
	must("shouldHandle: () => isTopmostOverlay(overlayRef.current)", "stacked Preact overlays give Escape only to the painted topmost dialog")
	mustNot("function editMemberModal(", "the legacy inline edit-member lifecycle is removed")

	// A representative slice of the form dialogs wired through
	// bindBackdropDiscard — a sanity net that the coverage table is real.
	// (One per bind*UI file so a whole file dropping its call is caught.)
	must(`id="perm-edit-modal" dialogClass="perm-edit-modal"`, "the permission editor uses the shared Preact overlay")
	must("onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}", "the Preact permission editor confirms before discarding")
	must("dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard}", "the Preact template editor confirms before discarding")
	must("if (!dirty || (await confirmDiscard())) onClose();", "Preact management editors confirm before discarding")
	must(`id="role-editor-modal"`, "the role editor uses the shared Preact dismissal boundary")
	must(`id="profile-editor-modal"`, "the profile editor uses the shared Preact dismissal boundary")
	must("bindBackdropDiscard('agent-spawn-modal', closeAgentSpawnModal);", "the spawn dialog confirms before discarding")
	must(`id="cron-create-modal"`, "the cron editor uses the shared Preact overlay")
	must("overlayClass=${editing ? 'cron-editing' : descriptor.kind === 'duplicate' ? 'cron-duplicating' : ''}",
		"component mode controls only the overlay presentation class")
	must("onSubmitHotkey=${busy ? null : () => submit(false)} dirty=${dirty} blocked=${busy}",
		"the cron editor confirms dirty dismissal and blocks close while saving")
	must(`id="human-reply-modal" labelledby="human-reply-title"`, "the human-reply dialog uses the shared Preact overlay")
	must("onClose=${state.close} dirty=${!!body} blocked=${busy} confirmDiscard=${confirmDiscard}", "the Preact human-reply dialog confirms before discarding")
	must("bindBackdropDiscard('operator-message-modal', close, () => !pending);", "the operator composer confirms before accidental dismissal")
	must("dismissGuard.tryDismiss()", "the operator composer routes Cancel through the same discard confirmation")
	must(`id="group-create-modal"`, "the Preact group-create dialog retains its scoped overlay id")
	must("dirty=${dirty}", "the Preact group-create dialog publishes its controlled dirty state")
	must("blocked=${busy}", "the Preact group-create dialog blocks dismissal while submitting")
	must("if (!dirty || await confirmDiscard()) state.close();", "the group-create Cancel path confirms before discarding")

	// The non-form LISTING overlays get the friction-free clean close (no
	// "discard?" for a typed filter). A child .modal-overlay on top claims
	// Escape first via bindManageOverlayDismiss's own guard.
	must(`id: 'templates-manage-modal'`, "the Preact templates browser uses the clean manage-overlay boundary")
	must(`manage: true`, "the templates browser uses the listing-overlay variant")
	must("manage-overlay show", "Preact management browsers use the clean shared overlay close")
	must(`id="links-manage-modal"`, "the Preact Links browser uses the clean shared overlay boundary")
	must(`id="link-modal"`, "the Preact Links editor uses the stacked form boundary")

	// A manage overlay's Escape must key on the z-index/DOM-order topmost test,
	// NOT a bare "any .modal-overlay is shown" guard. The naive guard couldn't
	// distinguish a child form modal ABOVE (yield to it) from a plain modal
	// BENEATH — e.g. the templates panel opened over the "Form a party" dialog
	// via "⧉ manage circles…" (JOH-356) — so it swallowed the Escape and left
	// the front-most panel un-closable. Pin the fix so it can't regress: the
	// topmost guard must now appear in BOTH Escape paths — bindBackdropDiscard
	// (form modals) AND bindManageOverlayDismiss (listing overlays). A bare
	// Contains would be satisfied by bindBackdropDiscard's copy alone even if
	// the manage-overlay guard were dropped, so assert the count instead: a
	// dropped manage guard takes it back to 1 and fails here.
	if n := strings.Count(dashboardAssets, "if (!isTopmostOverlay(el)) return;"); n < 2 {
		t.Errorf("expected the topmost-overlay Escape guard in both bindBackdropDiscard and bindManageOverlayDismiss, found %d occurrence(s)", n)
	}
	mustNot("if (document.querySelector('.modal-overlay.show')) return;", "the naive any-modal-shown Escape guard is gone from the manage overlays")

	// modal-term.js: the live-terminal modal DELIBERATELY does not bind
	// Escape (ESC is a control char the terminal itself needs). Pin the
	// rationale comment so the exclusion can't be "fixed" into a regression.
	must("// Escape is NOT a close key here", "the terminal modal keeps its documented Escape exclusion")
}
