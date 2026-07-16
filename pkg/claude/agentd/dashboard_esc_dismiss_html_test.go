package agentd

import "testing"

// JOH-358: every layered dialog binds <ESC> to exit — with a discard
// confirmation when an editable form has unsaved edits. ManagementOverlay is
// the shared Preact lifecycle; component suites cover its behavior and this
// embedded-asset check catches a dropped ownership connection at go test time.
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

	// member-editor-island.js owns a controlled dirty draft. The shared Preact
	// overlay confirms accidental close, preserves the legacy backdrop-drag
	// gesture guard, and yields Escape by painted overlay order to both the
	// stacked permission editor and the discard confirm.
	must("dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}", "the member editor confirms dirty accidental closes")
	must("guardBackdropDrag=${true}", "the member editor preserves selection-drag backdrop safety")
	must("shouldHandle: () => isTopmostOverlay(overlayRef.current)", "stacked Preact overlays give Escape only to the painted topmost dialog")
	mustNot("function editMemberModal(", "the legacy inline edit-member lifecycle is removed")

	// A representative slice of forms wired through ManagementOverlay.
	must("export function useGuardedOverlayClose() {", "form owners share one explicit-control close adapter")
	must("const cleanup = registerClose?.(close);", "ManagementOverlay publishes its guarded close transaction")
	must("return { requestClose, registerClose };", "the shared adapter exposes request and registration wiring")
	must("registerClose=${registerClose}", "dirty form owners register explicit controls with ManagementOverlay")
	must(`id="perm-edit-modal" dialogClass="perm-edit-modal"`, "the permission editor uses the shared Preact overlay")
	must("onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}", "the Preact permission editor confirms before discarding")
	must("dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard}", "the Preact template editor confirms before discarding")
	must("const discard = await state.confirmDiscard();", "the shared Preact overlay awaits discard confirmation")
	must("if (discard) state.onClose();", "the shared Preact overlay closes only after confirmed discard")
	must(`id="role-editor-modal"`, "the role editor uses the shared Preact dismissal boundary")
	must(`id="profile-editor-modal"`, "the profile editor uses the shared Preact dismissal boundary")
	must(`id="agent-spawn-modal"`, "the Preact spawn dialog retains its scoped overlay id")
	must("spawnDraftIsDirty(draft, baseline, attachments.length)", "the spawn dialog derives dirty state from its controlled draft")
	must(`id="cron-create-modal"`, "the cron editor uses the shared Preact overlay")
	must("overlayClass=${editing ? 'cron-editing' : descriptor.kind === 'duplicate' ? 'cron-duplicating' : ''}",
		"component mode controls only the overlay presentation class")
	must("onSubmitHotkey=${busy ? null : () => submit(false)} dirty=${dirty} blocked=${busy}",
		"the cron editor confirms dirty dismissal and blocks close while saving")
	must(`id="human-reply-modal" labelledby="human-reply-title"`, "the human-reply dialog uses the shared Preact overlay")
	must("onClose=${state.close} dirty=${!!body} blocked=${busy} confirmDiscard=${confirmDiscard}", "the Preact human-reply dialog confirms before discarding")
	must(`id="operator-message-modal"`, "the terminal operator composer is rendered by the Preact dialog owner")
	must("dirty=${dirty}", "the operator composer derives dismissal state from its controlled draft")
	must(`id="group-create-modal"`, "the Preact group-create dialog retains its scoped overlay id")
	must("dirty=${dirty}", "the Preact group-create dialog publishes its controlled dirty state")
	must("blocked=${busy}", "the Preact group-create dialog blocks dismissal while submitting")

	// The non-form LISTING overlays get the friction-free clean close (no
	// "discard?" for a typed filter). A child .modal-overlay on top claims
	// Escape through the same shared painted-stack guard.
	must(`id: 'templates-manage-modal'`, "the Preact templates browser uses the clean manage-overlay boundary")
	must(`manage: true`, "the templates browser uses the listing-overlay variant")
	must("manage-overlay show", "Preact management browsers use the clean shared overlay close")
	must(`id="links-manage-modal"`, "the Preact Links browser uses the clean shared overlay boundary")
	must(`id="link-modal"`, "the Preact Links editor uses the stacked form boundary")

	// Both form and listing variants share the same z-index/DOM-order check.
	must("shouldHandle: () => isTopmostOverlay(overlayRef.current)", "all shared overlays yield Escape to the painted topmost dialog")
	mustNot("if (document.querySelector('.modal-overlay.show')) return;", "the naive any-modal-shown Escape guard is gone from the manage overlays")

	// modal-term.js: the live-terminal modal DELIBERATELY does not bind
	// Escape (ESC is a control char the terminal itself needs). Pin the
	// rationale comment so the exclusion can't be "fixed" into a regression.
	must("// Escape is NOT a close key here", "the terminal modal keeps its documented Escape exclusion")
}
