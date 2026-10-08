package agentd_test

// docshots_test.go renders the screenshots embedded in README.md and docs/.
// It reuses the dashsnap capture driver, but seeds its own fleet: the dashsnap
// fixture is shaped for visual regression (awkward names, edge-case rows),
// while these images are meant to show a realistic mixed-harness team at work.
// Like TestDashSnap it is compiled by `go test ./...` and gated behind an env
// var so CI never launches a browser:
//
//	TCLAUDE_DOCSHOTS=1 go test ./pkg/claude/agentd/ -run TestDocShots -v -count=1 -timeout 600s
//
// Output lands in dashsnap-out/docshots-<timestamp>/ (gitignored); copy the
// PNGs you want into docs/assets/.

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	docRepo = "https://github.com/acme/webshop"
	// docHome is the fictional operator home the fleet's paths live under, so
	// screenshots show tidy paths instead of the test's temp dir.
	docHome = "/home/dev"
)

// docAgent is one seeded fleet member.
type docAgent struct {
	conv, title, role, harness, model, effort string
	status, detail                            string // "" leaves the session "running"
	online, owner                             bool
	cost                                      float64
	// startBranch is the launch branch; nowBranch, when set, is the worktree
	// branch the agent moved to — the dashboard stacks the two as INIT/NOW.
	startBranch, nowBranch string
	taskURL, taskLabel     string
	tags                   []string
	age, last              time.Duration
}

// docPR describes the fake PR the git-info resolver reports for a branch.
type docPR struct {
	number int
	state  string
	checks string // gh statusCheckRollup JSON
}

func TestDocShots(t *testing.T) {
	if os.Getenv("TCLAUDE_DOCSHOTS") == "" {
		t.Skip("docs screenshot generator — set TCLAUDE_DOCSHOTS=1 to run (needs a local headless Chrome)")
	}

	prs := map[string]docPR{
		"checkout-redesign": {1482, "open", docChecks(14, 0)},
		"search-index-v2":   {1477, "open", docChecks(9, 3)},
		"search-reranker":   {1491, "draft", docChecks(2, 0)},
		"gemini-onboarding": {1488, "open", docChecks(12, 0)},
		"payments-retry":    {1469, "open", docChecks(11, 1, "fail")},
	}
	t.Cleanup(agentd.SetGitInfoResolverWithChecksForTest(
		func(_, branch string) (string, string, int, string, string, string, bool) {
			pr, ok := prs[branch]
			if !ok {
				return docRepo, "main", 0, "", "", "", true
			}
			return docRepo, "main", pr.number, docRepo + "/pull/" + itoa(pr.number), pr.state, pr.checks, true
		}))

	f := newFlow(t)
	seedDocFleet(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	// Warm the branch-link and CI caches before the browser's first poll so
	// every badge is drawn on the first frame. The in-process fetches need the
	// popup base URL set; it is cleared again before the browser connects.
	restorePopup := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restorePopup) // in case a fetch below fails the test
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
	_ = fetchDashSnapshot(t, mux)
	restorePopup()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	const showGroups = `document.querySelector('nav [data-tab="groups"]').click();`
	const expandMain = `document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){
  d.open = d.getAttribute('data-group-key') === 'webshop';
});`
	states := []dashsnap.State{{
		Key:     "dashboard-groups",
		Title:   "Groups — mixed-harness fleet",
		Caption: "README / docs hero image.",
		JS:      showGroups + expandMain + `document.body.classList.add('dock-open');`,
		Width:   1760, Height: 1000,
		SettleMS: 400,
	}}

	outDir := filepath.Join(dashSnapOutRoot(t), "docshots-"+time.Now().Format("20060102-150405"))
	shots, err := dashsnap.Capture(dashsnap.Config{BaseURL: srv.URL, OutDir: outDir, States: states})
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	for _, s := range shots {
		if s.Err != "" {
			t.Errorf("%s: %s", s.State.Key, s.Err)
			continue
		}
		t.Logf("wrote %s", filepath.Join(outDir, s.File))
	}
}

func seedDocFleet(t *testing.T, f *testharness.Flow) {
	t.Helper()

	// Collapsed neighbours, so the tree reads like a real installation.
	for _, g := range []string{"data-pipeline", "infra", "docs-site", "mobile-app"} {
		f.HaveGroup(g)
	}
	main := f.HaveGroup("webshop")
	if _, err := db.SetAgentGroupDefaultCwd("webshop", docHome+"/git/webshop"); err != nil {
		t.Fatalf("group cwd: %v", err)
	}
	for _, sub := range []string{"webshop-release-2.4", "webshop-perf", "webshop-a11y"} {
		g := f.HaveGroup(sub)
		if _, err := db.SetAgentGroupParent(g.ID, "webshop"); err != nil {
			t.Fatalf("nest %s: %v", sub, err)
		}
	}

	agents := []docAgent{
		{conv: "d0c00000-0000-4000-8000-000000000001", title: "lead", role: "lead", owner: true,
			harness: "claude", model: "Opus 5.5", effort: "high", status: "idle", online: true, cost: 41.20,
			startBranch: "main", taskURL: "https://linear.app/acme/issue/SHOP-212", taskLabel: "SHOP-212",
			age: 26 * time.Hour, last: 4 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000002", title: "checkout-ui", role: "dev",
			harness: "claude", model: "Sonnet 5.5", effort: "medium", status: "working", detail: "Edit", online: true, cost: 6.85,
			startBranch: "main", nowBranch: "checkout-redesign", age: 3 * time.Hour, last: 8 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000003", title: "search-indexer", role: "dev",
			harness: "codex", model: "gpt-6.1-sol", effort: "high", status: "working", detail: "Bash", online: true, cost: 12.40,
			startBranch: "search-index-v2", nowBranch: "search-reranker", age: 5 * time.Hour, last: 3 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000004", title: "onboarding-copy", role: "dev",
			harness: "gemini", model: "gemini-3-pro", status: "idle", online: true, cost: 2.10,
			startBranch: "gemini-onboarding", age: 90 * time.Minute, last: 12 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000005", title: "payments-fix", role: "dev",
			harness: "copilot", model: "gpt-6-luna", effort: "xhigh", status: "awaiting_permission", online: true, cost: 4.75,
			startBranch: "payments-retry", age: 2 * time.Hour, last: 45 * time.Second},
		{conv: "d0c00000-0000-4000-8000-000000000006", title: "cold-reviewer", role: "reviewer",
			harness: "opencode", model: "openai/gpt-5.6-sol", effort: "medium", status: "idle", online: true, cost: 1.30,
			startBranch: "main", tags: []string{"review"}, age: 40 * time.Minute, last: 20 * time.Minute},
		{conv: "d0c00000-0000-4000-8000-000000000007", title: "perf-audit", role: "dev",
			harness: "claude", model: "Opus 5.5", effort: "medium", online: false, cost: 9.60,
			startBranch: "lighthouse-budget", age: 30 * time.Hour, last: 26 * time.Hour},
	}
	for _, a := range agents {
		seedDocAgent(t, f, main.ID, "webshop", a)
	}
	// A subscription account shows per-agent what-if cost (≈$) only when the
	// operator opts in; do so, as most fleet operators running the dashboard do.
	if err := config.Save(&config.Config{Cost: &config.CostConfig{ShowOnSubscription: true}}); err != nil {
		t.Fatalf("cost config: %v", err)
	}
	seedDocUsage(t)
	seedUsageHistoryDashSnap(t) // the Usage tab only appears once history exists
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
