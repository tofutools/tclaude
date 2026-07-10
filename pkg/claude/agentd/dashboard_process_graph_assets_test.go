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
		"data-morph-owned",
		"onPortDragStart",
		"onCanvasDrop",
		"this.nodeLayer.contains(node)",
		"this.portLayer.contains(port)",
		"captureFocus()",
		"'data-edge-id': edge.id",
		"'aria-pressed': 'false'",
		"process-shape-decision",
		"process-shape-compound")
	layoutBeforeOwnership := strings.Index(graph, "this.layout = layoutProcessGraph(this.graph")
	ownershipAfterRender := strings.Index(graph, "container.setAttribute('data-morph-owned', 'process-graph')")
	if layoutBeforeOwnership < 0 || ownershipAfterRender < 0 || layoutBeforeOwnership > ownershipAfterRender {
		t.Error("ProcessGraph must validate/layout before marking or emptying the live morph host")
	}

	morph := read("js/morph.js")
	mustContain("morph.js", morph,
		"if (from.hasAttribute('data-morph-owned')) {",
		"if (fromParent.nodeType === 1 && fromParent.hasAttribute('data-morph-owned')) return;")

	css := read("dashboard.css")
	mustContain("dashboard.css", css,
		".process-graph[data-color-scheme=\"light\"]",
		".process-edge-back .process-edge-path",
		"body.wizard .process-graph",
		".process-fit-button")

	// The core must stay inert and feature-flag-safe: no production dashboard
	// entry module imports it. The Processes host ticket imports it only after
	// config.ProcessesEnabled gates that tab; this standalone ticket adds no tab.
	for _, entry := range []string{"js/dashboard.js", "js/tabs.js"} {
		if strings.Contains(read(entry), "process-graph.js") {
			t.Errorf("%s eagerly imports process-graph.js; flag-off must render nothing", entry)
		}
	}
}
