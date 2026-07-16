package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestTerminalShellModalReconnectAndBackdropParity(t *testing.T) {
	actions := readDashboardJS(t, "terminal-shell-actions.js")
	island := readDashboardJS(t, "terminal-shell-island.js")
	if strings.Contains(actions, "setTimeout(connect") {
		t.Error("terminal shell silently reconnects instead of asking the operator")
	}
	for _, needle := range []string{
		"async function promptModalReconnect(id)",
		"title: 'Terminal disconnected'",
		"okLabel: 'Reconnect'",
		"if (disposed || confirmOpen",
		"onDisconnect=${() => actions.onModalDisconnect(descriptor.id)}",
		"if (event.currentTarget === event.target) void actions.confirmModalClose(descriptor.id)",
	} {
		if !strings.Contains(actions+island, needle) {
			t.Errorf("Preact terminal modal missing parity contract %q", needle)
		}
	}
	if strings.Contains(island, "onKeyDown") && strings.Contains(island, "confirmModalClose(descriptor.id)") {
		// Pane tabs legitimately own onKeyDown. Pin the modal's explicit lack of an
		// Escape close through its dedicated component block instead.
		start := strings.Index(island, "function TerminalModalSession(")
		end := strings.Index(island[start:], "\nfunction TerminalModal(")
		if start >= 0 && end >= 0 && strings.Contains(island[start:start+end], "event.key === 'Escape'") {
			t.Error("terminal modal must leave Escape to xterm")
		}
	}
}

func TestTerminalShellModalDetachCloseAndCopyMapping(t *testing.T) {
	actions := readDashboardJS(t, "terminal-shell-actions.js")
	island := readDashboardJS(t, "terminal-shell-island.js")
	for _, needle := range []string{
		"confirm(descriptor.seed.hideConv ?",
		"okLabel: 'Detach'",
		"okLabel: 'Close terminal'",
		"return closeModal(id, { detach: true })",
		"await closeModal(id, { detach: true })",
		"if (!conv) return",
		"fetchImpl(`/api/hide/${encodeURIComponent(conv)}`",
		"onClick=${() => void actions.detachModal(descriptor.id)}",
		"onClick=${() => void actions.confirmModalClose(descriptor.id)}",
		"descriptor.seed.hideConv ? html`",
	} {
		if !strings.Contains(actions+island, needle) {
			t.Errorf("Preact terminal modal missing detach/close contract %q", needle)
		}
	}
	detach := strings.Index(actions, "okLabel: 'Detach'")
	branch := strings.Index(actions, "} : {")
	close := strings.Index(actions, "okLabel: 'Close terminal'")
	if detach < 0 || branch < detach || close < branch {
		t.Error("hideConv copy mapping must offer Detach for live windows and Close terminal for throwaway shells")
	}
}

func TestTerminalShellModalCallersPreserveLiveSessionIdentity(t *testing.T) {
	spawn := readDashboardJS(t, "agent-spawn-actions.js")
	if strings.Contains(spawn, "payload.focus_ws") && !strings.Contains(spawn, "hideConv: payload.conv_id") {
		t.Error("spawn auto-focus must carry hideConv into the Preact terminal modal")
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
	for _, host := range []string{`id="terminals-root"`, `id="terminals-badge-root"`, `id="terminal-session-root"`} {
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
		"name: 'terminals'", "#terminals-root", "#terminals-badge-root", "#terminal-session-root",
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
		"confirm: confirmModal",
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
