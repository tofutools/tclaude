package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_MailPrevGensToggleWired guards the Messages-tab "show
// previous generations" sidebar toggle: predecessor (replaced) conv
// generations of an actor get an agent folder because their message history
// survives, and the toggle hides those folders from the agent listing — flat
// AND nested — until the operator ticks it. It's a pure client-side roster
// filter (mail.js reads the snapshot's replaced[] list, the same source the
// Groups tab uses), so the firehose is untouched.
//
// The pieces live across three files coupled only by these literal strings, so
// a rename in one silently breaks the feature in the browser — asserting the
// whole chain at `go test ./...` catches it.
func TestDashboardAssets_MailPrevGensToggleWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the footer checkbox.
		`id="mail-show-prev-gens"`,
		// JS: the predecessor-conv set is derived from the snapshot's
		// replaced[], the toggle handler exists, and the sidebar filter applies
		// it to the agent listing.
		"function prevGenConvSet(",
		"snap.replaced",
		"function setShowPrevGens(",
		"mail.showPrevGens",
		"const prevGens = prevGenConvSet();",
		// CSS: the dim rule for a predecessor folder row.
		".mailbox-row.prev-gen .mailbox",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Messages-tab prev-gens toggle wiring broken", needle)
		}
	}
}
