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
	if !strings.Contains(src, "confirmModal({") {
		t.Error("modal-term.js no longer calls confirmModal — the close/disconnect " +
			"confirmations are gone")
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
