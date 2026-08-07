package agentd

import (
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// TestDashboardTerminalInteractionsWired pins the three browser surfaces to the
// shared interaction module. The behavior itself is browser-owned, but a missed
// script/include/import otherwise fails only when a human opens that surface.
func TestDashboardTerminalInteractionsWired(t *testing.T) {
	core := readDashboardJS(t, "terminals-core.js")
	shell := readDashboardJS(t, "terminal-shell-island.js")
	interactions := readDashboardJS(t, "terminal-interactions.js")

	for name, src := range map[string]string{"terminals-core.js": core} {
		for _, needle := range []string{
			"import { attachTerminalInteractions } from './terminal-interactions.js';",
			"interactionsFactory = attachTerminalInteractions",
			"interactionsFactory({",
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
		"if (isBrowserPasteShortcut(event)) return false",
		"if (applicationClipboardShortcuts && isTerminalClipboardRequestShortcut(event)) {",
		"armTmuxClipboardFromGesture();",
		"term.input('\\x03');",
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
		"term.registerLinkProvider(",
		"visibleLocalFileLinkProvider(term, linkHandler)",
		"term.options.linkHandler = linkHandler",
		"if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;",
		"if (url.protocol !== 'file:'",
		"allowNonHttpProtocols: true",
		"`/api/terminal-file?${query}`",
		// Userinfo renders as the victim host right up to the '@'.
		"if (url.username || url.password) return null;",
		"host.addEventListener('paste', onPaste, true)",
		"`/api/terminal-attachments?terminal=${encodeURIComponent(terminalPath)}`",
		"term.paste(paths.join(' ') + ' ')",
		"if (controller.signal.aborted || generation !== myGeneration) return",
		"uploadController.abort()",
		"Option-drag to select on macOS; Shift-drag on Linux/Windows",
		"copyButton.dataset.hasSelection = selected ? 'true' : 'false'",
		"flash(SELECT_HINT);\n      term.focus();",
		"isComposeMessageShortcut(event)",
		"onComposeMessage();",
		// An OSC 8 link's label is chosen independently of its target, so the
		// destination must be reachable before the human commits to Ctrl/Cmd-click.
		// This pins only that the handlers are WIRED; what they show is behaviour,
		// covered for real in jstest/terminal-interactions.test.mjs.
		"hover: (event, text) => showLinkTarget(text)",
		"leave: () => clearLinkTarget()",
	} {
		if !strings.Contains(interactions, needle) {
			t.Errorf("terminal-interactions.js missing %q", needle)
		}
	}
	if strings.Contains(interactions, "host.title") {
		t.Error("terminal surface must not use a native title tooltip; guidance belongs in non-overlaying UI")
	}
	if strings.Contains(interactions, "copyButton.title") {
		t.Error("terminal Copy action must expose guidance accessibly without a native title tooltip")
	}
	for _, needle := range []string{
		"class=\"terminal-interaction-hint\"",
		"Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C",
		">✉ Message</button>",
		"onComposeMessage=${composeMessage}",
	} {
		if !strings.Contains(shell, needle) {
			t.Errorf("mux terminal header missing persistent guidance %q", needle)
		}
	}

	for _, needle := range []string{
		"import { terminalComposeShortcutAction } from './terminal-compose-route.js';",
		"import { hasShownOverlay } from './overlay-stack.js';",
		"const pane = current.panes.find((candidate) => candidate.key === current.activeKey);",
		"restoreFocus: () => actions.activatePane(pane.key)",
		"const dialogKind = composeMessageDialogKind();",
		"operatorModalOpen: dialogKind === 'operator-message',",
		"blockingOverlayOpen: hasShownOverlay(),",
		"tabActive: solo || document.getElementById('tab-terminals')?.classList.contains('active'),",
		"if (action === 'ignore') return;",
		"document.addEventListener('keydown', onComposeShortcut, true);",
		"document.removeEventListener('keydown', onComposeShortcut, true);",
		"event.stopPropagation();",
	} {
		if !strings.Contains(shell, needle) {
			t.Errorf("integrated terminals shortcut missing %q", needle)
		}
	}
	composer := readDashboardJS(t, "message-access-dialog-island.js")
	actions := readDashboardJS(t, "message-access-dialog-actions.js")
	for _, needle := range []string{
		`id="operator-message-modal"`,
		`id="operator-message-attach-input"`,
		`id="operator-message-submit"`,
	} {
		if !strings.Contains(composer, needle) {
			t.Errorf("operator message composer missing %q", needle)
		}
	}
	if !strings.Contains(actions, "'/api/operator-message'") {
		t.Error("operator message action is not wired to /api/operator-message")
	}
	if !strings.Contains(shell, "<span class=\"terminal-interaction-hint\">${INTERACTION_HINT}</span>") {
		t.Error("fallback terminal modal missing persistent selection/copy guidance")
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
	if !strings.Contains(shell, `id="term-session-copy"`) {
		t.Error("fallback terminal modal has no visible Copy action")
	}
	if !strings.Contains(shell, `class="term-session-status" id="term-session-status" role="status" aria-live="polite" aria-atomic="true"`) {
		t.Error("fallback terminal status must be a polite atomic live region")
	}
	for _, jsAttr := range []string{
		`class="mux-pane-status" role="status" aria-live="polite" aria-atomic="true"`,
	} {
		if !strings.Contains(shell, jsAttr) {
			t.Errorf("mux terminal status missing live-region attribute wiring %q", jsAttr)
		}
	}
	for _, needle := range []string{
		"const interactions = interactionsFactory({",
		"terminalPath: wsPath",
		"if (disposed) return",
		"try { interactions.dispose(); }",
	} {
		if !strings.Contains(core, needle) {
			t.Errorf("opaque terminal adapter missing interaction lifecycle guard %q", needle)
		}
	}
}

// TestTerminalAttachmentsRouteUsesBoundedUpload proves the terminal-specific
// route reaches the same authenticated/capped storage implementation as spawn
// attachments. That shared path is what makes remote-browser image paste work:
// bytes move through agentd rather than relying on its host OS clipboard.
func TestTerminalAttachmentsRouteUsesBoundedUpload(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)
	_, err := db.CreateAgentGroup("terminal-downloads", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("terminal-downloads", t.TempDir())
	require.NoError(t, err)

	r := newSpawnAttachUpload(t, []uploadPart{{filename: "pasted-image.png", data: []byte("png")}})
	r.URL.Path = "/api/terminal-attachments"
	r.URL.RawQuery = "terminal=" + url.QueryEscape("/api/group-term-ws/terminal-downloads")
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

func TestTerminalFileRouteDownloadsHostFile(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	dir := t.TempDir()
	_, err := db.CreateAgentGroup("terminal-downloads", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("terminal-downloads", dir)
	require.NoError(t, err)
	path := filepath.Join(dir, "result å.md")
	require.NoError(t, os.WriteFile(path, []byte("# done\n"), 0o600))

	query := url.Values{
		"terminal": {"/api/group-term-ws/terminal-downloads"},
		"path":     {path},
	}.Encode()
	r := dashboardRequest(http.MethodGet, "/api/terminal-file?"+query, "")
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerDashboardSpawnAttachmentRoutes(mux)
	mux.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "# done\n", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "result")
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
}

func TestTerminalFileRouteRejectsInvalidTargets(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	dir := t.TempDir()
	_, err := db.CreateAgentGroup("terminal-downloads", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("terminal-downloads", dir)
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerDashboardSpawnAttachmentRoutes(mux)

	for name, path := range map[string]string{
		"relative":  "report.md",
		"directory": dir,
		"missing":   filepath.Join(dir, "missing.md"),
	} {
		t.Run(name, func(t *testing.T) {
			query := url.Values{
				"terminal": {"/api/group-term-ws/terminal-downloads"},
				"path":     {path},
			}.Encode()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, dashboardRequest(http.MethodGet,
				"/api/terminal-file?"+query, ""))
			assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
		})
	}

	path := filepath.Join(dir, "report.md")
	require.NoError(t, os.WriteFile(path, []byte("private"), 0o600))
	for name, terminal := range map[string]string{
		"unknown route":     "/not-a-terminal",
		"missing agent":     "/api/term-ws/not-an-agent?which=current",
		"invalid selector":  "/api/term-ws/agent/nested?which=current",
		"invalid directory": "/api/term-ws/agent?which=elsewhere",
		"missing group":     "/api/group-term-ws/ghost",
		"nested group":      "/api/group-term-ws/terminal-downloads/nested",
	} {
		t.Run(name, func(t *testing.T) {
			query := url.Values{"terminal": {terminal}, "path": {path}}.Encode()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, dashboardRequest(http.MethodGet,
				"/api/terminal-file?"+query, ""))
			assert.NotEqual(t, http.StatusOK, w.Code)
		})
	}
}

func TestTerminalFileRoutePreservesLongUnicodeFilename(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	dir := t.TempDir()
	_, err := db.CreateAgentGroup("terminal-downloads", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("terminal-downloads", dir)
	require.NoError(t, err)
	original := strings.Repeat("å", 70) + ".md"
	path := filepath.Join(dir, original)
	require.NoError(t, os.WriteFile(path, []byte("done"), 0o600))

	query := url.Values{
		"terminal": {"/api/group-term-ws/terminal-downloads"},
		"path":     {path},
	}.Encode()
	w := httptest.NewRecorder()
	handleDashboardTerminalFile(w, dashboardRequest(http.MethodHead,
		"/api/terminal-file?"+query, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	require.NoError(t, err)
	assert.Equal(t, sanitizeAttachmentFilename(original), params["filename"])
	assert.True(t, utf8.ValidString(params["filename"]))
}

func TestTerminalAttachmentsRouteUsesLayerVisibleSessionRoot(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	const label = "layer-terminal"
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    label,
		TmuxSession:           "tmux-layer-terminal",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Status:                "working",
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	privateRoot := tclcommon.SpawnAttachmentsPrivateDir(label)
	require.NoError(t, os.MkdirAll(privateRoot, 0o700))

	r := newSpawnAttachUpload(t, []uploadPart{{
		filename: "pasted-image.png",
		data:     []byte("png"),
	}})
	r.URL.Path = "/api/terminal-attachments"
	r.URL.RawQuery = "terminal=" + url.QueryEscape("/api/spawn-focus-ws/"+label)
	w := httptest.NewRecorder()
	base, createBase, status, err := terminalAttachmentBase(
		"/api/spawn-focus-ws/"+label,
		func(tmux string) bool { return tmux == "tmux-layer-terminal" },
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, privateRoot, base)
	assert.False(t, createBase)

	// Exercise the HTTP storage seam with the same resolved base while avoiding
	// a real tmux subprocess in this focused unit test.
	storeDashboardAttachments(w, r, base, createBase)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var response spawnAttachmentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Files, 1)
	assert.Equal(t, privateRoot, filepath.Dir(response.Dir))
	assert.True(t, strings.HasPrefix(response.Files[0].Path, privateRoot+string(filepath.Separator)))
}

func TestTerminalAttachmentBaseRejectsStaleUnknownAndHostileTargets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    "unknown-layer",
		TmuxSession:           "tmux-unknown",
		SandboxImplementation: "future-sandbox",
		Status:                "working",
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          "legacy-builtin",
		TmuxSession: "tmux-builtin",
		Status:      "working",
		CreatedAt:   now,
		UpdatedAt:   now,
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    "sandbox-off",
		TmuxSession:           "tmux-off",
		SandboxImplementation: string(sandboxpolicy.ImplementationOff),
		Status:                "working",
		CreatedAt:             now,
		UpdatedAt:             now,
	}))

	base, createBase, status, err := terminalAttachmentBase(
		"/api/spawn-focus-ws/legacy-builtin",
		func(string) bool { return true },
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, spawnAttachmentsBaseDir(), base)
	assert.True(t, createBase)

	base, createBase, status, err = terminalAttachmentBase(
		"/api/spawn-focus-ws/sandbox-off",
		func(string) bool { return true },
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, spawnAttachmentsBaseDir(), base)
	assert.True(t, createBase)

	_, _, status, err = terminalAttachmentBase(
		"/api/spawn-focus-ws/unknown-layer",
		func(string) bool { return true },
	)
	require.ErrorContains(t, err, "unknown sandbox implementation")
	assert.Equal(t, http.StatusConflict, status)

	_, _, status, err = terminalAttachmentBase(
		"/api/spawn-focus-ws/unknown-layer",
		func(string) bool { return false },
	)
	require.ErrorContains(t, err, "not live")
	assert.Equal(t, http.StatusNotFound, status)

	for _, hostile := range []string{
		"",
		"https://example.test/api/spawn-focus-ws/unknown-layer",
		"/api/other-terminal/unknown-layer",
		"/api/spawn-focus-ws/unknown-layer/extra",
	} {
		_, _, status, err = terminalAttachmentBase(hostile, func(string) bool { return true })
		require.Error(t, err, hostile)
		assert.Equal(t, http.StatusBadRequest, status, hostile)
	}
}
