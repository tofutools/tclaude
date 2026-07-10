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

	// The data layer module exists and is wired into boot.
	present(`function loadProfiles(`, "profiles.js data layer")
	present(`function bindProfilesUI(`, "profiles UI binder")
	present(`bindProfilesUI();`, "profiles UI binder is called at boot")

	// 1. Spawn dialog: load-from-profile selector + Clear + Save-as-profile.
	present(`id="agent-spawn-load-profile"`, "spawn dialog load-from-profile selector")
	present(`id="agent-spawn-clear"`, "spawn dialog Clear button")
	present(`id="agent-spawn-save-profile"`, "spawn dialog Save-as-profile button")
	present(`function applyProfileToSpawnForm(`, "spawn-form profile applier")
	present(`function spawnFormAsProfileSeed(`, "spawn-form → profile seed for Save-as")

	// The default-profile pickers offer a "new profile" entry that jumps to
	// the editor (so an empty profile list isn't a dead end).
	present(`const PROFILE_PICKER_NEW`, "the picker's new-profile sentinel")
	present(`openProfileEditor(null, { onSaved:`, "new-profile entry opens the editor + sets the default")

	// 2. Manage-profiles overlay + editor, reached from the Groups cog.
	present(`id="profiles-manage-open"`, "the Groups cog's manage-profiles button")
	present(`id="profiles-manage-modal"`, "the manage-profiles overlay")
	present(`id="profile-create-open"`, "the + new profile button")
	present(`id="profile-export-open"`, "the export-profiles button")
	present(`id="profile-import-open"`, "the import-profiles button")
	present(`id="profiles-list"`, "the profiles card list mount")
	present(`id="profile-export-modal"`, "the profile export modal")
	present(`id="profile-export-list"`, "the profile export checklist")
	present(`id="profile-import-modal"`, "the profile import modal")
	present(`id="profile-import-preview"`, "the profile import preview")
	present(`id="profile-editor-modal"`, "the profile editor modal")
	present(`id="profile-editor-name"`, "the editor's profile-name field")
	present(`id="profile-editor-harness"`, "the editor's harness selector")
	present(`id="profile-editor-submit"`, "the editor's Save button")
	present(`function openProfileExportModal(`, "profile export dialog wiring")
	present(`function inspectProfileImportSource(`, "profile import inspect wiring")
	present(`function profileImportDecisions(`, "profile import per-row decisions")
	present(`exportProfiles, inspectProfileImport, importProfiles`, "profile transfer data helpers are exported")
	present(`.profile-import-conflict select,`, "profile import conflict select/input controls get dark modal styling")

	// 3. Group default-profile picker: the 🧠 badge is clickable.
	present(`data-act="set-group-profile"`, "the group default-profile picker action")
	present(`case 'set-group-profile':`, "the group default-profile handler")

	// 4. Dashboard default-profile chip replaced the user-default-model chip.
	present(`id="dashboard-default-profile"`, "the dashboard default-profile chip")
	present(`data-act="set-dash-profile"`, "the dashboard default-profile picker action")
	present(`case 'set-dash-profile':`, "the dashboard default-profile handler")
	present(`function renderDashDefaultProfile(`, "the dashboard default-profile chip renderer")
	present(`/api/spawn-profile-default`, "global default uses the validated operational endpoint")
	present(`await setDashDefaultProfile(name)`, "picker waits for persistence before reporting success")
	present(`refreshDashDefaultProfile()`, "open dashboards reconcile CLI changes during refresh")
	present(`body.trust_dir = $('#agent-spawn-trust-dir').checked`, "profile false trust intent stays explicit on spawn")

	// The retired user-level default-MODEL chip and its inline editor are gone.
	// (The backend /api/claude-settings/default-model endpoint and the
	// snapshot's user_default_model field are deliberately untouched — only
	// the chip UI was retired — so those are NOT asserted here.)
	absent(`id="user-default-model"`, "the user default-model chip was retired")
	absent(`data-act="set-user-default-model"`, "the user default-model edit action was retired")
	absent(`renderUserDefaultModel`, "the user default-model renderer was retired")
	absent(`id="model-alias-list"`, "the model-alias datalist (only the retired model chips used it) was removed")
}
