package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_SandboxProfilesUI(t *testing.T) {
	for needle, why := range map[string]string{
		`id="sandbox-profiles-manage-open"`:                            "Groups menu entry",
		`id="sandbox-profiles-manage-modal"`:                           "management overlay",
		`id="sandbox-profile-editor-modal"`:                            "profile editor",
		`id="sandbox-profile-editor-filesystem"`:                       "filesystem editor",
		`id="sandbox-profile-editor-environment"`:                      "environment editor",
		`id="sandbox-profile-global"`:                                  "global assignment control",
		`id="sandbox-profile-group-value"`:                             "group assignment control",
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
