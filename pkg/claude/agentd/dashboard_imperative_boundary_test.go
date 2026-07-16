package agentd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardImperativeOwnershipInventory(t *testing.T) {
	docsPath := filepath.Join("..", "..", "..", "docs", "dashboard.md")
	docsBytes, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	docs := string(docsBytes)
	categories := map[string]bool{
		"browser-io": true, "config-adapter": true, "cost-chart": true,
		"media-effects": true, "platform-layout": true, "preact-compat": true,
		"process-graph": true,
	}
	marker := regexp.MustCompile(`dashboard-imperative-boundary:\s*([a-z-]+)`)
	highRisk := regexp.MustCompile(`\.createElement(?:NS)?\s*\(|\.innerHTML\s*=|\.insertAdjacentHTML\s*\(|dangerouslySetInnerHTML`)

	for _, name := range dashboardJSModules() {
		body, readErr := fs.ReadFile(dashboardAssetsFS, name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		source := string(body)
		if !highRisk.MatchString(source) {
			continue
		}
		match := marker.FindStringSubmatch(source)
		if len(match) != 2 {
			t.Errorf("%s directly creates/injects DOM without an ownership marker; ordinary operator UI belongs in a Preact island. If this is a necessary opaque/browser boundary, add `// dashboard-imperative-boundary: <documented-category>` and document its owner, disposal, and behavioral test contract in docs/dashboard.md", name)
			continue
		}
		category := match[1]
		if !categories[category] {
			t.Errorf("%s uses undocumented imperative category %q; add a reviewed category with ownership, disposal, and behavioral tests to docs/dashboard.md", name, category)
		}
		if !strings.Contains(docs, "`"+category+"`") {
			t.Errorf("docs/dashboard.md does not describe imperative category %q used by %s", category, name)
		}
	}
}

func TestDashboardStaticShellHasNoOrdinaryDialogs(t *testing.T) {
	for _, name := range []string{"dashboard.html", "terminals.html"} {
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(body)
		for _, token := range []string{`role="dialog"`, `aria-modal="true"`, `class="modal-overlay`, `class="manage-overlay`} {
			if strings.Contains(source, token) {
				t.Errorf("%s contains static ordinary dialog markup %q; leave only an empty stable host and render the dialog from a keyed Preact island", name, token)
			}
		}
	}
}

func TestDashboardOperationExportsHaveDirectProducers(t *testing.T) {
	moved := []string{
		"shutdownScope", "powerOnScope", "openWindowModal", "openRetirePreview",
		"openRetireUngroupedPreview", "openDeleteRetiredPreview", "openWorktreeCleanup",
		"openDeleteGroupModal", "openCleanupModal", "resumeAgentReq",
	}
	operationsBytes, err := fs.ReadFile(dashboardAssetsFS, "js/dashboard-operations.js")
	if err != nil {
		t.Fatalf("read dashboard operations: %v", err)
	}
	refreshBytes, err := fs.ReadFile(dashboardAssetsFS, "js/refresh.js")
	if err != nil {
		t.Fatalf("read refresh: %v", err)
	}
	operations := string(operationsBytes)
	refresh := string(refreshBytes)
	refreshImports := regexp.MustCompile(`(?s)import\s*\{([^}]*)\}\s*from './refresh\.js'`).FindAllStringSubmatch(dashboardAssets, -1)

	for _, name := range moved {
		if !strings.Contains(operations, "function "+name) {
			t.Errorf("moved operation %s is no longer owned by dashboard-operations.js", name)
		}
		if strings.Contains(refresh, "function "+name) {
			t.Errorf("refresh.js regained the %s request/transaction launcher; polling must remain reconciliation-only", name)
		}
		for _, match := range refreshImports {
			if strings.Contains(match[1], name) {
				t.Errorf("%s is still compatibility-imported from refresh.js; import dashboard-operations.js directly", name)
			}
		}
		productionWithoutOwner := strings.ReplaceAll(dashboardAssets, operations, "")
		if !strings.Contains(productionWithoutOwner, name) {
			t.Errorf("moved operation %s has no live producer; delete the export instead of retaining compatibility surface", name)
		}
	}

	rowBytes, err := fs.ReadFile(dashboardAssetsFS, "js/row-actions.js")
	if err != nil {
		t.Fatalf("read row action binder: %v", err)
	}
	row := string(rowBytes)
	for _, forbidden := range []string{"switch (", "fetch(", ".dataset."} {
		if strings.Contains(row, forbidden) {
			t.Errorf("row-actions.js contains %q; it must remain a stateless live-producer binder", forbidden)
		}
	}
	for _, required := range []string{"Object.freeze({ ...source.dataset })", "!source.isConnected", "handleRowAction(actionDescriptor(source))"} {
		if !strings.Contains(row, required) {
			t.Errorf("row-actions.js missing %q; delegated routing must freeze plain data and reject stale producers", required)
		}
	}
}
