package agentd

import (
	"strings"
	"testing"
)

// JOH-210 inc3 adds the spawn-profiles dashboard UI: a load-from-profile
// selector in the spawn dialog, a manage-profiles overlay + editor (mirroring
// the templates UI), a clickable group default-profile picker, and a
// dashboard default-profile chip that REPLACED the retired user-default-model
// (settings.json) chip.
//
// This guards the wiring across the embedded HTML / CSS / JS: the new elements
// and their handlers exist, and the retired model-chip affordance can't creep
// back. It mirrors TestDashboardHTML_AccessTabMerged's present/absent style and
// searches the same dashboardAssets concatenation (dashboard.html +
// dashboard.css + every js/ module).
func TestDashboardHTML_SpawnProfilesUI(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
	absent := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard still contains %q (%s)", needle, why)
		}
	}

	// The data layer and Preact management feature exist and are wired at boot.
	present(`function loadProfiles(`, "profiles.js data layer")
	present(`function bindProfilesUI(`, "profiles UI binder")
	present(`bindProfilesUI();`, "profiles UI binder is called at boot")
	present(`mountManagementFeature({`, "Preact management feature is mounted before compatibility binders")
	present(`hosts: { root: '#management-root' }`, "management descriptor owns one explicit root")

	// 1. Spawn dialog: load-from-profile selector + Clear + Save-as-profile.
	present(`id="agent-spawn-load-profile"`, "spawn dialog load-from-profile selector")
	present(`id="agent-spawn-clear"`, "spawn dialog Clear button")
	present(`id="agent-spawn-save-profile"`, "spawn dialog Save-as-profile button")
	present(`export function applySpawnProfile(`, "plain spawn-model profile applier")
	present(`export function spawnProfileSeed(`, "controlled spawn draft → profile seed")
	present(`if (draft.profile) body.profile = draft.profile`, "dashboard spawns preserve the selected profile identity for server-side disable checks")

	// The default-profile pickers offer a "new profile" entry that jumps to
	// the editor (so an empty profile list isn't a dead end).
	present(`const NEW_PROFILE`, "the Preact picker's new-profile sentinel")
	present(`openProfileEditor(null, { onSaved })`, "new-profile entry opens the editor + sets the default")

	// 2. Manage-profiles overlay + editor, reached from the Groups cog.
	present(`id="profiles-manage-open"`, "the Groups cog's manage-profiles button")
	present(`id="management-root"`, "the shared Preact management root")
	present("id=${`${domKind}-manage-modal`}", "the manage-profiles overlay is component-owned")
	present(`id=${profiles ? 'profile-create-open'`, "the + new profile button")
	present(`id="profile-export-open"`, "the export-profiles button")
	present(`id="profile-import-open"`, "the import-profiles button")
	present(`id=${profiles ? 'profiles-list'`, "the profiles card list mount")
	present(`id="profile-export-modal"`, "the profile export modal")
	present(`id="profile-export-list"`, "the profile export checklist")
	present(`id="profile-import-modal"`, "the profile import modal")
	present(`id="profile-import-preview"`, "the profile import preview")
	if count := strings.Count(dashboardAssets, `class="tool profile-transfer-preview-button"`); count != 2 {
		t.Errorf("dashboard has %d styled import Preview actions, want 2 (spawn and sandbox profiles)", count)
	}
	present(".profile-transfer-preview-button {\n  align-self: flex-start;", "the import Preview action stays compact instead of stretching across the modal")
	present(`id="profile-editor-modal"`, "the profile editor modal")
	present(`id="profile-editor-name"`, "the editor's profile-name field")
	present(`id="profile-editor-disabled"`, "the editor can disable a profile without deleting it")
	present(`id="profile-editor-disabled-reason"`, "the editor captures the reason shown on failed spawns")
	present(`hidden=${local || !draft.disabled}`, "the disable reason is only shown while the profile is disabled")
	present(`#profile-editor-modal .cron-create-row > input[type=checkbox]`, "profile editor checkboxes use the aligned field-column styling")
	present(`profile-card-disabled`, "disabled profiles stay visible and visibly marked in the manager")
	present(`🚫 Disabled`, "disabled profile cards use an unmistakable prohibition marker")
	present(`const status = profile.disabled`, "profile selectors key their warning marker from the explicit disabled state")
	present(`id="profile-editor-operator-only"`, "the editor can restrict a profile to human/operator spawns")
	present(`👤 Operator only`, "operator-only profiles are visibly marked")

	// Duplicating a profile is a first-class listing action, and it routes
	// through the ordinary editor so the copy is reviewed and validated like any
	// hand-written profile rather than POSTed straight from the card.
	present(`class="tool profile-clone"`, "clone action in each spawn-profile card")
	present(`actions.openProfileClone(item)`, "clone action opens the profile editor")
	present(`function openProfileClone(`, "clone action is owned by the management actions")
	present(`{ editExisting: false, cloneSourceName: source.name }`, "clone editor keeps create semantics and remembers its source")
	present(`...(profile.aliases || []),`, "clone name suggestion dodges alias handles as well as primary names")
	present(`{ ...source, aliases: [], name: cloneName(source.name, taken) }`, "the copy drops the source's single-holder aliases and opens on a free handle")
	present(`!local && (editExisting || cloneSourceName) ? seed?.name || '' : ''`, "the clone's suggested name survives into the editor field")
	present("wizWord(`Clone profile: ${options.cloneSourceName}`", "the editor says it is cloning, and from what")

	// The listing panel is user-resizable and remembers its size, like the
	// group-templates panel it sits beside.
	present("resizeKey=${`tclaude.dash.modalSize.${domKind}-manage`}", "each management panel persists its own dragged size")
	present(`#profiles-manage-modal .manage-modal,`, "the spawn-profile panel carries the resize grip")
	present(`#profiles-manage-modal #profiles-list,`, "the profile listing absorbs the extra height of an enlarged panel")

	// Card-head alignment: the status pills are single-line labels, only the
	// summary gives, and the manager does not repeat in prose what it already
	// says as a badge.
	present(`.tc-disabled, .tc-operator-only {`, "both status pills share the non-wrapping pill geometry")
	present(`flex: none; white-space: nowrap;`, "a status pill never wraps mid-phrase and unbalances its row")
	// The summary is the head's shock absorber, but it must never be squeezed
	// below its content: a flex item narrower than its text does not clip, it
	// paints out over the action buttons. Both halves matter — the outsized
	// shrink factor (so the name and counts keep their line) and the floor.
	present(`.template-card .tc-descr { flex: 0 200 auto; min-width: 22ch;`, "the summary absorbs the row's deficit but keeps a floor so its text cannot paint over the buttons")
	// Rigid, because any shrink at all wraps a hyphenated name like
	// `gpt5.6-sol-high`; capped, so a pathological name wraps in its own column
	// instead of pushing the buttons off the card.
	present(`.template-card .tc-name { flex: none; max-width: 32ch;`, "the name never wraps at a realistic length and never monopolises the row")
	present(`.template-card .tc-actions { flex: none;`, "the action buttons are never squashed or pushed out of the row")
	present(`profileSummary(item, { status: false })`, "the manager card leaves the status to its badge")
	present(`function profileSummary(p, { status = true } = {})`, "surfaces without a status badge still get it in the summary")
	present(`id="profile-editor-harness"`, "the editor's harness selector")
	present(`id="profile-editor-submit"`, "the editor's Save button")
	present(`function ProfileExport(`, "profile export component")
	present(`function ProfileImport(`, "profile import component")
	present(`const [decisions, setDecisions]`, "profile import per-row decision state")
	present(`exportProfiles, inspectProfileImport, importProfiles`, "profile transfer data helpers are exported")
	absent(`function paintProfilesList(`, "legacy profile HTML-string rendering is retired")
	present(`.profile-import-conflict select,`, "profile import conflict select/input controls get dark modal styling")

	// 3. Group default-profile picker: the 🧠 badge is clickable.
	present(`'set-group-sandbox-profile' : 'set-group-profile'`, "the group default-profile picker action")
	present(`actions.setGroupProfile(group, kind, name)`, "the native group default-profile handler")

	// 4. Dashboard default-profile chip replaced the user-default-model chip.
	present(`id: 'dashboard-default-profile'`, "the dashboard default-profile chip")
	present(`data-act=${kind === 'sandbox' ? 'set-dash-sandbox-profile' : 'set-dash-profile'}`, "the dashboard default-profile picker action")
	present(`case 'set-dash-profile':`, "the dashboard default-profile handler")
	present(`function renderDashDefaultProfile(`, "the dashboard default-profile chip renderer")
	present(`/api/spawn-profile-default`, "global default uses the validated operational endpoint")
	present(`await setDashDefaultProfile(name)`, "picker waits for persistence before reporting success")
	present(`id="dashboard-default-profile-control"`, "the inline picker has a stable Preact host")
	present(`class="toolbar-profile-select"`, "the toolbar chip swaps to an inline select")
	present(`mountToolbarProfilePickerFeature`, "the picker is mounted as a keyed feature")
	absent(`select.replaceWith(chipEl)`, "Preact owns the inline chip/select replacement")
	present(`syncDashDefaultProfile(data.spawn_profile_default)`, "snapshot reconciles CLI changes without a separate poll request")
	absent(`function refreshDashDefaultProfile(`, "global default no longer has a separate poll fetch")
	present(`body.trust_dir = !!draft.trustDir`, "profile false trust intent stays explicit on spawn")
	present(`profile.trust_dir != null`, "sparse profiles preserve trust-dir fallthrough")
	present(`view.showTrustDir && draft.trustDirSpecified`, "untouched trust-dir stays omitted")
	// The trust-dir controls gate on the harness capability, not a hardcoded
	// harness name — Claude Code has a trust-folder dialog too, so the old
	// Codex-only gates would wrongly hide the checkbox from it.
	absent(`draft.harness === 'codex' && draft.trustDirSpecified`, "spawn body no longer gates trust-dir on the codex name")
	absent(`if (draft.harness === 'codex') seed.trust_dir`, "profile seed no longer gates trust-dir on the codex name")
	absent(`draft.harness !== 'codex'`, "the profile editor's Trust dir row no longer hides on the codex name")
	present(`hidden=${hEntry && !hEntry.can_dir_trust}`, "the Trust dir row gates on the dir-trust capability")

	// The retired user-level default-MODEL chip and its inline editor are gone.
	// (The backend /api/claude-settings/default-model endpoint and the
	// snapshot's user_default_model field are deliberately untouched — only
	// the chip UI was retired — so those are NOT asserted here.)
	absent(`id="user-default-model"`, "the user default-model chip was retired")
	absent(`data-act="set-user-default-model"`, "the user default-model edit action was retired")
	absent(`renderUserDefaultModel`, "the user default-model renderer was retired")
	absent(`id="model-alias-list"`, "the model-alias datalist (only the retired model chips used it) was removed")
}
