package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_MailPrevGensToggleWired guards the Messages-tab "show
// previous generations" sidebar toggle: predecessor (replaced) conv
// generations of an actor get an agent folder because their message history
// survives, and the toggle hides those folders from the agent listing — flat
// AND nested — until the operator ticks it. The roster is server-paged, so
// the filter runs server-side (hide_prev_gens on /api/mailboxes, which flags
// shown predecessors prev_gen); the firehose is untouched.
//
// The pieces live across three files coupled only by these literal strings, so
// a rename in one silently breaks the feature in the browser — asserting the
// whole chain at `go test ./...` catches it.
func TestDashboardAssets_MailPrevGensToggleWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the footer checkbox (a shared Messages part, given this
		// node's ID prefix).
		"`${idPrefix}-show-prev-gens`",
		`<${MailFooterToggles} idPrefix="mail"`,
		// JS: the toggle handler exists, the roster fetch asks the server to
		// hide predecessors while it is off, and the sidebar tags the ones
		// shown from the server's prev_gen flag.
		"function setShowPrevGens(",
		"mail.showPrevGens",
		"params.set('hide_prev_gens', '1')",
		"function prevGenConvSet(",
		"filter(mb => mb.prev_gen)",
		// CSS: the dim rule for a predecessor folder row.
		".mailbox-row.prev-gen .mailbox",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Messages-tab prev-gens toggle wiring broken", needle)
		}
	}
}
