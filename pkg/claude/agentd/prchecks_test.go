package agentd

import (
	"encoding/json"
	"testing"
	"time"
)

// The rollup is the one place a GitHub vocabulary meets a human-facing
// badge, and the mapping is opinionated (NEUTRAL passes, CANCELLED fails,
// SKIPPED is its own bucket). These tests pin that mapping, because a
// silent drift in it turns a red PR green on the dashboard.

func TestParseStatusCheckRollupBuckets(t *testing.T) {
	raw := json.RawMessage(`[
		{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS",
		 "workflowName":"CI","detailsUrl":"https://github.com/o/r/runs/1",
		 "startedAt":"2026-08-09T10:00:00Z","completedAt":"2026-08-09T10:03:12Z"},
		{"__typename":"CheckRun","name":"lint","status":"IN_PROGRESS","conclusion":"",
		 "workflowName":"CI","startedAt":"2026-08-09T10:00:00Z"},
		{"__typename":"CheckRun","name":"flaky","status":"COMPLETED","conclusion":"CANCELLED"},
		{"__typename":"CheckRun","name":"neutral-job","status":"COMPLETED","conclusion":"NEUTRAL"},
		{"__typename":"CheckRun","name":"docs-only","status":"COMPLETED","conclusion":"SKIPPED"},
		{"__typename":"StatusContext","context":"ci/legacy","state":"FAILURE",
		 "targetUrl":"https://ci.example/1","description":"build broke",
		 "createdAt":"2026-08-09T09:59:00Z"},
		{"__typename":"CheckRun","name":"","status":"COMPLETED","conclusion":"SUCCESS"}
	]`)
	now := time.Date(2026, 8, 9, 10, 5, 0, 0, time.UTC)
	info := parseStatusCheckRollup(raw, now)

	if got, want := len(info.Checks), 6; got != want {
		t.Fatalf("checks = %d, want %d (the nameless node must be dropped)", got, want)
	}
	buckets := map[string]string{}
	for _, c := range info.Checks {
		buckets[c.Name] = c.Bucket
	}
	for name, want := range map[string]string{
		"test":        "pass",
		"lint":        "pending",
		"flaky":       "fail", // a cancelled run is not a green light
		"neutral-job": "pass", // NEUTRAL is explicitly "not a failure"
		"docs-only":   "skipped",
		"ci/legacy":   "fail",
	} {
		if buckets[name] != want {
			t.Errorf("%s bucket = %q, want %q", name, buckets[name], want)
		}
	}

	s := info.Summary
	if s.Total != 6 || s.Passed != 2 || s.Failed != 2 || s.Pending != 1 || s.Skipped != 1 {
		t.Errorf("summary = %+v, want total 6 / passed 2 / failed 2 / pending 1 / skipped 1", s)
	}
	if s.State != "failing" {
		t.Errorf("state = %q, want failing (a failure outranks a pending run)", s.State)
	}
	if s.FetchedAt != now.Format(time.RFC3339) {
		t.Errorf("fetched_at = %q, want %q", s.FetchedAt, now.Format(time.RFC3339))
	}
}

func TestParseStatusCheckRollupCarriesDetail(t *testing.T) {
	raw := json.RawMessage(`[
		{"__typename":"CheckRun","name":"build / linux","status":"COMPLETED","conclusion":"TIMED_OUT",
		 "workflowName":"CI","detailsUrl":"https://github.com/o/r/runs/9",
		 "startedAt":"2026-08-09T10:00:00Z","completedAt":"2026-08-09T10:09:00Z"},
		{"__typename":"StatusContext","context":"coderabbit","state":"PENDING",
		 "targetUrl":"https://coderabbit/1","createdAt":"2026-08-09T10:01:00Z"}
	]`)
	info := parseStatusCheckRollup(raw, time.Now())

	run := info.Checks[0]
	if run.Conclusion != "timed out" || run.Source != "CI" || run.URL == "" {
		t.Errorf("check run detail = %+v, want a spelled-out conclusion, workflow and link", run)
	}
	if run.StartedAt == "" || run.CompletedAt == "" {
		t.Errorf("timestamps must ride the wire so the panel can render elapsed time: %+v", run)
	}
	// A StatusContext has no startedAt; its creation time is the only anchor
	// the panel can count from.
	ctx := info.Checks[1]
	if ctx.Bucket != "pending" || ctx.StartedAt != "2026-08-09T10:01:00Z" {
		t.Errorf("status context = %+v, want pending anchored at createdAt", ctx)
	}
}

func TestParseStatusCheckRollupEmptyIsResolved(t *testing.T) {
	info := parseStatusCheckRollup(json.RawMessage(`[]`), time.Now())
	if info.Checks == nil {
		t.Error("an empty rollup must parse to a non-nil, zero-length list")
	}
	if info.Summary.State != "none" || info.Summary.Total != 0 {
		t.Errorf("summary = %+v, want state none / total 0", info.Summary)
	}
	// A malformed payload must not panic or invent checks.
	if bad := parseStatusCheckRollup(json.RawMessage(`{"nope":1}`), time.Now()); bad.Summary.Total != 0 {
		t.Errorf("malformed rollup = %+v, want an empty summary", bad.Summary)
	}
}

// A check's details link is attacker-influenced: a commit status's target_url
// is set by whoever posted it, and the dashboard renders it as a live href in
// an origin whose cookie authorizes every mutating /api route. Anything that
// isn't http(s) must be dropped before it can reach the panel.
func TestParseStatusCheckRollupDropsHostileURLs(t *testing.T) {
	raw := json.RawMessage(`[
		{"__typename":"StatusContext","context":"evil","state":"SUCCESS",
		 "targetUrl":"javascript:fetch('/api/agents/x',{method:'DELETE'})"},
		{"__typename":"CheckRun","name":"evil-run","status":"COMPLETED","conclusion":"SUCCESS",
		 "detailsUrl":"data:text/html,<script>1</script>"},
		{"__typename":"CheckRun","name":"good","status":"COMPLETED","conclusion":"SUCCESS",
		 "detailsUrl":"https://github.com/o/r/runs/1"}
	]`)
	info := parseStatusCheckRollup(raw, time.Now())
	require := func(name, got, want string) {
		if got != want {
			t.Errorf("%s URL = %q, want %q", name, got, want)
		}
	}
	require("javascript status context", info.Checks[0].URL, "")
	require("data: check run", info.Checks[1].URL, "")
	require("ordinary check run", info.Checks[2].URL, "https://github.com/o/r/runs/1")
	// The rows themselves survive — only their links are dropped.
	if len(info.Checks) != 3 {
		t.Errorf("checks = %d, want all three rows kept", len(info.Checks))
	}
}

// Clicking the badge should land on the build that explains it, so the run
// URL is derived from a job URL and chosen by what the badge is currently
// reporting.
func TestGithubRunSummaryURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/o/r/actions/runs/123/job/456":      "https://github.com/o/r/actions/runs/123",
		"https://github.com/o/r/actions/runs/123/job/456?pr=7": "https://github.com/o/r/actions/runs/123",
		"https://github.com/o/r/actions/runs/123":              "https://github.com/o/r/actions/runs/123",
		"https://github.com/o/r/actions/runs/123/":             "https://github.com/o/r/actions/runs/123",
		"https://ci.example.com/build/9":                       "", // external app: no run page to guess
		"https://github.com/o/r/actions/runs/notanumber/job/1": "",
		"javascript:alert(1)/actions/runs/1":                   "", // hostile schemes never survive
		"https://github.com/o/r/pull/1/checks":                 "",
		// The badge href is the most prominent control in the row and
		// detailsUrl is set by the reporting app, so a lookalike host must
		// never survive — matching on the path substring alone would have
		// pointed a red pill at any of these.
		"https://evil.example/o/r/actions/runs/123/job/456":  "",
		"https://github.com.evil.example/o/r/actions/runs/1": "",
		"https://github.com@evil.example/o/r/actions/runs/1": "",
		"https://evil.example/x#/o/r/actions/runs/1":         "",
		"https://github.com/o/r/actions/runs":                "", // no run id
		"https://github.com/o/bad__repo!/actions/runs/1":     "",
		"HTTPS://GitHub.com/o/r/actions/runs/123/job/4":      "https://github.com/o/r/actions/runs/123",
	} {
		if got := githubRunSummaryURL(raw); got != want {
			t.Errorf("githubRunSummaryURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLeadingRunURLFollowsBadgeState(t *testing.T) {
	const (
		passRun    = "https://github.com/o/r/actions/runs/1"
		failRun    = "https://github.com/o/r/actions/runs/2"
		pendingRun = "https://github.com/o/r/actions/runs/3"
	)
	checks := []prCheckRun{
		{Bucket: "pass", RunURL: passRun, StartedAt: "2026-08-09T10:00:00Z"},
		{Bucket: "pending", RunURL: pendingRun, StartedAt: "2026-08-09T10:01:00Z"},
		{Bucket: "fail", RunURL: failRun, StartedAt: "2026-08-09T10:02:00Z"},
	}
	// Red badge -> the failing build, whatever else is going on.
	if got := leadingRunURL(checks, "failing"); got != failRun {
		t.Errorf("failing badge links to %q, want the failing run %q", got, failRun)
	}
	// Amber badge -> the run still going.
	if got := leadingRunURL(checks[:2], "pending"); got != pendingRun {
		t.Errorf("pending badge links to %q, want the running one %q", got, pendingRun)
	}
	// Green badge -> the most recent finished run.
	green := []prCheckRun{
		{Bucket: "pass", RunURL: passRun, StartedAt: "2026-08-09T10:00:00Z"},
		{Bucket: "pass", RunURL: failRun, StartedAt: "2026-08-09T10:05:00Z"},
	}
	if got := leadingRunURL(green, "passing"); got != failRun {
		t.Errorf("passing badge links to %q, want the newest run %q", got, failRun)
	}
	// Nothing to link: the frontend falls back to the PR's checks page.
	if got := leadingRunURL([]prCheckRun{{Bucket: "pass"}}, "passing"); got != "" {
		t.Errorf("leadingRunURL with no run URLs = %q, want empty", got)
	}
	// A red badge must never open a build that is merely pending: when the
	// failing check is a non-Actions status with no run page, the honest
	// answer is "nothing to link", and the badge falls back to the PR's
	// checks tab — which does list that status.
	mixed := []prCheckRun{
		{Bucket: "fail"}, // e.g. a commit status from an external CI app
		{Bucket: "pending", RunURL: pendingRun},
	}
	if got := leadingRunURL(mixed, "failing"); got != "" {
		t.Errorf("failing badge with no failing run URL = %q, want empty (not the pending run)", got)
	}
}

func TestSummarizePRChecksStatePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []prCheckRun
		want   string
	}{
		{"all green", []prCheckRun{{Bucket: "pass"}, {Bucket: "pass"}}, "passing"},
		{"a pending run", []prCheckRun{{Bucket: "pass"}, {Bucket: "pending"}}, "pending"},
		{"a failure outranks pending", []prCheckRun{{Bucket: "fail"}, {Bucket: "pending"}}, "failing"},
		{"only skips still passes", []prCheckRun{{Bucket: "skipped"}}, "passing"},
		{"nothing at all", nil, "none"},
	} {
		if got := summarizePRChecks(tc.checks, time.Time{}).State; got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every badge for one PR must read the same cache entry, however the PR
// reached the dashboard — branch link, startup link, or presented URL with
// a /files suffix.
func TestPRChecksCacheKeyIsPRIdentity(t *testing.T) {
	base := "https://github.com/tofutools/tclaude/pull/2151"
	for _, variant := range []string{
		base,
		base + "/files",
		"https://github.com/TofuTools/TClaude/pull/2151",
	} {
		if prChecksCacheKey(variant) != prChecksCacheKey(base) {
			t.Errorf("%s must share a checks cache key with %s", variant, base)
		}
	}
	if prChecksCacheKey("https://github.com/tofutools/tclaude/pull/2152") == prChecksCacheKey(base) {
		t.Error("different PRs must not share a checks cache key")
	}
	if key := prChecksCacheKey(base); key[:4] != "prc_" {
		t.Errorf("cache key %q must carry the prc_ namespace", key)
	}
}

func TestWithPRChecksStampsEveryBadge(t *testing.T) {
	branch := "https://github.com/o/r/pull/1"
	presented := "https://github.com/o/r/pull/2/files"
	idx := map[string]*prChecksSummary{
		prStateKey(branch):    {Total: 3, Passed: 3, State: "passing"},
		prStateKey(presented): {Total: 2, Failed: 1, Passed: 1, State: "failing"},
	}
	v := repoLinksView{
		BranchPRURL:  branch,
		StartupPRURL: branch, // the common case: agent never left its branch
		PresentedPRs: []presentedPRView{{URL: presented}, {URL: "https://github.com/o/r/pull/3"}},
	}.withPRChecks(idx)

	if v.BranchChecks == nil || v.BranchChecks.State != "passing" {
		t.Errorf("branch badge summary = %+v, want the passing entry", v.BranchChecks)
	}
	if v.StartupChecks == nil || v.StartupChecks.State != "passing" {
		t.Errorf("startup badge summary = %+v, want the same entry as the branch badge", v.StartupChecks)
	}
	if v.PresentedPRs[0].Checks == nil || v.PresentedPRs[0].Checks.State != "failing" {
		t.Errorf("presented badge summary = %+v, want the failing entry despite the /files suffix", v.PresentedPRs[0].Checks)
	}
	if v.PresentedPRs[1].Checks != nil {
		t.Error("a PR with no cached checks must stay unstamped rather than showing a zeroed badge")
	}
}
