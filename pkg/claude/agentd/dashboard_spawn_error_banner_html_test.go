package agentd

import (
	"strings"
	"testing"
)

// The spawn / clone / reincarnate dialogs surface input + server errors on a
// shared inline line (.cron-create-error). It used to be a small red hairline
// at the bottom of a tall, scrollable modal — easy to miss, especially when a
// Ctrl/Cmd+Enter submit fired while that line sat below the scroll fold. This
// guards the uplift: the line renders as a collapse-when-empty banner, and the
// spawn-family submit paths route through showModalError (which scrolls it into
// view + flashes it) rather than a bare textContent write.
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

	// The shared helper: sets the text, scrolls it into view, restarts the
	// flash. scrollIntoView is the fix for the below-the-fold hotkey submit.
	must("function showModalError(",
		"shared modal-error helper present")
	must("el.scrollIntoView({ block: 'nearest' });",
		"helper scrolls the error into view")
	must("el.classList.add('flash');",
		"helper (re)triggers the flash")

	// Dismiss button: the banner carries a ✕ that clears it on click, styled
	// to sit at the top-right (the .dismissible flex variant).
	must("x.addEventListener('click', () => showModalError(el, ''));",
		"✕ dismiss button clears the banner")
	must("cron-create-error-x",
		"dismiss button element + style present")
	must(".cron-create-error.dismissible {",
		"flex variant places the ✕ at the banner's right")

	// Every spawn-family submit error path goes through the helper. Three
	// dialogs each surface a non-OK fetch body the same way, so the
	// `showModalError(errEl, (await r.text())` shape must appear 3×.
	if n := strings.Count(dashboardAssets, "showModalError(errEl, (await r.text())"); n != 3 {
		t.Errorf("want 3 spawn/clone/reincarnate fetch-error sites routed through showModalError; got %d", n)
	}
	// And none of the spawn-family error lines should be set with a bare
	// textContent write any more — that was exactly the easy-to-miss path.
	for _, id := range []string{"agent-spawn-error", "clone-agent-error", "reincarnate-agent-error"} {
		if strings.Contains(dashboardAssets, "'#"+id+"').textContent =") {
			t.Errorf("#%s still set via bare textContent — route it through showModalError", id)
		}
	}
}
