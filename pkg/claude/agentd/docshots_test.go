package agentd_test

// docshots_test.go is the seeded server behind scripts/docs/screenshots.sh,
// which regenerates the dashboard images in README.md and docs/. It holds no
// browser code: the test seeds invented data into the isolated HOME newFlow
// sets up, serves the real dashboard mux and waits. The driver in
// scripts/e2e/drive/docshots captures the pages; it is a nested module so go-rod
// stays out of the main go.mod. Gated so `go test ./...` never serves anything.
//
// The script compiles this binary with a build overlay that freezes the wall
// clock of time.Now at the manifest's frozenTime, so the seeds below stamp
// time.Now() and every age, quota window and cost day in the daemon comes out the
// same on every run.

import (
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	docRepo   = "https://github.com/acme/webshop"
	learnRepo = "https://github.com/you/my-project"
	// docHome is the fictional operator home every seeded path lives under, so
	// images show tidy paths instead of the test's temp dir.
	docHome      = "/home/dev"
	learnProject = docHome + "/code/my-project"
)

// docAgent is one seeded fleet member.
type docAgent struct {
	conv, title, role, harness, model, effort string
	status, detail                            string // "" leaves the session "running"
	online, owner                             bool
	cost                                      float64
	// startBranch is the launch branch; nowBranch, when set, is the worktree
	// branch the agent moved to; the dashboard stacks the two as INIT/NOW.
	startBranch, nowBranch string
	taskURL, taskLabel     string
	descr                  string
	tags                   []string
	age, last              time.Duration
}

// docPR describes the fake PR the git-info resolver reports for a branch.
type docPR struct {
	number int
	state  string
	checks string // gh statusCheckRollup JSON
}

type learnAgent struct {
	conv, title, model, status, detail string
	branch                             string // "" means main in learnProject
	age, last                          time.Duration
}

// TestDocShotsServe seeds the fixture named by TCLAUDE_DOCSHOTS_FIXTURE, writes
// the server's URL to <TCLAUDE_DOCSHOTS_SERVE>/url and serves until a file named
// stop appears there.
func TestDocShotsServe(t *testing.T) {
	dir := os.Getenv("TCLAUDE_DOCSHOTS_SERVE")
	if dir == "" {
		t.Skip("docs screenshot server: run scripts/docs/screenshots.sh")
	}
	if before := time.Now(); func() bool { time.Sleep(20 * time.Millisecond); return time.Now().Round(0).Equal(before.Round(0)) }() == false {
		t.Fatal("the wall clock is not frozen: build through scripts/docs/screenshots.sh")
	}
	f := newFlow(t)
	var (
		repo string
		prs  map[string]docPR
	)
	switch fixture := os.Getenv("TCLAUDE_DOCSHOTS_FIXTURE"); fixture {
	case "fleet":
		repo = docRepo
		prs = map[string]docPR{
			"checkout-redesign": {1482, "open", docChecks(14, 0)},
			"search-index-v2":   {1477, "open", docChecks(9, 3)},
			"search-reranker":   {1491, "draft", docChecks(2, 0)},
			"gemini-onboarding": {1488, "open", docChecks(12, 0)},
			"payments-retry":    {1469, "open", docChecks(11, 1, "fail")},
		}
	case "learn":
		repo = learnRepo
		prs = map[string]docPR{
			"readme-typos": {12, "open", docChecks(6, 0)},
			"more-tests":   {13, "open", docChecks(4, 2)},
		}
	default:
		t.Fatalf("unknown TCLAUDE_DOCSHOTS_FIXTURE %q", fixture)
	}
	t.Cleanup(agentd.SetGitInfoResolverWithChecksForTest(
		func(_, branch string) (string, string, int, string, string, string, bool) {
			pr, ok := prs[branch]
			if !ok {
				return repo, "main", 0, "", "", "", true
			}
			return repo, "main", pr.number, repo + "/pull/" + itoa(pr.number), pr.state, pr.checks, true
		}))
	if repo == docRepo {
		seedDocFleet(t, f)
	} else {
		seedLearnTeam(t, f)
	}

	mux := agentd.BuildDashboardHandlerForTest()
	// Warm the branch-link and CI caches before the browser's first poll so
	// every badge is drawn on the first frame. The in-process fetches need the
	// popup base URL set; it is cleared again before the browser connects.
	restorePopup := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restorePopup)
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
	_ = fetchDashSnapshot(t, mux)
	restorePopup()

	srv := httptest.NewServer(mux)
	defer srv.Close()
	tmp := filepath.Join(dir, "url.tmp")
	if err := os.WriteFile(tmp, []byte(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "url")); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func seedDocFleet(t *testing.T, f *testharness.Flow) {
	t.Helper()

	// Collapsed neighbours, so the tree reads like a real installation.
	for _, g := range []string{"data-pipeline", "infra"} {
		f.HaveGroup(g)
	}
	main := f.HaveGroup("webshop")
	if _, err := db.SetAgentGroupDefaultCwd("webshop", docHome+"/git/webshop"); err != nil {
		t.Fatalf("group cwd: %v", err)
	}
	for _, sub := range []string{"webshop-release-2.4", "webshop-perf"} {
		g := f.HaveGroup(sub)
		if _, err := db.SetAgentGroupParent(g.ID, "webshop"); err != nil {
			t.Fatalf("nest %s: %v", sub, err)
		}
	}

	agents := []docAgent{
		{conv: "d0c00000-0000-4000-8000-000000000001", title: "lead", descr: "Plans the sprint, reviews and merges", role: "lead", owner: true,
			harness: "claude", model: "Opus 5.5", effort: "high", status: "idle", online: true, cost: 41.20,
			startBranch: "main", taskURL: "https://linear.app/acme/issue/SHOP-212", taskLabel: "SHOP-212",
			age: 26 * time.Hour, last: 4 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000002", title: "checkout-ui", taskURL: "https://linear.app/acme/issue/SHOP-198", taskLabel: "SHOP-198", descr: "Checkout page redesign", role: "dev",
			harness: "claude", model: "Sonnet 5.5", effort: "medium", status: "working", detail: "Edit", online: true, cost: 6.85,
			startBranch: "main", nowBranch: "checkout-redesign", age: 3 * time.Hour, last: 8 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000003", title: "search-indexer", taskURL: "https://linear.app/acme/issue/SHOP-205", taskLabel: "SHOP-205", descr: "Search relevance v2", role: "dev",
			harness: "codex", model: "gpt-6.1-sol", effort: "high", status: "working", detail: "Bash", online: true, cost: 12.40,
			startBranch: "search-index-v2", nowBranch: "search-reranker", age: 5 * time.Hour, last: 3 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000004", title: "onboarding-copy", taskURL: "https://linear.app/acme/issue/SHOP-219", taskLabel: "SHOP-219", descr: "Onboarding copy refresh", role: "dev",
			harness: "gemini", model: "gemini-3-pro", status: "idle", online: true, cost: 2.10,
			startBranch: "gemini-onboarding", age: 90 * time.Minute, last: 12 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000005", title: "payments-fix", taskURL: "https://linear.app/acme/issue/SHOP-207", taskLabel: "SHOP-207", descr: "Retry failed card payments", role: "dev",
			harness: "copilot", model: "gpt-6-luna", effort: "xhigh", status: "awaiting_permission", online: true, cost: 4.75,
			startBranch: "payments-retry", age: 2 * time.Hour, last: 45 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000006", title: "cold-reviewer", descr: "Fresh-eyes PR review", role: "reviewer",
			harness: "opencode", model: "openai/gpt-5.6-sol", effort: "medium", status: "idle", online: true, cost: 1.30,
			startBranch: "main", tags: []string{"review"}, age: 40 * time.Minute, last: 20 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000007", title: "perf-audit", taskURL: "https://linear.app/acme/issue/SHOP-176", taskLabel: "SHOP-176", descr: "Lighthouse budget", role: "dev",
			harness: "claude", model: "Opus 5.5", effort: "medium", online: false, cost: 9.60,
			startBranch: "lighthouse-budget", age: 30 * time.Hour, last: 26 * time.Hour},
	}
	for _, a := range agents {
		seedDocAgent(t, f, main.ID, "webshop", a)
	}
	// A subscription account shows per-agent what-if cost (≈$) only when the
	// operator opts in; do so, as most fleet operators running the dashboard do.
	if err := config.Save(&config.Config{
		Cost:       &config.CostConfig{ShowOnSubscription: true},
		Federation: &config.FederationConfig{Name: "dev@workstation"},
	}); err != nil {
		t.Fatalf("cost config: %v", err)
	}
	seedDocUsage(t)
	clearLogin, err := agentd.SeedAuthoredOpenPRsForTest("acme-dev", docRepo, []agentd.AuthoredOpenPRForTest{
		{Number: 1491, Title: "Rerank search results with click signals", Draft: true, Passed: 2},
		{Number: 1488, Title: "Refresh onboarding copy", Passed: 12},
		{Number: 1482, Title: "Checkout page redesign", Passed: 14},
		{Number: 1477, Title: "Search index v2", Passed: 9, Pending: 3},
		{Number: 1469, Title: "Retry failed card payments", Passed: 11, Failed: 1},
	})
	if err != nil {
		t.Fatalf("seed open PRs: %v", err)
	}
	t.Cleanup(clearLogin)
	seedDocUsageHistory(t) // the Usage tab only appears once history exists
	seedDocProfiles(t)
}

func seedDocAgent(t *testing.T, f *testharness.Flow, groupID int64, group string, a docAgent) {
	t.Helper()
	label := "lbl-" + a.title
	tmux := "tmux-" + a.title
	launch := docHome + "/git/webshop"
	if a.startBranch != "main" {
		launch = docHome + "/git/webshop-" + a.startBranch
	}
	f.HaveAliveSessionOnBranch(a.conv, label, tmux, launch, a.startBranch)
	// Title the conversation without clobbering the branch the .jsonl scan
	// just indexed (HaveConvWithTitle upserts a bare row).
	conv := agent.FreshConvRowResolved(a.conv)
	if conv == nil {
		t.Fatalf("conv_index scan for %s", a.title)
	}
	conv.CustomTitle = a.title
	if err := db.UpsertConvIndex(conv); err != nil {
		t.Fatalf("title %s: %v", a.title, err)
	}
	f.HaveMemberWithRole(group, a.conv, a.role)
	if a.nowBranch != "" {
		wt := docHome + "/git/webshop-" + a.nowBranch
		if err := db.UpsertAgentWorkdir(a.conv, wt+"/src", wt, a.nowBranch); err != nil {
			t.Fatalf("workdir %s: %v", a.title, err)
		}
	}

	row, err := db.LoadSession(label)
	if err != nil || row == nil {
		t.Fatalf("load session %s: %v", a.title, err)
	}
	row.Harness = a.harness
	if a.status != "" {
		row.Status = a.status
		row.StatusDetail = a.detail
	}
	row.CreatedAt = time.Now().Add(-a.age)
	row.LastHook = time.Now().Add(-a.last)
	if err := db.SaveSession(row); err != nil {
		t.Fatalf("save session %s: %v", a.title, err)
	}
	if !a.online {
		f.MarkOffline(tmux)
	}
	if err := db.UpdateSessionModel(label, a.model); err != nil {
		t.Fatalf("model %s: %v", a.title, err)
	}
	if a.effort != "" {
		if err := db.UpdateSessionEffort(label, a.effort); err != nil {
			t.Fatalf("effort %s: %v", a.title, err)
		}
	}
	if err := db.UpdateSessionVirtualCost(label, a.cost); err != nil {
		t.Fatalf("cost %s: %v", a.title, err)
	}
	if a.descr != "" {
		if _, err := db.UpdateAgentGroupMember(groupID, a.conv, nil, &a.descr); err != nil {
			t.Fatalf("descr %s: %v", a.title, err)
		}
	}
	if a.owner {
		if err := db.AddAgentGroupOwner(groupID, a.conv, "docshots"); err != nil {
			t.Fatalf("owner %s: %v", a.title, err)
		}
	}
	agentID, err := db.AgentIDForConv(a.conv)
	if err != nil || agentID == "" {
		t.Fatalf("no actor for %s: %v", a.title, err)
	}
	if len(a.tags) > 0 {
		if err := db.ReplaceAgentTags(agentID, a.tags); err != nil {
			t.Fatalf("tags %s: %v", a.title, err)
		}
	}
	if a.taskURL != "" {
		if _, err := db.SetAgentTaskRef(agentID, a.taskURL, a.taskLabel); err != nil {
			t.Fatalf("task %s: %v", a.title, err)
		}
	}
	// Age reads the actor's birth time; backdate it so the column varies.
	d, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE agents SET created_at = ? WHERE agent_id = ?`,
		time.Now().Add(-a.age).UnixNano(), agentID); err != nil {
		t.Fatalf("age %s: %v", a.title, err)
	}
}

// seedDocUsage fills the header's subscription quota bars for Claude and Codex.
func seedDocUsage(t *testing.T) {
	t.Helper()
	now := time.Now()
	blob, err := json.Marshal(usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 34, ResetsAt: now.Add(2*time.Hour + 41*time.Minute)},
		SevenDay:      &usageapi.CachedBucket{Pct: 58, ResetsAt: now.Add(3*24*time.Hour + 5*time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveUsageCache(blob, now, now); err != nil {
		t.Fatalf("usage cache: %v", err)
	}
	cx, err := json.Marshal(harness.CodexUsage{
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 12, ResetsAt: now.Add(4*time.Hour + 10*time.Minute)},
		Weekly:   &harness.CodexRateLimitWindow{UsedPercent: 27, ResetsAt: now.Add(5*24*time.Hour + 2*time.Hour)},
		Observed: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveCodexUsageCacheIfNewer(cx, now, "docshots"); err != nil {
		t.Fatalf("codex usage cache: %v", err)
	}
}

// seedDocProfiles fills the palette dock with one spawn profile per harness.
func seedDocProfiles(t *testing.T) {
	t.Helper()
	for _, p := range []db.SpawnProfile{
		{Name: "opus-high", Descr: "Claude Code lead", Model: "claude-opus-5-5", Effort: "high", AutoCompactWindow: "450000"},
		{Name: "sonnet-dev", Descr: "Claude Code implementer", Model: "claude-sonnet-5-5", Effort: "medium"},
		{Name: "sol-codex", Descr: "Codex implementer", Harness: "codex", Model: "gpt-6.1-sol", Effort: "high", Sandbox: "tclaude-agent"},
		{Name: "gemini-pro", Descr: "Gemini CLI (beta)", Harness: "gemini", Model: "pro"},
		{Name: "luna-copilot", Descr: "Copilot CLI", Harness: "copilot", Model: "gpt-6-luna", Effort: "xhigh"},
		{Name: "sol-opencode", Descr: "OpenCode reviewer", Harness: "opencode", Model: "openai/gpt-5.6-sol", Effort: "medium"},
	} {
		if _, err := db.CreateSpawnProfile(&p); err != nil {
			t.Fatalf("profile %s: %v", p.Name, err)
		}
	}
	for _, r := range []db.Role{
		{Name: "lead", Descr: "Coordinates the group", Brief: "You lead the group."},
		{Name: "dev", Descr: "Implements features", Brief: "You implement features."},
		{Name: "reviewer", Descr: "Cold-reviews diffs", Brief: "You review diffs cold."},
	} {
		if _, err := db.CreateRole(&r); err != nil && !errors.Is(err, db.ErrRoleNameTaken) {
			t.Fatalf("role %s: %v", r.Name, err)
		}
	}
}

// docChecks builds a gh statusCheckRollup with passed successes and pending
// in-progress runs, plus one run with the optional extra conclusion.
func docChecks(passed, pending int, extra ...string) string {
	var runs []map[string]string
	for i := range passed {
		runs = append(runs, map[string]string{"__typename": "CheckRun", "name": "check-" + itoa(i), "status": "COMPLETED", "conclusion": "SUCCESS"})
	}
	for i := range pending {
		runs = append(runs, map[string]string{"__typename": "CheckRun", "name": "pending-" + itoa(i), "status": "IN_PROGRESS"})
	}
	for _, c := range extra {
		if c == "fail" {
			runs = append(runs, map[string]string{"__typename": "CheckRun", "name": "e2e", "status": "COMPLETED", "conclusion": "FAILURE"})
		}
	}
	b, _ := json.Marshal(runs)
	return string(b)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func seedDocUsageHistory(t *testing.T) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour).Truncate(15 * time.Minute)
	claudeFiveHour := []float64{72, 18, 25, 31}
	claudeWeekly := []float64{40, 42, 45, 48}
	codexWeekly := []float64{55, 59, 62, 66}
	for i := range claudeFiveHour {
		observedAt := base.Add(time.Duration(i) * 15 * time.Minute)
		if _, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderAnthropic, ObservedAt: observedAt, Source: "docshots",
			Windows: []db.SubscriptionUsageWindow{
				{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: claudeFiveHour[i], ResetsAt: base.Add(5 * time.Hour)},
				{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: claudeWeekly[i], ResetsAt: base.Add(6 * 24 * time.Hour)},
			},
		}); err != nil {
			t.Fatalf("seed Claude usage history: %v", err)
		}
		if _, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderOpenAI, ObservedAt: observedAt, Source: "docshots",
			Windows: []db.SubscriptionUsageWindow{
				{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: codexWeekly[i], ResetsAt: base.Add(5 * 24 * time.Hour)},
			},
		}); err != nil {
			t.Fatalf("seed Codex usage history: %v", err)
		}
	}
}

// seedLearnTeam builds the end state of "Run a team through a lead": a group
// "hello" with its startup context, alice leading workers bob and carol, their
// mail and pull requests, alice's notification to the operator, and one plain
// session that is not in any group.
func seedLearnTeam(t *testing.T, f *testharness.Flow) {
	t.Helper()
	// The header's node chip defaults to the real user@host; pin a fake one.
	if err := config.Save(&config.Config{Federation: &config.FederationConfig{Name: "dev@workstation"}}); err != nil {
		t.Fatalf("node name: %v", err)
	}
	g := f.HaveGroup("hello")
	if _, err := db.SetAgentGroupDefaultCwd("hello", learnProject); err != nil {
		t.Fatalf("group cwd: %v", err)
	}
	if _, err := db.SetAgentGroupDefaultContext("hello", learnGroupContext); err != nil {
		t.Fatalf("group context: %v", err)
	}
	// alice leads (she owns the group); bob and carol are the workers she
	// spawned, each on a worktree branch with an open pull request.
	const alice = "1ea40000-0000-4000-8000-00000000a11c"
	const bob = "1ea40000-0000-4000-8000-000000000b0b"
	const carol = "1ea40000-0000-4000-8000-0000000ca201"
	for _, a := range []learnAgent{
		{conv: alice, title: "alice", model: "Sonnet 5.5", status: "working", detail: "Read", age: 26 * time.Minute, last: 2 * time.Second},
		{conv: bob, title: "bob", model: "Sonnet 5.5", status: "idle", branch: "readme-typos", age: 9 * time.Minute, last: 40 * time.Second},
		{conv: carol, title: "carol", model: "Sonnet 5.5", status: "working", detail: "Bash", branch: "more-tests", age: 9 * time.Minute, last: 5 * time.Second},
	} {
		seedLearnSession(t, f, a)
		f.HaveMemberWithRole("hello", a.conv, "")
		backdateAgent(t, a.conv, a.age)
	}
	if err := db.AddAgentGroupOwner(g.ID, alice, "docshots"); err != nil {
		t.Fatalf("owner alice: %v", err)
	}

	base := time.Now().Add(-8 * time.Minute)
	for i, m := range []db.AgentMessage{
		{GroupID: g.ID, FromConv: alice, ToConv: bob, Subject: "Fix the README typos",
			Body: "Hi bob! Please fix the typos in README.md on your branch and open a pull request. Reply with its link when it is up."},
		{GroupID: g.ID, FromConv: alice, ToConv: carol, Subject: "Add tests for the config loader",
			Body: "Hi carol! Please add unit tests for the config loader on your branch and open a pull request. Reply with its link when it is up."},
		{GroupID: g.ID, FromConv: bob, ToConv: alice, Subject: "Re: Fix the README typos",
			Body: "Done: fixed four typos in README.md. The pull request is #12, and CI is green."},
	} {
		m.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := db.InsertAgentMessage(&m); err != nil {
			t.Fatalf("message: %v", err)
		}
	}
	seedLearnNotification(t, f, alice)

	// A bare `tclaude -d` while the daemon runs: enrolled, in no group.
	plain := learnAgent{conv: "1ea40000-0000-4000-8000-0000000501a0", title: "20261012-1017-1ea40501", model: "Sonnet 5.5", status: "idle", age: time.Minute, last: 30 * time.Second}
	seedLearnSession(t, f, plain)
	f.HaveEnrolledAgent(plain.conv)

	// keep-agents-going: bob after "Run a team through a lead" retired him (a
	// separate conversation from the live bob above), and a conversation
	// started from the shell that never became an agent.
	retired := "1ea40000-0000-4000-8000-0000000b0b01"
	seedLearnOfflineConv(t, f, retired, "bob")
	f.HaveEnrolledAgent(retired)
	if _, err := db.RetireAgent(retired, "operator", ""); err != nil {
		t.Fatalf("retire bob: %v", err)
	}
	seedLearnOfflineConv(t, f, "1ea40000-0000-4000-8000-0000000c0a01", "fix the flaky login test")
}

// learnGroupContext is the hello group's startup context in team-setup.
const learnGroupContext = `This is my-project, a small CLI written in Go.
Work on your own branch and open a pull request when a change is ready.
Run go test ./... before you push.`

// seedLearnNotification delivers alice's notify-human message to the
// operator, with a small chart attached as an image.
func seedLearnNotification(t *testing.T, f *testharness.Flow, from string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 480, 270))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{0xf6, 0xf7, 0xf9, 0xff}}, image.Point{}, draw.Src)
	bar := func(x, h int, c color.RGBA) {
		draw.Draw(img, image.Rect(x, 240-h, x+70, 240), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	// Test count before and after carol's change, per package.
	for i, h := range [][2]int{{60, 150}, {90, 120}, {40, 170}} {
		bar(50+i*140, h[0], color.RGBA{0xa0, 0xa8, 0xb8, 0xff})
		bar(50+i*140+40, h[1], color.RGBA{0x3b, 0x82, 0xf6, 0xff})
	}
	draw.Draw(img, image.Rect(30, 240, 450, 242), &image.Uniform{color.RGBA{0x55, 0x5b, 0x66, 0xff}}, image.Point{}, draw.Src)
	path := filepath.Join(f.World.HomeDir, "test-counts.png")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertHumanMessageWithAttachments(&db.HumanMessage{
		FromConv: from, FromTitle: "alice", GroupName: "hello",
		Subject: "Both PRs are up",
		Body: "bob fixed the README typos (#12, CI green) and carol added tests for the config loader (#13, CI running).\n\n" +
			"The chart shows the test count per package before and after. Shall I retire both workers once #13 is green?",
		CreatedAt: time.Now().Add(-90 * time.Second),
	}, []*db.HumanMessageAttachment{{
		Filename: "test-counts.png", ContentType: "image/png", SizeBytes: info.Size(), StoragePath: path,
	}}); err != nil {
		t.Fatalf("notification: %v", err)
	}
}

// seedLearnOfflineConv writes a conversation transcript with no live session.
func seedLearnOfflineConv(t *testing.T, f *testharness.Flow, conv, title string) {
	t.Helper()
	cc := testharness.NewCCSimWithID(t, f.World.HomeDir, conv, learnProject)
	if err := cc.Start(); err != nil {
		t.Fatalf("transcript %s: %v", title, err)
	}
	row := agent.FreshConvRowAt(conv, learnProject)
	if row == nil {
		t.Fatalf("conv_index scan for %s", title)
	}
	row.CustomTitle = title
	if err := db.UpsertConvIndex(row); err != nil {
		t.Fatalf("title %s: %v", title, err)
	}
}

func seedLearnSession(t *testing.T, f *testharness.Flow, a learnAgent) {
	t.Helper()
	label := "lbl-" + a.title
	cwd, branch := learnProject, "main"
	if a.branch != "" {
		cwd, branch = learnProject+"-"+a.branch, a.branch
	}
	f.HaveAliveSessionOnBranch(a.conv, label, "tmux-"+a.title, cwd, branch)
	conv := agent.FreshConvRowResolved(a.conv)
	if conv == nil {
		t.Fatalf("conv_index scan for %s", a.title)
	}
	conv.CustomTitle = a.title
	if err := db.UpsertConvIndex(conv); err != nil {
		t.Fatalf("title %s: %v", a.title, err)
	}
	row, err := db.LoadSession(label)
	if err != nil || row == nil {
		t.Fatalf("load session %s: %v", a.title, err)
	}
	row.Harness = "claude"
	row.Status = a.status
	row.StatusDetail = a.detail
	row.CreatedAt = time.Now().Add(-a.age)
	row.LastHook = time.Now().Add(-a.last)
	if err := db.SaveSession(row); err != nil {
		t.Fatalf("save session %s: %v", a.title, err)
	}
	if err := db.UpdateSessionModel(label, a.model); err != nil {
		t.Fatalf("model %s: %v", a.title, err)
	}
}

func backdateAgent(t *testing.T, conv string, age time.Duration) {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		t.Fatalf("no actor for %s: %v", conv, err)
	}
	d, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE agents SET created_at = ? WHERE agent_id = ?`,
		time.Now().Add(-age).UnixNano(), agentID); err != nil {
		t.Fatalf("age %s: %v", conv, err)
	}
}
