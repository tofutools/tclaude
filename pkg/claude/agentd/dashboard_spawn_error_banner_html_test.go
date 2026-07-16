package agentd

import (
	"strings"
	"testing"
)

// The spawn / clone / reincarnate dialogs surface input + server errors on a
// shared inline line (.cron-create-error). It used to be a small red hairline
// at the bottom of a tall, scrollable modal — easy to miss, especially when a
// Ctrl/Cmd+Enter submit fired while that line sat below the scroll fold. This
// guards the uplift: the migrated spawn/clone/reincarnate paths render a
// collapse-when-empty controlled banner.
func TestDashboardHTML_SpawnErrorBannerWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// CSS: the shared error line is a banner, collapsed when empty, with a
	// re-triggerable attention flash.
	must(".cron-create-error:empty { display: none; }",
		"empty error line collapses (no reserved hairline)")
	must("@keyframes cron-error-flash",
		"attention-flash animation defined")
	must(".cron-create-error.flash {",
		"flash class drives the animation")

	// The controlled banner's dismiss button sits at the top-right.
	must("cron-create-error-x",
		"dismiss button element + style present")
	must(".cron-create-error.dismissible {",
		"flex variant places the ✕ at the banner's right")

	// Spawn and the action dialogs use controlled error state with the same
	// scroll/flash/dismiss contract.
	must(`<${ErrorBanner} error=${error} onDismiss=${() => setError('')} />`, "Preact spawn dialog renders its controlled banner")
	must("setError(errorMessage(cause))", "spawn request failures publish through controlled state")
	must("function ErrorBanner({ id, error, onDismiss })", "Preact action dialogs share a controlled error banner")
	must("element.scrollIntoView({ block: 'nearest' });", "Preact error banner scrolls into view")
	must("<${ErrorBanner} id=\"clone-agent-error\"", "clone dialog uses the Preact banner")
	must("<${ErrorBanner} id=\"reincarnate-agent-error\"", "reincarnate dialog uses the Preact banner")
	// And none of the spawn-family error lines should be set with a bare
	// textContent write any more — that was exactly the easy-to-miss path.
	for _, id := range []string{"agent-spawn-error", "clone-agent-error", "reincarnate-agent-error"} {
		if strings.Contains(dashboardAssets, "'#"+id+"').textContent =") {
			t.Errorf("#%s still set via bare textContent — route it through controlled Preact error state", id)
		}
	}
}
