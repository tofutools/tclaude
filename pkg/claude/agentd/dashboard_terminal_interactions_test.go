package agentd

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardTerminalInteractionsWired pins the three browser surfaces to the
// shared interaction module. The behavior itself is browser-owned, but a missed
// script/include/import otherwise fails only when a human opens that surface.
func TestDashboardTerminalInteractionsWired(t *testing.T) {
	core := readDashboardJS(t, "terminals-core.js")
	modal := readDashboardJS(t, "modal-term.js")
	interactions := readDashboardJS(t, "terminal-interactions.js")

	for name, src := range map[string]string{"terminals-core.js": core, "modal-term.js": modal} {
		for _, needle := range []string{
			"import { attachTerminalInteractions } from './terminal-interactions.js';",
			"attachTerminalInteractions({",
		} {
			if !strings.Contains(src, needle) {
				t.Errorf("%s missing %q", name, needle)
			}
		}
		if !strings.Contains(src, "macOptionClickForcesSelection: true") {
			t.Errorf("%s must enable Option-drag selection on macOS", name)
		}
	}
	for _, needle := range []string{
		"term.attachCustomKeyEventHandler(",
		"const input = terminalKeyInput(event)",
		"term.input(input)",
		"term.onSelectionChange(",
		"term.parser.registerOscHandler(52,",
		"const text = decodeOSC52(payload)",
		"beginGestureClipboardWrite()",
		"new ClipboardItemCtor({ 'text/plain': content })",
		"ownerDocument.addEventListener('mouseup', onTmuxMouseUp, true)",
		"token.deferred.resolve(text)",
		"activeTmuxClipboardCopy = token",
		"term.modes.mouseTrackingMode",
		"Ignore unsolicited OSC 52 completely",
		"navigator.clipboard.writeText(",
		"new globalThis.WebLinksAddon.WebLinksAddon(",
		"term.options.linkHandler = linkHandler",
		"url.protocol === 'http:' || url.protocol === 'https:'",
		"host.addEventListener('paste', onPaste, true)",
		"fetch('/api/terminal-attachments'",
		"term.paste(paths.join(' ') + ' ')",
		"if (controller.signal.aborted || generation !== myGeneration) return",
		"uploadController.abort()",
		"Option-drag to select on macOS; Shift-drag on Linux/Windows",
		"copyButton.dataset.hasSelection = selected ? 'true' : 'false'",
		"flash(SELECT_HINT);\n      term.focus();",
	} {
		if !strings.Contains(interactions, needle) {
			t.Errorf("terminal-interactions.js missing %q", needle)
		}
	}

	for _, name := range []string{"dashboard.html", "terminals.html"} {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(data), `/static/vendor/xterm/addon-web-links.min.js`) {
			t.Errorf("%s does not load the web-links addon", name)
		}
	}
	if data, err := fs.ReadFile(dashboardAssetsFS, "vendor/xterm/addon-web-links.min.js"); err != nil || len(data) < 1000 {
		t.Errorf("vendored web-links addon missing or unexpectedly small: bytes=%d err=%v", len(data), err)
	}
	if !strings.Contains(dashboardAssets, `id="term-session-copy"`) {
		t.Error("fallback terminal modal has no visible Copy action")
	}
	modalLiveStatus := `<span class="term-session-status" id="term-session-status" role="status"
        aria-live="polite" aria-atomic="true"></span>`
	if !strings.Contains(dashboardAssets, modalLiveStatus) {
		t.Error("fallback terminal status must be a polite atomic live region")
	}
	for _, jsAttr := range []string{
		`statusEl.setAttribute('role', 'status')`,
		`statusEl.setAttribute('aria-live', 'polite')`,
		`statusEl.setAttribute('aria-atomic', 'true')`,
	} {
		if !strings.Contains(core, jsAttr) {
			t.Errorf("mux terminal status missing live-region attribute wiring %q", jsAttr)
		}
	}
	for _, needle := range []string{
		"if (interactions) interactions.invalidate();",
		"interactions = attachTerminalInteractions({",
	} {
		if !strings.Contains(modal, needle) {
			t.Errorf("modal-term.js missing upload-session race guard %q", needle)
		}
	}
}

// TestTerminalAttachmentsRouteUsesBoundedUpload proves the terminal-specific
// route reaches the same authenticated/capped storage implementation as spawn
// attachments. That shared path is what makes remote-browser image paste work:
// bytes move through agentd rather than relying on its host OS clipboard.
func TestTerminalAttachmentsRouteUsesBoundedUpload(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	r := newSpawnAttachUpload(t, []uploadPart{{filename: "pasted-image.png", data: []byte("png")}})
	r.URL.Path = "/api/terminal-attachments"
	mux := http.NewServeMux()
	registerDashboardSpawnAttachmentRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("terminal attachment upload: status %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pasted-image.png") {
		t.Errorf("terminal attachment response missing stored file: %s", w.Body.String())
	}
}
