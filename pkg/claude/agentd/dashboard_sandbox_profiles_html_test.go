package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardHTML_SandboxProfilesUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="sandbox-profiles-manage-open"`:        "Groups menu entry",
		`id="sandbox-profiles-manage-modal"`:       "management overlay",
		`id="sandbox-profile-editor-modal"`:        "profile editor",
		`id="sandbox-profile-editor-filesystem"`:   "filesystem editor",
		`id="sandbox-profile-editor-environment"`:  "environment editor",
		`id="sandbox-profile-global"`:              "global assignment control",
		`id="sandbox-profile-group-value"`:         "group assignment control",
		`id="dashboard-default-sandbox-profile"`:   "global quick assignment selector",
		`data-sandbox-profile-quick-group=`:        "group quick assignment selector",
		`function setQuickAssignment(`:             "shared quick-assignment mutation handler",
		`const QUICK_NEW = '/new-sandbox-profile'`: "collision-proof create shortcut sentinel",
		`＋ new sandbox profile…`:                   "quick selector create shortcut",
		`openEditor(null, { onCreate:`:             "shortcut opens existing editor with assignment handoff",
		`await onCreate(body.name)`:                "successful create assigns back to launching scope",
		`if (editorSaving) return`:                 "save and cancel lifecycle is locked while request is pending",
		`const onCreate = editorOnCreate;`:         "launching assignment target is captured before request",
		`setEditorSaving(true)`:                    "editor controls lock before create request",
		`setEditorSaving(false)`:                   "editor controls unlock on every settled request",
		`bindBackdropDiscard('sandbox-profile-editor-modal', closeEditor, () => !editorSaving)`: "saving editor blocks stale discard confirmation",
		`bindManageOverlayDismiss, refresh } from './refresh.js'`:                               "quick assignment refresh is explicitly imported",
		`select.disabled = false`:                                                "quick selector is re-enabled after mutation",
		`data-sandbox-profile-quick-pending="true"`:                              "snapshot refresh pauses during quick assignment",
		`summary:focus-within .group-sandbox-profile-quick select`:               "folded group selector reveals for keyboard focus",
		`class="sandbox-profile-quick group-sandbox-profile-quick" tabindex="0"`: "folded icon is keyboard reveal target",
		`summary .group-sandbox-profile-quick select {
    display: none;`: "folded group selector is icon-only",
		`{ sel: '#dashboard-default-sandbox-profile-control', dock:`:   "global quick selector follows spawn profile into dock",
		`renderDashSandboxProfile();`:                                  "global quick selector snapshot refresh",
		`id="agent-spawn-sandbox-profile"`:                             "explicit spawn selector",
		`id="agent-spawn-sandbox-profile-preview"`:                     "redacted effective preview",
		`body.sandbox_profile = sandboxProfile`:                        "spawn request plumbing",
		`function bindSandboxProfilesUI()`:                             "UI binder",
		`bindSandboxProfilesUI();`:                                     "boot wiring",
		`function refreshSpawnSandboxProfileUI(`:                       "spawn preview refresh",
		`const generation = ++spawnPreviewGeneration`:                  "out-of-order preview guard",
		`if (generation !== spawnPreviewGeneration) return`:            "stale preview responses are discarded",
		`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`: "group provenance lookup",
		`environment keys:`:                                            "redacted environment display",
		`not a secrets facility`:                                       "non-secret warning",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
}

func TestDashboardSandboxQuickCreateLocksAndCapturesAssignmentTarget(t *testing.T) {
	raw, err := fs.ReadFile(dashboardAssetsFS, "js/sandbox-profiles.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	capture := strings.Index(js, "const onCreate = editorOnCreate;")
	request := strings.Index(js, "await api(editingName ?")
	if capture < 0 || request < 0 || capture > request {
		t.Fatalf("quick-create assignment target must be captured before the async create request (capture=%d request=%d)", capture, request)
	}
	for needle, why := range map[string]string{
		"if (editorSaving) return;":                             "duplicate save and dismiss are guarded",
		"$('#sandbox-profile-editor-submit').disabled = saving": "submit locks during the request",
		"$('#sandbox-profile-editor-cancel').disabled = saving": "cancel locks during the request",
		"await onCreate(body.name);":                            "successful create hands off directly to assignment",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
	if strings.Contains(js, "await loadSandboxProfiles();\n      await onCreate(body.name);") {
		t.Error("quick-create must not insert a fallible list reload between successful create and assignment")
	}
}
