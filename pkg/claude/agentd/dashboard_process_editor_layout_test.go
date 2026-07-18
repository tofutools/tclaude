package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardProcessEditorWorkspaceLayout pins the editor's one bounded
// layout chain. The browser smoke covers computed geometry; these asset checks
// keep viewport arithmetic and an accidental broken flex/grid boundary from
// returning in ordinary CI.
func TestDashboardProcessEditorWorkspaceLayout(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("read dashboard.css: %v", err)
	}
	css := string(cssBytes)
	mustRule := func(selector string, declarations ...string) {
		t.Helper()
		start := strings.Index(css, selector+" {")
		if start < 0 {
			t.Errorf("dashboard.css missing rule %q", selector)
			return
		}
		end := strings.Index(css[start:], "}")
		if end < 0 {
			t.Errorf("dashboard.css rule %q is unterminated", selector)
			return
		}
		block := css[start : start+end]
		for _, declaration := range declarations {
			if !strings.Contains(block, declaration) {
				t.Errorf("dashboard.css rule %q missing %q", selector, declaration)
			}
		}
	}
	mustRule("body:has(#tab-processes.active #process-editor-view)",
		"height: 100vh;", "display: flex;", "flex-direction: column;")
	mustRule("body:has(#tab-processes.active #process-editor-view) #processes-root",
		"flex: 1 1 auto;", "min-height: 0;", "display: flex;", "flex-direction: column;")
	mustRule("body:has(#tab-processes.active #process-editor-view) .processes-island",
		"flex: 1 1 auto;", "min-height: 0;", "display: grid;", "grid-template-rows: auto minmax(0, 1fr);", "gap: 12px;")
	mustRule("body:has(#tab-processes.active #process-editor-view) .process-canvas-view",
		"display: grid;", "grid-template-rows: auto minmax(0, 1fr);", "overflow-y: auto;", "overscroll-behavior: contain;")
	mustRule("#process-editor-canvas.process-editor-mount",
		"--process-editor-min-height: 440px;", "height: 100%;", "min-height: var(--process-editor-min-height);", "overflow: hidden;")

	for _, needle := range []string{
		"body:has(#tab-processes.active #process-editor-view) main,",
		".process-editor { display: flex; flex-direction: column; height: 100%; min-height: 0; }",
		".process-editor-stage { position: relative; flex: 1; min-width: 0; min-height: 0; display: flex; }",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("dashboard.css missing %q", needle)
		}
	}
	for _, brittle := range []string{
		".process-canvas-view { min-height: 62vh; }",
		".process-editor { display: flex; flex-direction: column; min-height: 62vh; }",
		"min-height: 55vh",
		"max-height: 68vh",
	} {
		if strings.Contains(css, brittle) {
			t.Errorf("dashboard.css restored brittle editor viewport sizing %q", brittle)
		}
	}

	islandBytes, err := fs.ReadFile(dashboardAssetsFS, "js/processes-island.js")
	if err != nil {
		t.Fatalf("read processes-island.js: %v", err)
	}
	island := string(islandBytes)
	renderStart := strings.Index(island, "return html`<div class=\"processes-island\"")
	if renderStart < 0 {
		t.Fatal("ProcessesApp root markup missing")
	}
	renderMarkup := island[renderStart:]
	ordered := []string{
		"class=\"process-subnav\"",
		"process-canvas-view${spec.kind === 'editor' ? ' process-scroll-surface' : ''}",
		"data-process-close-view",
		"<${ProcessEditorBoundary}",
	}
	last := -1
	for _, needle := range ordered {
		at := strings.Index(renderMarkup, needle)
		if at < 0 {
			t.Fatalf("ProcessesApp layout markup missing %q", needle)
		}
		if at <= last {
			t.Fatalf("ProcessesApp layout markup %q is out of order", needle)
		}
		last = at
	}
}
