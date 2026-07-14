package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardProcessGraphAssets(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
		return string(body)
	}
	mustContain := func(name, source string, needles ...string) {
		t.Helper()
		for _, needle := range needles {
			if !strings.Contains(source, needle) {
				t.Errorf("%s missing %q", name, needle)
			}
		}
	}

	layout := read("js/process-layout.js")
	mustContain("process-layout.js", layout,
		"export function layoutProcessGraph(",
		"export function rerouteProcessLayout(",
		"export function defaultFeedbackArc(",
		"edge.back || ignored.has(edge.inputIndex)")
	if strings.Contains(layout, "Math.random(") {
		t.Error("process layout must remain deterministic; Math.random() is forbidden")
	}

	graph := read("js/process-graph.js")
	mustContain("process-graph.js", graph,
		"export class ProcessGraph",
		"export function createProcessGraph(",
		"export function normalizeWheelDelta(",
		"export function isGraphTypingTarget(",
		"this.onSpaceKey(event)",
		"this.options.wheelPan && !event.ctrlKey",
		"onNodeDragStart",
		"onPortDragStart",
		"onCanvasDrop",
		"onCanvasClick",
		"onMarqueeSelect",
		"this.pendingClickTarget",
		"this.renderTransientEdges(",
		"this.root.focus({ preventScroll: true })",
		// A browser-cancelled gesture must never commit: pointercancel routes
		// to the dedicated cancel path (cancelled: true, no hit-testing), not
		// the ordinary pointerup handler. TCL-296 review finding.
		"this.onPointerCancel(event)",
		"targetNodeId: null, targetPort: null, cancelled: true, event,",
		"this.nodeLayer.contains(node)",
		"this.portLayer.contains(port)",
		"captureFocus()",
		"'data-edge-id': edge.id",
		"'aria-pressed': 'false'",
		"process-shape-decision",
		"process-shape-compound",
		// TCL-299 badge surface: severity class on the overlay anchor, badge
		// glyph on edges, and viewport centering for issue navigation.
		"overlay-${overlay.severity}",
		"process-overlay-tooltip",
		"process-edge-badge-${edge.badgeSeverity || 'error'}",
		"centerOn(x, y)")
	if strings.Contains(graph, "data-morph-owned") {
		t.Error("process graph retains the deleted reconciler ownership marker")
	}

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-graph[data-color-scheme=\"light\"]",
		".process-edge-back .process-edge-path",
		"body.wizard .process-graph",
		".process-fit-button",
		// Severity palette exists in all three skins (dark/light/wizard set
		// their own --pg-error/--pg-warn values).
		"--pg-error",
		".process-overlay-anchor.overlay-error .process-overlay-ring",
		".process-edge-badge-warning")

	// The core must stay inert and feature-flag-safe: no production dashboard
	// entry module imports it. The Processes host ticket imports it only after
	// config.ProcessesEnabled gates that tab; this standalone ticket adds no tab.
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js"} {
		if strings.Contains(read(entry), "process-graph.js") {
			t.Errorf("%s eagerly imports process-graph.js; flag-off must render nothing", entry)
		}
	}
}
