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

// TestDashboardContextFeaturesSpawnActionForwarded pins the seam that actually
// broke: the spawn island calls actions.openContextFeatures(), but the actions
// object is a frozen, explicitly-listed façade built by createAgentSpawnActions.
// dashboard.js passed openContextFeatures in, the island called it — and the
// façade in between simply never named it, so the property was undefined and the
// Context… button threw on click instead of opening anything. The shape tests
// above all passed throughout, because every needle they check was present.
// Assert the dependency is both accepted and forwarded, in that one file.
func TestDashboardContextFeaturesSpawnActionForwarded(t *testing.T) {
	source := dashboardAssetFile(t, "js/agent-spawn-actions.js")
	start := strings.Index(source, "export function createAgentSpawnActions(")
	if start < 0 {
		t.Fatalf("agent-spawn-actions.js no longer exports createAgentSpawnActions")
	}
	head, body, ok := strings.Cut(source[start:], "} = {}) {")
	if !ok {
		t.Fatalf("createAgentSpawnActions no longer has a destructured dependency object")
	}
	if !regexp.MustCompile(`\bopenContextFeatures\b`).MatchString(head) {
		t.Errorf("createAgentSpawnActions does not accept openContextFeatures; the spawn " +
			"dialog's Context… button calls actions.openContextFeatures() and would throw")
	}
	// Either shape puts the property on the frozen object and makes the button
	// work: the explicit wrapper that mirrors openPermissions, or the shorthand
	// that passes the dependency straight through. Accept both — this test is
	// here to catch the property going MISSING, not to pin one spelling.
	forwarded := dashboardSourceContains(body, "openContextFeatures(options) { return openContextFeatures(options); }") ||
		regexp.MustCompile(`(?m)^\s*openContextFeatures,\s*$`).MatchString(body)
	if !forwarded {
		t.Errorf("createAgentSpawnActions does not forward openContextFeatures onto the frozen " +
			"actions object; the spawn dialog's Context… button would throw on click")
	}
}

// cssRule is one declaration block of the stylesheet, split into the selector
// list and the declarations, with comments already stripped.
type cssRule struct{ selectors, declarations string }

// dashboardCSSRules parses dashboard.css into declaration blocks.
//
// Stripping comments FIRST is the point. dashboard.css carries long explanatory
// comments that routinely name ids in prose ("the #agent-spawn-perms button note
// below…"), so any assertion that searches raw text can be satisfied — or
// derailed — by a sentence rather than a selector. With comments gone, a match
// on a selector list is a real selector.
func dashboardCSSRules(t *testing.T) []cssRule {
	t.Helper()
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(dashboardAssetFile(t, "dashboard.css"), "")
	var rules []cssRule
	for block := range strings.SplitSeq(css, "}") {
		selectors, declarations, ok := strings.Cut(block, "{")
		if !ok {
			continue
		}
		rules = append(rules, cssRule{selectors, declarations})
	}
	return rules
}

// TestDashboardContextFeaturesButtonsStyled guards the two CSS halves of the same
// regression. On the spawn dialog the Role-row Context… button shipped with no
// rule at all, so it fell back to the browser's default white chrome next to its
// dark Permissions… twin. In the profile editor it had a rule but rendered
// taller than its twin, because `.cron-create-row > button.tool` stretches a
// button to its row and only "Startup context" wraps to two lines.
//
// Both fixes are one-line CSS that nothing else in the package would notice
// going missing, so assert each rule pairs the two buttons.
func TestDashboardContextFeaturesButtonsStyled(t *testing.T) {
	rules := dashboardCSSRules(t)
	// pairs asserts that the rule matching `owner` + `marker` also names `twin`.
	pairs := func(what, owner, marker, twin string) {
		t.Helper()
		for _, rule := range rules {
			if !strings.Contains(rule.selectors, owner) || !strings.Contains(rule.declarations, marker) {
				continue
			}
			if !strings.Contains(rule.selectors, twin) {
				t.Errorf("%s: the rule `%s {%s}` does not also cover %s",
					what, strings.TrimSpace(rule.selectors), strings.TrimSpace(rule.declarations), twin)
			}
			return
		}
		t.Errorf("%s: no rule found selecting %s and declaring %s", what, owner, marker)
	}

	// Spawn dialog: the dark inline-button chrome, and its hover.
	pairs("spawn dialog Context… button renders with the browser's default (white) chrome",
		".spawn-role-row #agent-spawn-perms", "background:", "#agent-spawn-context-features")
	pairs("spawn dialog Context… button does not react to hover like its twin",
		".spawn-role-row #agent-spawn-perms:hover", "background:", "#agent-spawn-context-features:hover")

	// Profile editor: the opt-out from the .tool stretch. Without it the wrapped
	// "Startup context" label inflates the button past its Permissions… twin.
	pairs("profile editor Context… button is stretched taller than its twin by its wrapped label",
		"#profile-editor-context-features", "align-self: flex-start", "#profile-editor-perms")
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
