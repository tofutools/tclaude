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
}

func TestDashboardHasOneAuthoritativeSnapshotPoll(t *testing.T) {
	var authoritativeSnapshotFetches, modalReplacementFetches, schedulerCalls, manualDebounces int
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
		case "js/modal-human-reply.js":
			// This legacy data-only timer runs only while its modal suspends the
			// authoritative renderer. Keep the exception explicit until that
			// modal becomes an island and can subscribe to the shared store.
			modalReplacementFetches += directSnapshotFetches
			if strings.Count(source, "setInterval(pollReplyOnline, 2000)") != 1 {
				t.Errorf("%s no longer has exactly one known modal replacement timer", name)
			}
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
	if modalReplacementFetches != 1 {
		t.Errorf("legacy modal replacement /api/snapshot fetch count = %d, want exactly one", modalReplacementFetches)
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
