package agentd

import (
	"strings"
	"testing"
)

// The spawn modal's auto-normalize-name behaviour (config
// agent.spawn_name_normalize) is pure embedded JS + markup with no server
// path of its own to flow-test — the daemon/CLI halves are covered by
// spawn_name_flow_test.go and the agent package's unit tests. This guards
// the front-end half against a silent drop in a future refactor: the
// normalizer + its preview/commit wiring, the live-preview element, and the
// Config-tab opt-out checkbox all have to stay present. Mirrors
// TestDashboardHTML_WorktreeNameSyncWired.
func TestDashboardHTML_SpawnNameNormalizeWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The normalizer + its helpers (mirror of agent.NormalizeSpawnName).
	must("function normalizeSpawnName(", "client-side normalizer")
	must("export function spawnNameHint(", "controlled live preview of the normalized name")
	must("export function prepareSpawnDraft(", "applies the normalized name on blur/submit")

	// The flag the front-end gates on is the snapshot field the daemon sets.
	must("normalizeNames: snapshot.spawn_name_normalize !== false", "open snapshots the daemon-provided flag")

	// Wiring: live preview on input, commit on blur.
	must("onInput=${(event) => updateName(event.currentTarget.value)}",
		"name edits refresh the normalize preview")
	must("onBlur=${() => {",
		"leaving the field commits the normalized name")

	// The preview element + the Config-tab opt-out checkbox exist.
	must(`id="agent-spawn-name-hint"`, "live-preview element")
	must(`id="cfg-agent-spawnnormalize"`, "Config-tab opt-out checkbox")
}

// The name-derived session label (config agent.spawn_label_from_name) is
// daemon-side behaviour covered by spawn_label_flow_test.go; its only
// front-end surface is the Config-tab opt-in. Guard that the checkbox and both
// halves of its round-trip (load → checked, save → key) stay wired, so a
// future form refactor can't silently drop the toggle and strand the flag in
// config.json with no way to reach it.
func TestDashboardHTML_SpawnLabelFromNameWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	must(`id="cfg-agent-spawnlabelname"`, "Config-tab opt-in checkbox")
	must("$('#cfg-agent-spawnlabelname').checked = a.spawn_label_from_name === true",
		"load reflects the default-off flag")
	must("a.spawn_label_from_name = true", "save persists the explicit opt-in")
}
