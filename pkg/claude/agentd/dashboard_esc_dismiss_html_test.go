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

	// modal-term.js: the live-terminal modal DELIBERATELY does not bind
	// Escape (ESC is a control char the terminal itself needs). Pin the
	// rationale comment so the exclusion can't be "fixed" into a regression.
	must("// Escape is NOT a close key here", "the terminal modal keeps its documented Escape exclusion")
}
