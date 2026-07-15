package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardShellPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, "js/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(body)
	}
	must := func(source, needle, why string) {
		t.Helper()
		if !strings.Contains(source, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
	mustNot := func(source, needle, why string) {
		t.Helper()
		if strings.Contains(source, needle) {
			t.Errorf("still contains %q (%s)", needle, why)
		}
	}

	htmlBody, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlBody)
	loader := read("preact-loader.js")
	dashboard := read("dashboard.js")
	groupsList := read("groups-list.js")
	memberTable := read("groups-member-table.js")
	refresh := read("refresh.js")
	model := read("shell-model.js")
	island := read("shell-island.js")
	state := read("shell-state.js")

	for _, host := range []string{
		"shell-activity-root", "shell-usage-root", "shell-status-root",
		"shell-notify-root", "shell-credits-root", "shell-messages-badge-root",
		"shell-meta-root", "shell-disconnect-root", "shell-confirm-root",
		"shell-toast-root", "shell-palette-button-root", "shell-palette-modal-root",
	} {
		must(html, `id="`+host+`"`, "the shell has an explicit stable host")
		must(loader, "#"+host, "the shell descriptor claims the host")
	}
	must(loader, "const shellDescriptor = createIslandDescriptor({", "the shell uses the guarded island lifecycle")
	must(loader, "mountShellIsland({", "core shell widgets mount through Preact")
	must(loader, "throw new Error('Dashboard shell failed to mount')", "critical feedback failure aborts bootstrap")
	must(loader, "mountNotifyIsland({", "notification settings mount through Preact")
	must(loader, "mountCreditsIsland({", "the credits counter mounts through Preact")
	must(loader, "mountPaletteIsland({", "the command palette mounts through Preact")
	must(dashboard, "pageCleanups.push(await mountShellFeature({ notify: toast }));", "page teardown owns the shell mount")
	must(dashboard, "for (const cleanup of pageCleanups.reverse()) cleanup?.();", "pagehide tears down every owner in reverse order")

	for _, needle := range []string{"document.", "fetch(", "innerHTML", "morphInto"} {
		mustNot(model, needle, "the pure shell model must not own browser effects")
	}
	for _, needle := range []string{"fetch(", "morphInto", "innerHTML"} {
		mustNot(island, needle, "the Preact shell renderer must not use legacy painting or API effects")
	}
	must(state, "const status = signal(", "status feedback is signal-owned")
	must(state, "const confirmation = signal(", "confirmation feedback is signal-owned")
	must(island, "state.snapshot.value", "snapshot-backed widgets read the accepted snapshot signal")
	must(island, "state.connection.value.status === 'disconnected'", "disconnect UI reads the shared connection signal")
	must(island, "mounted.slice().reverse()", "partial mounts can be rolled back safely")

	for _, legacy := range []string{
		"function renderGlobalActivity(", "function renderMessagesBadge(",
		"function renderUsage(", "function renderNotifyGlobal(", "function showStatus(",
	} {
		mustNot(groupsList, legacy, "the native Groups list does not own migrated shell DOM")
		mustNot(memberTable, legacy, "the native member table does not own migrated shell DOM")
	}
	for _, legacyCall := range []string{
		"renderGlobalActivity()", "renderMessagesBadge(", "renderUsage(",
		"renderNotifyGlobal(", "morphInto($('#meta')",
	} {
		mustNot(refresh, legacyCall, "snapshot refresh publishes state instead of repainting shell DOM")
	}
}
