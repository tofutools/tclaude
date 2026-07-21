package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_SandboxProfilesUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="sandbox-profiles-manage-open"`:                                          "Groups menu entry",
		`id="management-root"`:                                                       "shared Preact management root",
		`id="sandbox-profile-editor-modal"`:                                          "Preact profile editor",
		`id="sandbox-profile-editor-filesystem"`:                                     "raw filesystem editor",
		`id="sandbox-profile-editor-environment"`:                                    "raw environment editor",
		`id="sandbox-profile-editor-includes"`:                                       "raw includes editor",
		`id="sandbox-profile-editor-agent-directories"`:                              "raw agent-owned directory editor",
		`id="sandbox-profile-editor-submit"`:                                         "stable submit hook for plain and wizard skins",
		`class="sbx-agent-name"`:                                                     "structured agent-owned directory row",
		`.sbx-section input:not([type])`:                                             "Preact structured inputs retain dark modal styling",
		`.sbx-section select`:                                                        "Preact structured selects retain dark modal styling",
		`.sbx-row button`:                                                            "Preact structured row buttons retain dark modal styling",
		`.sbx-row button:last-child:hover`:                                           "Preact remove buttons retain destructive hover styling",
		`#sandbox-profile-editor-submit:disabled::before`:                            "wizard saving state suppresses the decorative submit label",
		`/api/sandbox-profile-directories/inspect`:                                   "missing-directory inspection",
		`/api/sandbox-profile-directories/create`:                                    "explicit missing-directory creation",
		`agent_directories: draft.agent_directories`:                                 "agent-owned directories persist in save payloads",
		`network_access: draft.network_access || ''`:                                 "network posture persists in save payloads",
		`id="sandbox-profile-editor-network"`:                                        "structured network posture selector",
		`id="sandbox-profile-scribe-open"`:                                           "new-profile agent configuration",
		`id="sandbox-profile-editor-scribe"`:                                         "current-draft agent configuration",
		`id="sandbox-profile-export-open"`:                                           "export trigger",
		`id="sandbox-profile-import-open"`:                                           "import trigger",
		`id="sandbox-profile-export-modal"`:                                          "export modal",
		`id="sandbox-profile-import-modal"`:                                          "import modal",
		`id="sandbox-profile-import-conflict"`:                                       "conflict-policy selector",
		`function SandboxEditor(`:                                                    "component-owned structured editor",
		`function SandboxImport(`:                                                    "component-owned import flow",
		`function SandboxExport(`:                                                    "component-owned export flow",
		`function SandboxDiffModal(`:                                                 "component-owned normalized diff preview",
		`id="sandbox-profile-diff-modal"`:                                            "sandbox diff confirmation overlay",
		`id="sandbox-profile-diff-body"`:                                             "line-by-line JSON diff",
		`lineDiff(beforeRaw, afterRaw)`:                                              "edits render as an LCS line diff",
		`previewSandboxProfile`:                                                      "save validates before commit",
		`preview.revision || ''`:                                                     "commit is coupled to preview revision",
		`await options.onCreate?.(preview.after.name)`:                               "successful create hands off canonical name",
		`apply_assignments: false`:                                                   "import never applies assignments",
		`id: 'dashboard-default-sandbox-profile'`:                                    "global quick assignment chip",
		`id="dashboard-default-sandbox-profile-control"`:                             "stable global inline-picker host",
		`'set-group-sandbox-profile' : 'set-group-profile'`:                          "group quick assignment chip",
		`openSandboxProfileEditor(null, { onCreate:`:                                 "quick-create assignment handoff",
		`id="agent-spawn-sandbox-profile"`:                                           "explicit spawn selector",
		`descriptionID="agent-spawn-sandbox-profile-preview"`:                        "redacted effective preview",
		`const sandboxProfilesDisabled = draft.harness === 'codex'`:                  "danger-full-access compatibility predicate",
		`disabled=${view.sandboxProfilesDisabled}`:                                   "sandbox changes update profile visibility",
		`if (!view.sandboxProfilesDisabled && draft.sandboxProfile)`:                 "disabled profiles are omitted from spawn requests",
		`function bindSandboxProfilesUI()`:                                           "compatibility binder",
		`async loadSandboxPolicy(groupName, selected = '')`:                          "spawn preview refresh",
		`const request = ++sandboxRequest.current`:                                   "out-of-order preview guard",
		`request !== sandboxRequest.current`:                                         "stale preview rejection",
		`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`:               "group provenance lookup",
		`class="sbx-cap-tag sbx-cap-env"`:                                            "environment bindings render with the env tag",
		`class="sbx-cap-tag sbx-cap-inc"`:                                            "included profiles render with the include tag",
		`.sbx-cap-inc   { color: #d2a8ff;`:                                           "included-profile tags retain their purple styling",
		"const binding = `${entry.name} → ${entry.value}`":                           "static environment binding includes its value",
		`title=${binding}>${binding}`:                                                "full environment binding remains available when truncated",
		`id="sandbox-profile-editor-read-baseline"`:                                  "strict read-baseline selector",
		`read_baseline: draft.read_baseline || ''`:                                   "read baseline persists in save payloads",
		`break_glass_filesystem: draft.break_glass_filesystem || []`:                 "break-glass rules persist in save payloads",
		`id="sandbox-profile-editor-break-glass"`:                                    "raw break-glass editor",
		`id="sandbox-profile-editor-break-glass-ack"`:                                "editor break-glass acknowledgement",
		`id="sandbox-profile-import-break-glass-ack"`:                                "import break-glass acknowledgement",
		`break_glass_acknowledged: true`:                                             "explicit acknowledgement rides commit bodies",
		`[1, 2, 3, 4].includes(parsed?.format_version)`:                              "import accepts v4 export envelopes",
		`read_baseline_exclusions: draft.read_baseline_exclusions || []`:             "read exclusions persist in save payloads",
		`id="sandbox-profile-diff-read-exclusions"`:                                  "diff confirmation keeps read exclusions visible",
		`class="sbx-exclusion-choice"`:                                               "exclusion rows stay a compact checkbox plus short label",
		`class="sbx-exclusion-badge"`:                                                "locked and inherited exclusions keep a visible badge",
		`open=${helpOpen === helpID}`:                                                "exclusion help opens one row at a time behind [?]",
		`const [exclusionHelpOpen, setExclusionHelpOpen] = useState('')`:             "the open disclosure is lifted so rows cannot both expand",
		`id="sandbox-profile-editor-exclusions-fold"`:                                "the restriction catalog ships folded behind a summary",
		`const exclusionCatalogLoaded = (readExclusionCatalog.version || 0) > 0`:     "unknown restrictions are only classified against a loaded catalog",
		`if (unknownExclusionKey) setExclusionsOpen(true)`:                           "an unknown restriction unfolds the section instead of hiding",
		`unknownExclusions.map((entry) => entry.id).sort().join('\0')`:               "the unfold re-fires when the unknown ID set changes, not just its emptiness",
		`.sbx-exclusion-row .spawn-field-description:focus,`:                         "exclusion help popovers reuse the spawn-dialog disclosure styling",
		`export const BREAK_GLASS_ACK_CODE = 'break_glass_acknowledgement_required'`: "typed 422 code is shared wire vocabulary",
		`if (body?.code) error.code = body.code`:                                     "request failures preserve the daemon's typed code",
		`error?.code === BREAK_GLASS_ACK_CODE`:                                       "save recovery keys off the typed code, not message text",
		`id="sandbox-profile-import-include-error"`:                                  "per-policy include errors render in the import preview",
		`id="sandbox-profile-editor-recovery"`:                                       "failed registry reload blocks saving with a recovery banner",
		`id="sandbox-profile-editor-recovery-retry"`:                                 "explicit registry-reload retry affordance",
		`return { breakGlassAckRequired: true, recovered }`:                          "save recovery reports reload success distinctly from failure",
		`registryOk = (await actions.load('sandbox')) === true`:                      "import recovery reloads the local registry, not just the inspect",
		`registryRecoveryRequired.value = true`:                                      "a failed registry reload sticks until an authoritative reload succeeds",
		`const sandboxRegistryRecoveryRequired = signal(false)`:                      "the stale-registry marker outlives the import dialog lifecycle",
		`conflict === 'skip' ? 'skip' : 'overwrite'`:                                 "the error policy shares the all-incoming overwrite graph",
		`id="sandbox-profile-diff-break-glass"`:                                      "diff confirmation keeps break-glass visible",
		`⚠ BREAK-GLASS protected access`:                                             "resolved preview marks break-glass with a persistent caveat",
		`read baseline: minimal`:                                                     "resolved preview names the strict baseline and its origin",
		`class="sbx-cap-tag sbx-cap-bg"`:                                             "break-glass card tag renders as danger",
		`class="sbx-cap-tag sbx-cap-baseline"`:                                       "minimal read baseline card chip",
		`.sbx-bg-warning {`:                                                          "prominent break-glass warning styling",
		`request.body.break_glass_acknowledged = true`:                               "spawn acknowledgement follows the resolved policy",
		`confirmBreakGlassSpawn`:                                                     "spawn-time break-glass confirmation",
		`breakGlassAssignmentPrompt`:                                                 "assignment surfaces share the danger prompt",
		`export const BREAK_GLASS_WARNING`:                                           "single source for the concrete consequence copy",
		`const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft']`:                    "draft-only scribe grant",
		`Never create, edit, delete, assign, or apply a sandbox profile`:             "scribe safety brief",
		`Agent draft loaded. Review every field`:                                     "explicit human preview",
		"fetch(`/api/sandbox-profile-drafts/":                                        "draft handoff polling",
		`createSandboxDraftQueue`:                                                    "parallel draft queue",
		`sandboxDraftQueue.enqueue({ draft, targetName, onCreate })`:                 "completed drafts are retained",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
	if !strings.Contains(dashboardAssets, `body.wizard #sandbox-profile-editor-modal .sbx-section input:not([type])`) {
		t.Error("wizard structured sandbox inputs lost the arcane skin")
	}

	for _, retired := range []string{
		`function paintSandboxProfiles(`,
		`function bindLegacySandboxProfilesUI(`, `profileCapabilitiesHTML(`,
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
		`openSandboxProfileEditor(draft.profile, { targetName, onCreate, notice:`: "scribe preserves the explicit target",
		`openSandboxProfileEditor(seed, { targetName, onCreate, notice:`:          "scribe failure preserves the explicit target",
		`targetName || seed?.name || ''`:                                          "management action forwards the intended edit target",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
}
