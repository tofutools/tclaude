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
		{boot, `await Promise.race([refresh(), firstSnapshot, bootTimedOut])`, "the first snapshot wait must be bounded while the poll retries"},
		{boot, `await settleInitialLayout();`, "deferred dock/nav geometry must settle before reveal"},
		{boot, `document.body.classList.remove('dashboard-booting')`, "successful bootstrap must reveal the dashboard"},
		{boot, `startSnapshotPoll(refresh, { immediate: false })`, "the scheduler must retry without duplicating the explicit first refresh"},
		{poll, `if (immediate) void refresh();`, "the poller must support the post-bootstrap scheduling mode"},
	} {
		if !strings.Contains(check.source, check.needle) {
			t.Errorf("dashboard startup CLS guard missing %q: %s", check.needle, check.why)
		}
	}

	refreshAt := strings.Index(boot, "await Promise.race([refresh(), firstSnapshot, bootTimedOut])")
	revealAt := strings.Index(boot, "document.body.classList.remove('dashboard-booting')")
	pollAt := strings.Index(boot, "startSnapshotPoll(refresh, { immediate: false })")
	if pollAt < 0 || refreshAt < pollAt || revealAt < refreshAt {
		t.Errorf("startup order must be recurring poll -> bounded initial refresh -> reveal (indexes %d, %d, %d)", pollAt, refreshAt, revealAt)
	}
}
