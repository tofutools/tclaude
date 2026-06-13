package agentd

import (
	"strings"
	"testing"
)

// The spawn dialog remembers, per Model, the effort the human last
// spawned with, so re-selecting that model re-applies it (e.g. high
// for fable, xhigh for opus). The memory lives entirely in the
// dashboard's embedded JS (a localStorage model→effort map in
// modal-spawn.js) — there is no server code path a flow test can
// exercise, and the repo has no JS test runner.
//
// Following the established dashboard_*_test.go structural guards
// (dnd-confirm, context-meter, wtsync), this pins the *shape* of the
// per-model effort memory: the three helpers exist, the open path and
// the Model-change listener re-apply the remembered effort, and submit
// persists it. A refactor that drops any leg silently breaks the
// feature in the browser; this fails it at `go test ./...`.

// spawnFuncBody returns the source span of a top-level modal-spawn.js
// function (column-0 in the ES module): from `function <name>(` up to
// the next column-0 `function ` / `async function ` definition, then
// trimmed back to this function's own closing brace ("\n}").
func spawnFuncBody(t *testing.T, name string) string {
	t.Helper()
	start := strings.Index(dashboardAssets, "\nfunction "+name+"(")
	if start < 0 {
		start = strings.Index(dashboardAssets, "\nasync function "+name+"(")
	}
	if start < 0 {
		t.Fatalf("dashboard assets: function %s not found", name)
	}
	start++ // skip the leading newline so start points at the keyword
	rest := dashboardAssets[start+1:]
	// Bound by whichever top-level definition comes first after `start`.
	end := len(rest)
	for _, marker := range []string{"\nfunction ", "\nasync function "} {
		if i := strings.Index(rest, marker); i >= 0 && i < end {
			end = i
		}
	}
	body := rest[:end]
	if i := strings.LastIndex(body, "\n}"); i >= 0 {
		body = body[:i+len("\n}")]
	}
	return body
}

func TestDashboardHTML_SpawnEffortMemory(t *testing.T) {
	// The three helpers and the stable localStorage key must exist —
	// the key is the persistence contract, a rename silently orphans
	// every human's saved defaults.
	for _, needle := range []string{
		"const SPAWN_MODEL_EFFORT_KEY = 'tclaude.dash.spawn.modelEffort';",
		"function loadModelEffortMap(",
		"function rememberModelEffort(",
		"function applyRememberedEffort(",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — per-model effort memory wiring broken", needle)
		}
	}

	// Open path: the modal restores the selected model's remembered
	// effort instead of always blanking the Effort field.
	open := spawnFuncBody(t, "openAgentSpawnModal")
	if !strings.Contains(open, "applyRememberedEffort($('#agent-spawn-model').value)") {
		t.Error("openAgentSpawnModal: must applyRememberedEffort for the selected model on open")
	}

	// Submit path: spawning persists the chosen effort for the chosen
	// model, so the next dialog can restore it.
	submit := spawnFuncBody(t, "submitAgentSpawn")
	if !strings.Contains(submit, "rememberModelEffort(model, effort)") {
		t.Error("submitAgentSpawn: must rememberModelEffort(model, effort) so the choice persists")
	}

	// Bind path: switching the Model live-re-applies that model's
	// remembered effort — without this the memory only takes effect on
	// modal open, not when the human changes models mid-dialog.
	bind := spawnFuncBody(t, "bindAgentSpawnModal")
	change := strings.Index(bind, "$('#agent-spawn-model').addEventListener('change'")
	if change < 0 {
		t.Fatal("bindAgentSpawnModal: missing a change listener on #agent-spawn-model")
	}
	if !strings.Contains(bind[change:], "applyRememberedEffort(e.target.value)") {
		t.Error("bindAgentSpawnModal: the Model change listener must applyRememberedEffort(e.target.value)")
	}
}
