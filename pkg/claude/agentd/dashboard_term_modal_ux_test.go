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
	// Route the actual close through the ask-first confirm (confirmAndClose), not
	// a direct teardown. The exact copy (detach vs close) varies by view type and
	// is pinned in TestTermModal_BackdropDetachVsCloseByViewType.
	if !strings.Contains(src, "confirmAndClose();") {
		t.Error("modal-term.js backdrop handler must route the close through " +
			"confirmAndClose() (ask first), not tear the terminal down directly")
	}
}

// TestTermModal_BackdropDetachVsCloseByViewType pins the copy split keyed on
// hideConv. A web window (the live-agent "open window" attach, hideConv set) is
// a view onto the agent's real tmux session: clicking outside it must read as a
// DETACH — it only drops the tmux client (via /api/hide) while the agent keeps
// running — not as a shutdown. The ad hoc web terminal (hideConv null, its own
// throwaway shell) keeps asking to CLOSE, exactly as before. Both gestures still
// route through the same confirmAndClose → detachAndClose; only the wording and
// the server-side hide differ.
func TestTermModal_BackdropDetachVsCloseByViewType(t *testing.T) {
	src := readTermModalSrc(t)

	// The confirm copy is chosen off hideConv (set = web window, null = web term).
	start := strings.Index(src, "confirmModal(hideConv ?")
	if start < 0 {
		t.Fatal("modal-term.js confirmAndClose must pick its copy off hideConv — a web " +
			"window detaches (agent keeps running) while an ad hoc web terminal closes")
	}
	// Pin the branch→copy MAPPING, not just that both labels appear somewhere: the
	// hideConv-truthy branch (before the `} : {` separator) must carry 'Detach',
	// the else branch (after it) 'Close terminal'. A transposition that swapped the
	// two branches' copy would still pass a "both strings present" check, so order
	// is the real invariant.
	rest := src[start:]
	detachIdx := strings.Index(rest, "okLabel: 'Detach'")
	elseIdx := strings.Index(rest, "} : {")
	closeIdx := strings.Index(rest, "okLabel: 'Close terminal'")
	ordered := detachIdx >= 0 && elseIdx >= 0 && closeIdx >= 0 &&
		detachIdx < elseIdx && elseIdx < closeIdx
	if !ordered {
		t.Error("modal-term.js confirm copy is mis-mapped: the hideConv-truthy branch must " +
			"offer 'Detach' (web window) and the else branch 'Close terminal' (ad hoc web terminal)")
	}
}

// TestTermModal_SpawnAutoFocusDetaches pins that the spawn auto-focus in-browser
// fallback (modal-spawn.js) is treated as a web window, not a throwaway terminal.
// It attaches to the agent's LIVE tmux session (handleDashboardSpawnFocusWS →
// openAttachCmd, the same attach the open-window row action uses), so closing it
// must run the reliable server-side detach (/api/hide) and its confirm must ask
// to DETACH — exactly like the open-window caller. That requires passing hideConv
// to openTermModal; without it the modal both shows the wrong "Close terminal?"
// copy and skips the detach, leaving the forked tmux client stuck attached.
func TestTermModal_SpawnAutoFocusDetaches(t *testing.T) {
	data, err := fs.ReadFile(dashboardAssetsFS, "js/modal-spawn.js")
	if err != nil {
		t.Fatalf("read js/modal-spawn.js: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "payload.focus_ws") {
		t.Skip("spawn auto-focus in-browser fallback not present — nothing to pin")
	}
	if !strings.Contains(src, "hideConv: payload.conv_id") {
		t.Error("modal-spawn.js spawn auto-focus opens a live-session web window — it must " +
			"pass hideConv (payload.conv_id) so closing it detaches via /api/hide and asks to " +
			"detach, not just drop the WebSocket")
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
	// Gated on hideConv: a null hideConv (ad hoc web-term) must NOT fire a hide.
	// /api/hide resolves to the agent's MAIN session, so hiding from a throwaway
	// terminal would detach the agent's real window.
	if !strings.Contains(src, "if (hideConv) {") {
		t.Error("modal-term.js detach must be gated on `if (hideConv)` so a null " +
			"hideConv (web-term) never POSTs /api/hide")
	}
	// Order: close (which nulls ws.onclose) BEFORE the awaited hide. /api/hide
	// drops this window's own client → agentd closes the WS; if onclose were
	// still armed it would fire a spurious "Terminal disconnected" reconnect
	// prompt over the just-closed modal. Pin closeTermModal() immediately ahead
	// of the hide block.
	if !strings.Contains(src, "closeTermModal();\n  if (hideConv) {") {
		t.Error("modal-term.js detachAndClose must call closeTermModal() BEFORE the " +
			"awaited /api/hide — otherwise the server-side WS close races a spurious " +
			"reconnect prompt onto the screen")
	}
}

// TestTermModal_OnlyOpenWindowPassesHideConv pins the load-bearing wiring and
// guards a footgun WITHIN row-actions.js. The open-window row action must thread
// the agent selector through as hideConv so the modal's detach hits /api/hide for
// the agent's live session. Crucially, of row-actions.js's openTermModal callers
// EXACTLY ONE may pass hideConv: the web-term / term-dir callers attach to a
// THROWAWAY tclaude-term-… session, but /api/hide resolves to the agent's MAIN
// spwn-… session — so passing hideConv there would detach the agent's real window
// when its throwaway terminal closes. The count invariant fails the build if a
// future edit copy-pastes hideConv onto one of those callers.
//
// hideConv is legitimate from OTHER files too when the view attaches to the
// agent's main session — the spawn auto-focus fallback in modal-spawn.js is the
// other such caller (see TestTermModal_SpawnAutoFocusDetaches). This count is
// scoped to row-actions.js precisely so those legitimate callers don't relax it.
func TestTermModal_OnlyOpenWindowPassesHideConv(t *testing.T) {
	src := readRowActionsSrc(t)
	if !strings.Contains(src, "hideConv: agent") {
		t.Error("row-actions.js open-window caller must pass `hideConv: agent` to " +
			"openTermModal — without it the web window's Detach/Close can't hit /api/hide")
	}
	if n := strings.Count(src, "hideConv:"); n != 1 {
		t.Errorf("exactly one row-actions.js openTermModal caller may pass hideConv (the "+
			"open-window action); found %d — a web-term/term-dir caller passing hideConv would "+
			"detach the agent's MAIN session when its throwaway terminal closes", n)
	}
}

func readRowActionsSrc(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(dashboardAssetsFS, "js/row-actions.js")
	if err != nil {
		t.Fatalf("read js/row-actions.js: %v", err)
	}
	return string(data)
}

// TestTermModal_DetachButtonOnlyForWebWindow pins that the Detach button is
// shown only for a web WINDOW (a view onto the agent's live session, hideConv
// set) and hidden for an ad hoc web TERMINAL (hideConv null) — a throwaway
// shell has nothing to detach from, so it gets only the × Close. The toggle is
// keyed off hideConv in openTermModal.
func TestTermModal_DetachButtonOnlyForWebWindow(t *testing.T) {
	src := readTermModalSrc(t)
	if !strings.Contains(src, "$('#term-session-detach').style.display = hideConv ?") {
		t.Error("modal-term.js must toggle the Detach button on hideConv (shown for a web " +
			"window, hidden for an ad hoc web terminal) — `$('#term-session-detach')." +
			"style.display = hideConv ? …`")
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
