package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_SandboxProfilesUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="sandbox-profiles-manage-open"`:                                            "Groups menu entry",
		`id="management-root"`:                                                         "shared Preact management root",
		`id="sandbox-profile-editor-modal"`:                                            "Preact profile editor",
		`id="sandbox-profile-editor-filesystem"`:                                       "raw filesystem editor",
		`id="sandbox-profile-editor-environment"`:                                      "raw environment editor",
		`id="sandbox-profile-editor-includes"`:                                         "raw includes editor",
		`id="sandbox-profile-editor-agent-directories"`:                                "raw agent-owned directory editor",
		`id="sandbox-profile-editor-submit"`:                                           "stable submit hook for plain and wizard skins",
		`class="tool sandbox-profile-clone"`:                                           "clone action in each sandbox-profile card",
		`actions.openSandboxClone(item)`:                                               "clone action opens the guarded sandbox editor",
		`options: { editExisting: false, cloneSourceName: source.name }`:               "clone editor retains create semantics and source context",
		`options.editExisting === false`:                                               "clone save cannot patch its source profile",
		`const MAX_SANDBOX_PROFILE_NAME_BYTES = 200`:                                   "clone suggestions obey the server name limit",
		`{ editExisting, cloneSourceName: options.cloneSourceName || '' }`:             "scribe handoff carries explicit create/edit mode and clone context",
		`{ ...editorOptions, targetName, onCreate, notice:`:                            "returned scribe drafts preserve clone-create mode",
		`class="sbx-agent-name"`:                                                       "structured agent-owned directory row",
		`.sbx-section input:not([type])`:                                               "Preact structured inputs retain dark modal styling",
		`.sbx-section select`:                                                          "Preact structured selects retain dark modal styling",
		`.sbx-row button`:                                                              "Preact structured row buttons retain dark modal styling",
		`.sbx-row button:last-child:hover`:                                             "Preact remove buttons retain destructive hover styling",
		`#sandbox-profile-editor-submit:disabled::before`:                              "wizard saving state suppresses the decorative submit label",
		`/api/sandbox-profile-directories/inspect`:                                     "missing-directory inspection",
		`/api/sandbox-profile-directories/create`:                                      "explicit missing-directory creation",
		`agent_directories: draft.agent_directories`:                                   "agent-owned directories persist in save payloads",
		`if (draft.network !== undefined) body.network = draft.network`:                "network axis persists in save payloads",
		`if (draft.unix_sockets !== undefined) body.unix_sockets = draft.unix_sockets`: "Unix-socket axis persists in save payloads",
		`id="sandbox-profile-editor-network-mode"`:                                     "structured network posture selector",
		`id="sandbox-profile-editor-unix-sockets-mode"`:                                "structured Unix-socket posture selector",
		`id="sandbox-profile-editor-evaluate-for"`:                                     "authoritative target prediction picker",
		`id="sandbox-profile-editor-unix-sockets"`:                                     "raw Unix-socket JSON editor",
		`actions.predictSandbox(draft, targets`:                                        "editor prediction uses the server route",
		`warnings.composition.map(`:                                                    "empty intersections have their own warning surface",
		`id="sandbox-profile-scribe-open"`:                                             "new-profile agent configuration",
		`id="sandbox-profile-editor-scribe"`:                                           "current-draft agent configuration",
		`id="sandbox-profile-export-open"`:                                             "export trigger",
		`id="sandbox-profile-import-open"`:                                             "import trigger",
		`id="sandbox-profile-export-modal"`:                                            "export modal",
		`id="sandbox-profile-import-modal"`:                                            "import modal",
		`id="sandbox-profile-import-conflict"`:                                         "conflict-policy selector",
		`function SandboxEditor(`:                                                      "component-owned structured editor",
		`function SandboxImport(`:                                                      "component-owned import flow",
		`function SandboxExport(`:                                                      "component-owned export flow",
		`function SandboxDiffModal(`:                                                   "component-owned normalized diff preview",
		`id="sandbox-profile-diff-modal"`:                                              "sandbox diff confirmation overlay",
		`id="sandbox-profile-diff-body"`:                                               "line-by-line JSON diff",
		`lineDiff(beforeRaw, afterRaw)`:                                                "edits render as an LCS line diff",
		`previewSandboxProfile`:                                                        "save validates before commit",
		`preview.revision || ''`:                                                       "commit is coupled to preview revision",
		`await options.onCreate?.(preview.after.name)`:                                 "successful create hands off canonical name",
		`apply_assignments: false`:                                                     "import never applies assignments",
		`id: 'dashboard-default-sandbox-profile'`:                                      "global quick assignment chip",
		`id="dashboard-default-sandbox-profile-control"`:                               "stable global inline-picker host",
		`'set-group-sandbox-profile' : 'set-group-profile'`:                            "group quick assignment chip",
		`openSandboxProfileEditor(null, { onCreate:`:                                   "quick-create assignment handoff",
		`id="agent-spawn-sandbox-profile"`:                                             "explicit spawn selector",
		`descriptionID="agent-spawn-sandbox-profile-preview"`:                          "redacted effective preview",
		`SANDBOX_PROFILE_NONE : draft.sandboxProfile`:                                  "forced profile omission stays visible",
		`body.omit_sandbox_profiles = true`:                                            "explicit and mode-forced omissions reach the daemon",
		`function bindSandboxProfilesUI()`:                                             "compatibility binder",
		`async loadSandboxPolicy(groupName, selected = '')`:                            "spawn preview refresh",
		`const request = ++sandboxRequest.current`:                                     "out-of-order preview guard",
		`request !== sandboxRequest.current`:                                           "stale preview rejection",
		`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`:                 "group provenance lookup",
		`class="sbx-cap-tag sbx-cap-env"`:                                              "environment bindings render with the env tag",
		`class="sbx-cap-tag sbx-cap-inc"`:                                              "included profiles render with the include tag",
		`.sbx-cap-inc   { color: #d2a8ff;`:                                             "included-profile tags retain their purple styling",
		"const binding = `${entry.name} → ${entry.value}`":                             "static environment binding includes its value",
		`title=${binding}>${binding}`:                                                  "full environment binding remains available when truncated",
		`id="sandbox-profile-editor-common-rules"`:                                     "the common-rule preset menu rides on the filesystem table",
		`id="sandbox-profile-editor-show-global-filesystem"`:                           "inherited global filesystem rows have an explicit visibility control",
		`const [showGlobalFilesystem, setShowGlobalFilesystem] = useState(false)`:      "inherited global filesystem rows start folded",
		`id="sandbox-profile-editor-global-harness-filter"`:                            "shown inherited rows have a harness display filter",
		`<option value="none">None</option>`:                                           "the harness filter can hide every builtin row",
		`globalFilesystemForHarness(globalFilesystem, globalHarnessFilter)`:            "the harness filter narrows rows and provenance",
		`function TemplateManagerSlot(`:                                                "template polling is scoped to the template-manager overlay",
		`function DialogSlot(`:                                                         "ordinary management dialogs own their signal subscriptions",
		`class="sbx-row sbx-global-row" role="group" title=${tooltip}`:                 "global harness rule provenance uses a regular tooltip",
		`readonly aria-readonly="true"`:                                                "global config paths cannot be edited into the named profile",
		`globalFilesystemRuleTooltip(row)`:                                             "immutable rows explain their harness config provenance",
		`.sbx-global-harness {`:                                                        "Claude/Codex provenance stays visible without opening a tooltip",
		"access: 'deny' }))] }))":                                                      "a preset inserts ordinary deny rows, not a stored mechanism",
		`id="sandbox-profile-editor-common-rule-notice"`:                               "an insertion reports what it added, warning included",
		`class="sbx-common-rule-warn"`:                                                 "preset warnings are visible before and after insertion",
		`.sbx-common-rule-notice {`:                                                    "the insertion notice has its own caution styling",
		`function loadCommonRuleCatalog()`:                                             "the repurposed catalog feeds the preset menu",
		`[1, 2, 3, 4, 5, 6, 7].includes(parsed?.format_version)`:                       "import accepts every export envelope version, including access-axis v7",
		`if (body?.code) error.code = body.code`:                                       "request failures preserve the daemon's typed code",
		`id="sandbox-profile-import-include-error"`:                                    "per-policy include errors render in the import preview",
		`conflict === 'skip' ? 'skip' : 'overwrite'`:                                   "the error policy shares the all-incoming overwrite graph",
		`const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft']`:                      "draft-only scribe grant",
		`Never create, edit, delete, assign, or apply a sandbox profile`:               "scribe safety brief",
		`Agent draft loaded. Review every field`:                                       "explicit human preview",
		"fetch(`/api/sandbox-profile-drafts/":                                          "draft handoff polling",
		`createSandboxDraftQueue`:                                                      "parallel draft queue",
		`sandboxDraftQueue.enqueue({ draft, targetName, onCreate, editorOptions })`:    "completed drafts retain their editor mode",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
	if !strings.Contains(dashboardAssets, `body.wizard #sandbox-profile-editor-modal .sbx-section input:not([type])`) {
		t.Error("wizard structured sandbox inputs lost the arcane skin")
	}

	// TCL-791 removed break-glass. Nothing on any dashboard surface may still
	// name it: not an editor field, not an acknowledgement checkbox, not a
	// warning banner, not the wire key. These needles are the tripwire for a
	// partial revert that restores the UI without the daemon, or vice versa.
	for _, retired := range []string{
		`break_glass_filesystem`, `break_glass_acknowledged`, `BREAK_GLASS_WARNING`,
		`BREAK_GLASS_ACK_CODE`, `breakGlassAssignmentPrompt`, `confirmBreakGlassSpawn`,
		`sandbox-break-glass.js`, `sbx-bg-warning`, `sbx-cap-bg`, `sbx-bg-ack`,
		`function paintSandboxProfiles(`,
		`function bindLegacySandboxProfilesUI(`, `profileCapabilitiesHTML(`,
		`const current = state.view.value; const descriptor = current.dialog;`,
		`data-sandbox-profile-action=`, `id="sandbox-profile-global"`, `Validated policy to save:`,
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("retired sandbox manager ownership remains: %q", retired)
		}
	}
}

func TestDashboardSandboxCreateCapturesAssignmentTarget(t *testing.T) {
	js := compactDashboardSource(dashboardAssets)
	capture := strings.Index(js, compactDashboardSource("const body = { name: draft.name.trim()"))
	request := strings.Index(js, compactDashboardSource("await sandbox.previewSandboxProfile(targetName, body)"))
	handoff := strings.Index(js, compactDashboardSource("await options.onCreate?.(preview.after.name)"))
	if capture < 0 || request < capture || handoff < request {
		t.Fatalf("sandbox save must capture the draft, preview it, commit it, then hand off its canonical name (capture=%d request=%d handoff=%d)", capture, request, handoff)
	}
}

func TestDashboardNamedNewSandboxScribeDraftRemainsCreate(t *testing.T) {
	for needle, why := range map[string]string{
		`openSandboxProfileEditor(draft.profile, { ...editorOptions, targetName, onCreate, notice:`: "scribe preserves the explicit target and create mode",
		`openSandboxProfileEditor(seed, { ...editorOptions, targetName, onCreate, notice:`:          "scribe failure preserves the explicit target and create mode",
		`editExisting ? options.targetName || seed?.name || '' : ''`:                                "management action distinguishes a named create draft from an edit target",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
}
