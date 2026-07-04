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
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contain %q (%s)", needle, why)
		}
	}

	// refresh.js: the shared "Discard input?" confirm is a single helper so
	// every dirty-form dismiss path shows identical copy. bindBackdropDiscard
	// and the inline editMemberModal both route through it.
	must("export function confirmDiscard() {", "the shared discard-confirm helper exists")
	must("if (dirty && !(await confirmDiscard())) return;", "a dirty form confirms before an accidental close")

	// refresh.js: editMemberModal is the one editable form dismissed inline
	// (title / description text, role, owner) — it now dirty-tracks and
	// confirms on Escape / backdrop instead of silently discarding edits.
	must("overlay.addEventListener('input', markDirty);", "edit-member marks dirty on typed input")
	must("overlay.addEventListener('change', markDirty);", "edit-member marks dirty on toggle/select changes")
	// Its capture-phase Escape handler bails while the stacked discard
	// confirm is up so confirmModal's own handler cancels only the confirm,
	// and never re-enters to pop a second one underneath.
	must("if ($('#confirm-modal').classList.contains('show')) return;", "edit-member yields Escape to a stacked discard confirm")

	// A representative slice of the form dialogs wired through
	// bindBackdropDiscard — a sanity net that the coverage table is real.
	// (One per bind*UI file so a whole file dropping its call is caught.)
	must("bindBackdropDiscard('perm-edit-modal', closePermEditModal);", "the permission editor confirms before discarding")
	must("bindBackdropDiscard('template-editor-modal', closeTemplateEditor);", "the template editor confirms before discarding")
	must("bindBackdropDiscard('role-editor-modal', closeRoleEditor);", "the role editor confirms before discarding")
	must("bindBackdropDiscard('profile-editor-modal', closeProfileEditor);", "the profile editor confirms before discarding")
	must("bindBackdropDiscard('agent-spawn-modal', closeAgentSpawnModal);", "the spawn dialog confirms before discarding")
	must("bindBackdropDiscard('cron-create-modal', closeCronCreateModal);", "the cron-create dialog confirms before discarding")
	must("bindBackdropDiscard('human-reply-modal', closeHumanReplyModal);", "the human-reply dialog confirms before discarding")
	must("bindBackdropDiscard('group-create-modal', closeGroupCreateModal);", "the group-create dialog confirms before discarding")

	// The non-form LISTING overlays get the friction-free clean close (no
	// "discard?" for a typed filter). A child .modal-overlay on top claims
	// Escape first via bindManageOverlayDismiss's own guard.
	must("bindManageOverlayDismiss('templates-manage-modal', closeTemplatesManageModal);", "the templates browser closes cleanly")
	must("bindManageOverlayDismiss('roles-manage-modal', closeRolesManageModal);", "the roles browser closes cleanly")
	must("bindManageOverlayDismiss('profiles-manage-modal', closeProfilesManageModal);", "the profiles browser closes cleanly")
	must("bindManageOverlayDismiss('links-manage-modal', closeLinksManageModal);", "the links browser closes cleanly")

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
