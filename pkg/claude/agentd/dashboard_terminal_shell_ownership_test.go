package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestTerminalShellCallersPreserveLiveSessionIdentity(t *testing.T) {
	spawn := readDashboardJS(t, "agent-spawn-actions.js")
	if strings.Contains(spawn, "payload.focus_ws") && !strings.Contains(spawn, "hideConv: payload.conv_id") {
		t.Error("spawn auto-focus must carry hideConv into the Terminals tab")
	}

	rows := readDashboardJS(t, "row-action-handler.js")
	if !strings.Contains(rows, "hideConv: agent") {
		t.Error("open-window fallback must carry hideConv")
	}
	if n := strings.Count(rows, "hideConv:"); n != 1 {
		t.Errorf("exactly one row-action handler caller may inline hideConv; found %d", n)
	}

	tab := readDashboardJS(t, "terminals-tab.js")
	windowAt := strings.Index(tab, "export function openWebWindowPane(")
	termAt := strings.Index(tab, "export function openWebTermPane(")
	focusAt := strings.Index(tab, "export function focusTerminalForConv(")
	if windowAt < 0 || termAt < 0 || focusAt < 0 || windowAt >= termAt || termAt >= focusAt {
		t.Fatal("terminal controller helper order is malformed")
	}
	if !strings.Contains(tab[windowAt:termAt], "hideConv: agent") {
		t.Error("live web-window pane must carry hideConv")
	}
	if strings.Contains(tab[termAt:focusAt], "hideConv: agent") {
		t.Error("throwaway web-term pane must not carry hideConv")
	}
}

func TestTerminalShellPreactAtomicOwnership(t *testing.T) {
	htmlBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, host := range []string{`id="terminals-root"`, `id="terminals-badge-root"`} {
		if !strings.Contains(html, host) {
			t.Errorf("dashboard missing terminal Preact host %s", host)
		}
	}
	for _, retired := range []string{`id="term-session-modal"`, `id="term-session-xterm"`, `id="term-tab-tabs"`, `id="term-tab-panes"`} {
		if strings.Contains(html, retired) {
			t.Errorf("static dashboard terminal writer remains: %s", retired)
		}
	}
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/modal-term.js"); err == nil {
		t.Error("retired modal-term.js remains embedded")
	}

	loader := readDashboardJS(t, "preact-loader.js")
	for _, needle := range []string{
		"name: 'terminals'", "#terminals-root", "#terminals-badge-root",
		"mountTerminalShellIsland", "createTerminalShellActions",
	} {
		if !strings.Contains(loader, needle) {
			t.Errorf("terminal loader missing ownership contract %q", needle)
		}
	}
	controller := readDashboardJS(t, "terminals-tab.js")
	for _, forbidden := range []string{"document.", "querySelector", "createElement", "mountMux"} {
		if strings.Contains(controller, forbidden) {
			t.Errorf("terminal compatibility controller still writes DOM through %q", forbidden)
		}
	}
	dashboard := readDashboardJS(t, "dashboard.js")
	for _, needle := range []string{
		"mountTerminalsFeature({",
		"onComposeMessage: (seed) => openOperatorMessageDialog(seed)",
		"composeMessageDialogKind: activeMessageAccessDialogKind",
	} {
		if !strings.Contains(dashboard, needle) {
			t.Errorf("dashboard terminal ownership mount missing %q", needle)
		}
	}
	for _, retired := range []string{"bindTermModal", "initTerminalsTab", "modal-term.js"} {
		if strings.Contains(dashboard, retired) {
			t.Errorf("dashboard still binds retired terminal path %q", retired)
		}
	}
}
