package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardStartupLayoutCurtain(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	html := read("dashboard.html")
	css := read("dashboard.css")
	boot := read("js/dashboard.js")
	poll := read("js/snapshot-poll.js")

	for _, check := range []struct {
		source, needle, why string
	}{
		{html, `class="hide-processes dashboard-booting"`, "the parser must curtain the shell before its first paint"},
		{css, `body.dashboard-booting > *`, "booting boxes must stay in layout but out of paint"},
		{css, `animation: dashboard-boot-failsafe 0s 8s forwards`, "a JS fault must not leave the dashboard permanently hidden"},
		{css, `animation: dashboard-boot-label-failsafe 0s 8s forwards`, "the CSS failsafe must retire its loading label"},
		{css, `pointer-events: none`, "the visual loading label must never block recovery interactions"},
		{boot, `const bootFinished = waitForInitialSnapshot(firstSnapshot, bootTimedOut);`, "the first snapshot wait must be bounded while the poll retries"},
		{boot, `await bootFinished;`, "URL restoration must wait on that bound"},
		{boot, `await settleInitialLayout();`, "deferred dock/nav geometry must settle before reveal"},
		{boot, `document.body.classList.remove('dashboard-booting')`, "successful bootstrap must reveal the dashboard"},
		{boot, `startSnapshotPoll(refresh, {`, "the scheduler must own the recurring cadence"},
		// Without immediate:true there is no bootstrap attempt at all — the first
		// refresh would be the t=2s tick and the curtain would sit up for it.
		{boot, `immediate: true,`, "the poll must take the bootstrap attempt at once, not at the first tick"},
		{boot, `bootOptions: { includeLists: false },`, "the curtain must not wait on the Groups tab's paginated lists"},
		// Tied to the curtain's own bound, so a FAILED first attempt does not put
		// the heavy lists back in front of the retries behind it.
		{boot, `bootUntil: bootFinished,`, "boot narrowing must span the whole curtain, not just the first attempt"},
		{poll, `if (immediate) run();`, "the poller must own the bootstrap attempt so its in-flight guard covers it"},
		{poll, `refresh(booting ? bootOptions : undefined)`, "every attempt taken under the curtain must carry the boot narrowing"},
	} {
		if !strings.Contains(check.source, check.needle) {
			t.Errorf("dashboard startup CLS guard missing %q: %s", check.needle, check.why)
		}
	}

	boundAt := strings.Index(boot, "const bootFinished = waitForInitialSnapshot(firstSnapshot, bootTimedOut);")
	refreshAt := strings.Index(boot, "await bootFinished;")
	navAt := strings.Index(boot, "initNavHistory();")
	revealAt := strings.Index(boot, "document.body.classList.remove('dashboard-booting')")
	pollAt := strings.Index(boot, "startSnapshotPoll(refresh, {")
	if boundAt < 0 || pollAt < boundAt || refreshAt < pollAt || navAt < refreshAt || revealAt < navAt {
		t.Errorf("startup order must be curtain bound -> recurring poll -> bounded initial refresh -> URL restore -> reveal (indexes %d, %d, %d, %d, %d)", boundAt, pollAt, refreshAt, navAt, revealAt)
	}
}
