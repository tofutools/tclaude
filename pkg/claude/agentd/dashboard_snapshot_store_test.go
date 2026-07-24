package agentd

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardSnapshotStoreBoundary(t *testing.T) {
	store, err := fs.ReadFile(dashboardAssetsFS, "js/snapshot-store.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "morphInto", "/api/", "fetch("} {
		if strings.Contains(string(store), forbidden) {
			t.Errorf("snapshot store contains forbidden rendering/API knowledge %q", forbidden)
		}
	}

	refresh, err := fs.ReadFile(dashboardAssetsFS, "js/refresh.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"dashboardState.beginRequest()",
		"dashboardState.isCurrentRequest(requestId)",
		"dashboardState.commitRequest(requestId, data)",
		"dashboardState.failRequest(requestId, e, { responded })",
		"dashboardState.discardRequest(requestId, { responded })",
	} {
		if !strings.Contains(string(refresh), needle) {
			t.Errorf("authoritative poll is missing Signals bridge %q", needle)
		}
	}

	// A refresh whose generation is superseded bails without publishing, so two
	// overlapping refreshes are not redundant work — the newer one CANCELS the
	// older. Two invariants keep a cycle slower than the poll interval from
	// starving every attempt (the boot curtain stuck on "Fetching data…", and a
	// steady-state dashboard that silently stops updating): the scheduler refuses
	// to overlap, and a refresh that IS superseded stops costing agentd a full
	// snapshot build it will never read.
	poll, err := fs.ReadFile(dashboardAssetsFS, "js/snapshot-poll.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"if (inFlightSince !== null && nowImpl() - inFlightSince < stallMs) return;",
		"const done = () => { if (inFlightSince === startedAt) inFlightSince = null; };",
	} {
		if !strings.Contains(string(poll), needle) {
			t.Errorf("snapshot poll is missing its in-flight guard %q", needle)
		}
	}
	for _, needle := range []string{
		"inFlightFetches?.abort(abortReason('superseded by a newer refresh'));",
		"signal: abort.signal",
		"if (inFlightFetches === abort) inFlightFetches = null;",
	} {
		if !strings.Contains(string(refresh), needle) {
			t.Errorf("authoritative poll is missing supersede-abort wiring %q", needle)
		}
	}
	// The stall bound is what keeps the in-flight guard from becoming a permanent
	// wedge, so the refresh timeout that guarantees settlement must stay under it.
	if !strings.Contains(string(poll), "SNAPSHOT_STALL_MS = 30000") ||
		!strings.Contains(string(refresh), "SNAPSHOT_REQUEST_TIMEOUT_MS = 20000") {
		t.Error("the refresh timeout must stay bounded below the poll's stall escape hatch")
	}
}

func TestDashboardHasOneAuthoritativeSnapshotPoll(t *testing.T) {
	var authoritativeSnapshotFetches, schedulerCalls, manualDebounces int
	scheduledRefresh := regexp.MustCompile(`(?s)set(?:Interval|Timeout)\s*\([^;]{0,300}\brefresh\b`)
	snapshotFetch := regexp.MustCompile(`fetch\s*\(\s*['"\x60]/api/snapshot(?:\?[^'"\x60]*)?['"\x60]`)
	schedulerCall := regexp.MustCompile(`(?m)^[\t ]*(?:(?:(?:const|let|var)\s+)?[\w$.]+\s*=\s*)?(?:[\w$.]+\.push\()?startSnapshotPoll\s*\(\s*refresh\b`)
	for _, syntax := range []string{
		"fetch('/api/snapshot')", `fetch("/api/snapshot")`, "fetch(`/api/snapshot`)",
		"fetch('/api/snapshot?poll=1')", `fetch("/api/snapshot?poll=1")`, "fetch(`/api/snapshot?poll=1`)",
	} {
		if !snapshotFetch.MatchString(syntax) {
			t.Fatalf("direct snapshot fetch detector misses %q", syntax)
		}
	}
	for _, syntax := range []string{
		"startSnapshotPoll(refresh)",
		"const stop = startSnapshotPoll(refresh, { immediate: false })",
		"pageCleanups.push(startSnapshotPoll(refresh, { immediate: false }))",
	} {
		if !schedulerCall.MatchString(syntax) {
			t.Fatalf("snapshot scheduler call detector misses %q", syntax)
		}
	}
	if schedulerCall.MatchString("export function startSnapshotPoll(refresh, options) {}") {
		t.Fatal("snapshot scheduler call detector mistakes the function declaration for an installation")
	}
	err := fs.WalkDir(dashboardAssetsFS, "js", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".js") {
			return nil
		}
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			return err
		}
		source := string(data)
		directSnapshotFetches := len(snapshotFetch.FindAllStringIndex(source, -1))
		schedulerCalls += len(schedulerCall.FindAllStringIndex(source, -1))
		switch name {
		case "js/refresh.js":
			authoritativeSnapshotFetches += directSnapshotFetches
			// The legacy Groups filter deliberately debounces a one-shot manual
			// refresh. Remove it before looking for a periodic/recursive scheduler.
			manualDebounces += strings.Count(source, "setTimeout(refresh, 250)")
			source = strings.ReplaceAll(source, "setTimeout(refresh, 250)", "")
		case "js/jobs-island.js":
			// The Preact Jobs query has the same one-shot debounce, routed through
			// the action boundary. It does not repeat or fetch snapshot directly.
			const jobsDebounce = "setTimeout(() => void actions.refresh(), 250)"
			manualDebounces += strings.Count(source, jobsDebounce)
			source = strings.ReplaceAll(source, jobsDebounce, "")
		case "js/groups-island.js":
			// Groups owns the same one-shot server-filter debounce through the
			// dashboard action boundary; it never schedules the recurring poll.
			const groupsDebounce = "setTimeout(() => void actions.refresh(), 250)"
			manualDebounces += strings.Count(source, groupsDebounce)
			source = strings.ReplaceAll(source, groupsDebounce, "")
		default:
			if directSnapshotFetches != 0 {
				t.Errorf("%s owns %d direct /api/snapshot fetches; use the shared store/actions", name, directSnapshotFetches)
			}
		}
		if name != "js/snapshot-poll.js" && scheduledRefresh.MatchString(source) {
			t.Errorf("%s schedules refresh outside the authoritative snapshot-poll module", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if authoritativeSnapshotFetches != 1 {
		t.Errorf("authoritative refresh /api/snapshot fetch count = %d, want exactly one", authoritativeSnapshotFetches)
	}
	if schedulerCalls != 1 {
		t.Errorf("snapshot scheduler installation count = %d, want exactly one", schedulerCalls)
	}
	if manualDebounces != 2 {
		t.Errorf("known one-shot filter refresh debounce count = %d, want 2", manualDebounces)
	}

	dashboard, err := fs.ReadFile(dashboardAssetsFS, "js/dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dashboard), "configureDashboardActions({ refresh })") {
		t.Error("dashboard does not connect future island actions to the authoritative refresh")
	}
}
