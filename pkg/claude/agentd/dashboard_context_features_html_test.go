package agentd

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Startup-context trimming (TCL-597) put a "Context…" button on the spawn
// dialog's Role row and a "Startup context" row in the spawn-profile editor, both
// opening one shared buffered tri-state selector. That wiring lives entirely in
// the embedded dashboard JS/HTML, so no server path proves it — this guards the
// shape. context_features_spawn_flow_test.go is the companion that exercises the
// endpoint behaviour.
func TestDashboardContextFeaturesUI_Wired(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// Spawn dialog — the Context… button and its trimmed/kept badge.
	present(`id="agent-spawn-context-features"`, "spawn dialog has the Context… button")
	present(`id="agent-spawn-context-features-indicator"`, "spawn dialog has the trims indicator")

	// Profile editor — the same control on a labelled row.
	present(`id="profile-editor-context-features"`, "profile editor has the Context… button")

	// One shared dialog, opened through the same controller seam the buffered
	// permission editor uses.
	present("function ContextFeaturesDialog(", "the trim selector has one shared Preact renderer")
	present("export function openContextFeaturesEditor(options = {})",
		"the controller exposes the trim-editor opener")
	present("openContextFeatures(options = {})", "the state owns a keyed trim-editor launch")

	// The three states must all be reachable — a two-state control could not
	// express "keep this even though the profile trimmed it".
	present(`data-state="default"`, "the selector offers Default")
	present(`data-state="on"`, "the selector offers Keep")
	present(`data-state="off"`, "the selector offers Trim")

	// The spawn body carries the map, and carries it UNCONDITIONALLY for a capable
	// harness: an explicitly empty selection has to beat a profile's trims, so
	// omitting the field when empty would silently re-apply them.
	present("body.context_features = { ...(draft.contextFeatures || {}) }",
		"the spawn body always sends the trim map for a capable harness")
	// The profile payload sends its own map (sparse — only real intent).
	present("body.context_features = { ...draft.context_features }",
		"the profile payload sends its trim map")

	// The harness catalog gates the control, so a Codex spawn never shows it.
	present("can_context_features", "the capability view gates on the harness flag")
}

// TestDashboardContextFeaturesEditorStacksAboveProfileEditor guards the same
// Escape-correctness invariant TestDashboardPermEditorStacksAboveProfileEditor
// does, for the same reason: the startup-context editor opens ON TOP of
// #profile-editor-modal from that editor's Context… button. Escape dismissal
// ranks overlays by computed z-index and breaks ties by DOM order, so if the two
// ever share a z-index Escape would close the editor BENEATH the visible one.
func TestDashboardContextFeaturesEditorStacksAboveProfileEditor(t *testing.T) {
	zIndexOf := func(selector string) int {
		t.Helper()
		re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{[^}]*z-index:\s*(\d+)`)
		m := re.FindStringSubmatch(dashboardAssets)
		if m == nil {
			t.Fatalf("no z-index rule found for %s", selector)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad z-index for %s: %v", selector, err)
		}
		return n
	}

	trims := zIndexOf("#context-features-modal")
	profile := zIndexOf("#profile-editor-modal")
	if trims <= profile {
		t.Errorf("#context-features-modal z-index (%d) must be strictly above "+
			"#profile-editor-modal (%d) so Escape closes the trim editor stacked on top of it, "+
			"not the editor beneath", trims, profile)
	}
}
