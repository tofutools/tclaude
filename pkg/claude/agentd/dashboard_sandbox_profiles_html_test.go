package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardHTML_SandboxProfilesUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="sandbox-profiles-manage-open"`:                                "Groups menu entry",
		`id="sandbox-profiles-manage-modal"`:                               "management overlay",
		`id="sandbox-profile-editor-modal"`:                                "profile editor",
		`id="sandbox-profile-editor-filesystem"`:                           "filesystem editor",
		`id="sandbox-profile-editor-environment"`:                          "environment editor",
		`id="sandbox-profile-editor-agent-directories"`:                    "agent-owned directory raw-JSON editor",
		`id="sandbox-profile-editor-agent-rows"`:                           "agent-owned directory structured rows",
		`id="sandbox-profile-editor-agent-add"`:                            "agent-owned directory add-row action",
		`agent_directories: editorAgentDirectories()`:                      "agent-owned directories persist in editor payloads",
		`id="sandbox-profile-editor-missing"`:                              "missing-directory warning",
		`id="sandbox-profile-editor-mkdir"`:                                "explicit missing-directory creation action",
		`id="sandbox-profile-diff-modal"`:                                  "save diff confirmation",
		`id="sandbox-profile-diff-body"`:                                   "server-normalized JSON diff",
		`id="sandbox-profile-scribe-open"`:                                 "new-profile agent configuration",
		`id="sandbox-profile-editor-scribe"`:                               "current-draft agent configuration",
		`id="sandbox-profile-export-open"`:                                 "export trigger",
		`id="sandbox-profile-import-open"`:                                 "import trigger",
		`id="sandbox-profile-export-modal"`:                                "export modal",
		`id="sandbox-profile-import-modal"`:                                "import modal",
		`id="sandbox-profile-import-conflict"`:                             "import conflict-policy selector",
		`function submitExport(`:                                           "export submit handler",
		`function submitImport(`:                                           "import submit handler",
		`${API}/export?`:                                                   "export hits the shared daemon endpoint",
		`${API}/import/inspect`:                                            "import preview uses server portability validation",
		"`${API}/import`":                                                  "import hits the shared daemon endpoint",
		`profile-transfer-warning`:                                         "missing local paths render as preview warnings",
		`apply_assignments: false`:                                         "import never re-applies exported assignments",
		`id="dashboard-default-sandbox-profile"`:                           "global quick assignment chip",
		`<button type="button" id="dashboard-default-sandbox-profile"`:     "global chip keeps native keyboard semantics",
		`data-act="set-dash-sandbox-profile"`:                              "global quick assignment picker wiring",
		`data-act="set-group-sandbox-profile"`:                             "group quick assignment chip",
		`case 'set-dash-sandbox-profile':`:                                 "global quick assignment mutation handler",
		`fetch('/api/sandbox-profile-default'`:                             "global assignment persistence",
		`btn.dataset.sandboxProfilePending = 'true'`:                       "global assignment locks against concurrent writes",
		`btn.disabled = true`:                                              "global chip is disabled during persistence",
		`btn.disabled = false`:                                             "global chip is re-enabled after persistence",
		`if (restoreFocus) chipEl.focus();`:                                "Escape returns keyboard focus to the chip",
		`＋ new sandbox profile…`:                                           "quick selector create shortcut",
		`openSandboxProfileEditor(null, { onCreate:`:                       "shortcut opens existing editor with assignment handoff",
		`await onCreate(savedBody.name)`:                                   "successful create assigns the previewed canonical name",
		`if (editorSaving) return`:                                         "save and cancel lifecycle is locked while request is pending",
		`const onCreate = editorOnCreate;`:                                 "launching assignment target is captured before request",
		`setEditorSaving(true)`:                                            "editor controls lock before create request",
		`setEditorSaving(false)`:                                           "editor controls unlock on every settled request",
		"`${target}?dry_run=1`":                                            "save validates and previews before writing",
		`confirmSandboxProfileDiff(preview.before || null, preview.after)`: "human confirms daemon-normalized diff",
		`body: JSON.stringify(savedBody)`:                                  "real write matches the previewed payload",
		`revision=${encodeURIComponent(preview.revision || '')}`:           "edit commit is coupled to the previewed revision",
		`editorOverlay.inert = true`:                                       "underlying editor is inert while the diff is modal",
		`editorOverlay.removeAttribute('aria-hidden')`:                     "underlying editor accessibility is restored after preview",
		`bindBackdropDiscard('sandbox-profile-editor-modal', closeEditor, () => !editorSaving)`:        "saving editor blocks stale discard confirmation",
		`bindManageOverlayDismiss } from './refresh.js'`:                                               "retired select mutation plumbing is no longer imported",
		"`<span class=\"group-sandbox-profile${g.sandbox_profile ? '' : ' unset'}\"":                   "group chip renders icon-only via qo-text/unset when inheriting",
		`.group-default-model, .user-default-model, .group-sandbox-profile, .global-sandbox-profile {`: "both sandbox chips share the spawn-profile chip skin",
		`noneLabel: '(none)'`:           "global picker clears the default",
		`noneLabel: '(inherit)'`:        "group picker clears back to inherit",
		`loadList: loadSandboxProfiles`: "group picker lists the sandbox-profile registry",
		`openNewEditor: (onSaved) => openSandboxProfileEditor(null, { onCreate: onSaved })`: "group picker create shortcut opens the sandbox editor with assignment handoff",
		`{ sel: '#dashboard-default-sandbox-profile', dock:`:                                "stable global chip follows spawn profile into dock",
		`renderDashSandboxProfile();`:                                                       "global quick chip snapshot refresh",
		`id="agent-spawn-sandbox-profile"`:                                                  "explicit spawn selector",
		`id="agent-spawn-sandbox-profile-preview"`:                                          "redacted effective preview",
		`body.sandbox_profile = sandboxProfile`:                                             "spawn request plumbing",
		`function bindSandboxProfilesUI()`:                                                  "UI binder",
		`/api/sandbox-profile-directories/inspect`:                                          "editor detects missing directories without mutation",
		`/api/sandbox-profile-directories/create`:                                           "editor explicitly creates missing directories",
		`Saving is allowed; read/write rules activate on a later launch`:                    "missing paths do not block saving",
		`bindSandboxProfilesUI();`:                                                          "boot wiring",
		`function refreshSpawnSandboxProfileUI(`:                                            "spawn preview refresh",
		`const generation = ++spawnPreviewGeneration`:                                       "out-of-order preview guard",
		`if (generation !== spawnPreviewGeneration) return`:                                 "stale preview responses are discarded",
		`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`:                      "group provenance lookup",
		`sbx-cap-env">env</span>`:                                                           "env keys render as name-only tags (redacted, no values)",
		`<span class="sbx-cap-val" title="${esc(e.name)}">${esc(e.name)}</span>`:            "environment card display shows the key name only, never its value",
		`not a secrets facility`:                                                            "non-secret warning",
		`const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft']`:                           "draft-only scribe grant",
		`Never create, edit, delete, assign, or apply a sandbox profile`:                    "scribe safety brief",
		`Agent draft loaded. Review every field`:                                            "explicit human preview",
		"fetch(`/api/sandbox-profile-drafts/":                                               "draft handoff polling",
		`createSandboxDraftQueue`:                                                           "parallel scribe drafts use the review queue",
		`sandboxDraftQueue.enqueue({ draft, targetName, onCreate })`:                        "each completed scribe draft is retained",
		`sandbox scribe draft ready — queued for review`:                                    "queued parallel drafts are visible to the human",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	// The retired select-based global control was folded into a chip (#982); its
	// plumbing must stay gone.
	for needle, why := range map[string]string{
		`exclusive: true`:                                "sandbox scribe should inherit ordinary agent defaults",
		`function setQuickAssignment(`:                   "retired select assignment handler",
		`const QUICK_NEW = '/new-sandbox-profile'`:       "retired select create sentinel",
		`data-sandbox-profile-quick-pending="true"`:      "retired select refresh guard",
		`id="dashboard-default-sandbox-profile-control"`: "retired select wrapper",
		`scribePollGeneration`:                           "a later scribe must not cancel an earlier draft poll",
	} {
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard still contains %q (%s)", needle, why)
		}
	}

	// The in-dialog global/group default selectors moved out to the Groups tab
	// (the 🛡 header + group chips), so the profiles dialog must no longer carry
	// them. Guard the removal so they don't quietly creep back.
	for _, gone := range []string{
		`id="sandbox-profile-global"`,
		`id="sandbox-profile-group"`,
		`id="sandbox-profile-group-value"`,
		`class="sandbox-profile-assignments"`,
	} {
		if strings.Contains(dashboardAssets, gone) {
			t.Errorf("dashboard still carries removed in-dialog assignment control %q", gone)
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
	request := strings.Index(js, "const preview = await api(")
	if capture < 0 || request < 0 || capture > request {
		t.Fatalf("quick-create assignment target must be captured before the async create request (capture=%d request=%d)", capture, request)
	}
	for needle, why := range map[string]string{
		"if (editorSaving) return;":                             "duplicate save and dismiss are guarded",
		"$('#sandbox-profile-editor-submit').disabled = saving": "submit locks during the request",
		"$('#sandbox-profile-editor-cancel').disabled = saving": "cancel locks during the request",
		"await onCreate(savedBody.name);":                       "successful create hands off the previewed canonical name",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
	if strings.Contains(js, "await loadSandboxProfiles();\n    await onCreate(savedBody.name);") {
		t.Error("quick-create must not insert a fallible list reload between successful create and assignment")
	}
}

func TestDashboardNamedNewSandboxScribeDraftRemainsCreate(t *testing.T) {
	raw, err := fs.ReadFile(dashboardAssetsFS, "js/sandbox-profiles.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for needle, why := range map[string]string{
		"targetName = null": "an omitted target remains distinguishable from the empty target of a new draft",
		"targetName === null ? (p ? p.name : '') : targetName": "a name entered into a new draft must not turn the create into an edit",
		"openEditor(draft.profile, { targetName, onCreate })":  "the named scribe draft preserves its original create target",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
	if strings.Contains(js, "targetName || (p ? p.name : '')") {
		t.Error("scribe create drafts must not infer an update target from their proposed name")
	}
}
