package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The in-browser terminal modal (js/modal-term.js) must not tear itself
// down or silently retry behind the user's back. There is no JS test
// runner, so these structural guards pin the two behaviours against the
// embedded module source — they assert invariants, not user-facing copy,
// so rewording the dialogs won't churn them.

// TestTermModal_NoSilentReconnect guards that a dropped connection prompts
// the human (promptReconnect → confirmModal) instead of looping a quiet
// setTimeout(connect, …) retry that hides a session that has actually ended.
func TestTermModal_NoSilentReconnect(t *testing.T) {
	src := readTermModalSrc(t)

	if strings.Contains(src, "setTimeout(connect") {
		t.Error("modal-term.js auto-reconnects via setTimeout(connect, …) — a drop must " +
			"prompt the human (promptReconnect), not silently retry")
	}
	for _, needle := range []string{
		"import { confirmModal } from './refresh.js'", // the prompt primitive
		"promptReconnect",                             // the disconnect handler
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("modal-term.js missing %q — disconnect prompt wiring broken", needle)
		}
	}
}

// TestTermModal_BackdropDoesNotCloseDirectly guards that an outside (backdrop)
// click asks for confirmation rather than tearing the terminal down — clicking
// beside the modal while reaching for the terminal is too easy to do by
// accident. The old direct-close handler must be gone.
func TestTermModal_BackdropDoesNotCloseDirectly(t *testing.T) {
	src := readTermModalSrc(t)

	if strings.Contains(src, "if (e.target === overlay) closeTermModal()") {
		t.Error("modal-term.js closes on a backdrop click without confirming — " +
			"an outside click must ask first, not tear the terminal down")
	}
	// Pin the backdrop handler specifically (not just "confirmModal is called
	// somewhere", which promptReconnect alone would satisfy): the click
	// handler must early-return on a non-backdrop target and route the actual
	// close through confirmModal.
	if !strings.Contains(src, "if (e.target !== overlay) return;") {
		t.Error("modal-term.js backdrop handler must early-return on a non-backdrop " +
			"target before deciding to close")
	}
	if !strings.Contains(src, "okLabel: 'Close terminal'") {
		t.Error("modal-term.js backdrop close must go through a confirmModal " +
			"offering 'Close terminal' — the outside click must ask first")
	}
}

// TestTermModal_DetachVsClose pins the two-button split: the Detach button is
// the instant, no-confirm path (binds straight to detachAndClose), while the ×
// Close button asks first (routes through confirmAndClose). Both keep the
// underlying agent session alive — the only difference is the confirmation
// gate, so these guard the wiring, not the copy.
func TestTermModal_DetachVsClose(t *testing.T) {
	src := readTermModalSrc(t)

	// Detach = instant detach+close, no confirm.
	if !strings.Contains(src, "$('#term-session-detach').addEventListener('click', detachAndClose)") {
		t.Error("modal-term.js Detach button must bind directly to detachAndClose " +
			"(instant, no confirm)")
	}
	// × Close must confirm first — it must NOT bind directly to the close/detach
	// path.
	if strings.Contains(src, "$('#term-session-close').addEventListener('click', closeTermModal)") ||
		strings.Contains(src, "$('#term-session-close').addEventListener('click', detachAndClose)") {
		t.Error("modal-term.js × Close must confirm first (confirmAndClose), not bind " +
			"directly to a close/detach handler")
	}
	if !strings.Contains(src, "$('#term-session-close').addEventListener('click', confirmAndClose)") {
		t.Error("modal-term.js × Close must route through confirmAndClose (ask first)")
	}
}

// TestTermModal_DetachCallsHideAPI pins the actual fix: the detach path issues
// the server-side detach (POST /api/hide/{conv}) — the same reliable mechanism
// the per-agent "hide" eye button uses — rather than only closing the
// WebSocket (which did not reliably detach the open-window tmux client).
func TestTermModal_DetachCallsHideAPI(t *testing.T) {
	src := readTermModalSrc(t)
	if !strings.Contains(src, "/api/hide/") {
		t.Error("modal-term.js detach path must POST /api/hide/{conv} — closing the " +
			"WebSocket alone did not reliably detach the open-window tmux client")
	}
}

// TestTermModal_DetachButtonInMarkup pins the Detach button into the modal
// header (dashboard.html) so the JS binding above has an element to attach to.
func TestTermModal_DetachButtonInMarkup(t *testing.T) {
	data, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	if !strings.Contains(string(data), `id="term-session-detach"`) {
		t.Error(`dashboard.html missing id="term-session-detach" — the Detach affordance ` +
			"has no element for bindTermModal to wire")
	}
}

func readTermModalSrc(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(dashboardAssetsFS, "js/modal-term.js")
	if err != nil {
		t.Fatalf("read js/modal-term.js: %v", err)
	}
	return string(data)
}
