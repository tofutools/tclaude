package agentd_test

// dashsnap_test.go is the DRIVER for the JOH-386 visual smoke harness. It composes
// a canned dashboard fixture through the SAME test seams the flow tests use
// (newFlow / HaveGroup / HaveMember / BuildDashboardHandlerForTest — NOT a
// parallel fixture system), serves the real dashboard handler over a real TCP
// port via httptest, and drives a headless Chrome (pkg/.../dashsnap, the only
// importer of the rod browser driver) across a both-skins state matrix.
//
// It is a normal `_test.go` — compiled by `go test ./...` so it can never
// silently bit-rot — but GATED behind the TCLAUDE_DASHSNAP env var so CI never
// launches a browser. That env gate (rather than a build tag) is deliberate: it
// keeps rod a normal, tidy-stable test dependency reached only through the
// dashsnap package, while `go list -deps ./` (the tclaude binary) stays free of
// rod. The full matrix takes on the order of ten minutes, so the canonical
// invocation shards it (each shard is a deterministic round-robin subset; run
// 1/4 through 4/4 to cover everything, or drop the shard variable with
// -timeout 1800s for one full run):
//
//	TCLAUDE_DASHSNAP=1 TCLAUDE_DASHSNAP_SHARD=1/4 go test ./pkg/claude/agentd/ -run TestDashSnap -v -count=1 -timeout 600s
//
// Output: dashsnap-out/<timestamp><shard-suffix>/ (gitignored) with one PNG per
// state + an index.html contact sheet. See pkg/claude/agentd/dashsnap/dashsnap.go
// for the runtime prerequisites (system Chrome, --no-sandbox, the harmless
// stderr noise).

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDashSnap is the manually-run visual smoke harness. It is skipped unless
// TCLAUDE_DASHSNAP is set, so `go test ./...` (CI) compiles it but never drives a
// browser.
func TestDashSnap(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("visual smoke harness — set TCLAUDE_DASHSNAP=1 to run (needs a local headless Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)

	// Serve the REAL dashboard handler over a real port. dashTestHandler injects
	// the session cookie on every request (so a browser is authed without a login
	// flow), and popupBaseURL is left empty so the loopback Origin pin is off —
	// the browser's same-origin fetches carry a Referer and pass auth.
	//
	// One guard wraps it: the retire-busy state relies on an InitJS fetch hold
	// to keep its retire POST inside the browser, and that seam must fail
	// CLOSED. If the production dialog ever stops routing through the held
	// window.fetch (different primitive, changed URL), the escaped request is
	// recorded and rejected here — 503, nothing mutated — instead of retiring
	// the live fixture agent and poisoning the later wizard-skin pass. The
	// count is asserted to be zero after the matrix.
	var busyRetireEscapes atomic.Int64
	dash := agentd.BuildDashboardHandlerForTest()
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/agents/"+retireBusyConv+"/retire" {
			busyRetireEscapes.Add(1)
			http.Error(w, "dashsnap: the retire-busy POST must be held open in the browser, never reach the daemon",
				http.StatusServiceUnavailable)
			return
		}
		dash.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(guarded)
	defer srv.Close()

	states := dashSnapStates()
	// Every visual state also proves that the production shell island claimed
	// and rendered its hosts through the embedded Preact + Signals graph.
	const preactRuntimeReady = `
var __preactShell = document.querySelector('#shell-status-root[data-island-owner="shell"]');
if (!__preactShell || !__preactShell.firstElementChild) throw new Error('Preact shell runtime not ready');
`
	for i := range states {
		states[i].JS = preactRuntimeReady + states[i].JS
	}
	filter := os.Getenv("TCLAUDE_DASHSNAP_FILTER")
	if filter != "" {
		filtered := states[:0]
		for _, state := range states {
			if strings.Contains(state.Key, filter) {
				filtered = append(filtered, state)
			}
		}
		if len(filtered) == 0 {
			t.Fatalf("TCLAUDE_DASHSNAP_FILTER %q matched no states", filter)
		}
		states = filtered
	}
	// TCLAUDE_DASHSNAP_SHARD=i/n bounds one invocation to a deterministic
	// round-robin slice of the (filtered) matrix, so each documented command
	// finishes within a predictable budget while the shards together cover
	// every state.
	matrixSize := len(states)
	shard, err := dashsnap.ParseShard(os.Getenv("TCLAUDE_DASHSNAP_SHARD"))
	if err != nil {
		t.Fatalf("TCLAUDE_DASHSNAP_SHARD: %v", err)
	}
	states = shard.Pick(states)
	if len(states) == 0 {
		// A filter can shrink the matrix below the shard count; every remaining
		// state then lands in a lower-numbered shard. That makes an empty shard
		// a clean no-op — not a failure — so a fixed shard loop (the canonical
		// 1/4..4/4 commands) stays combinable with any TCLAUDE_DASHSNAP_FILTER.
		t.Skipf("TCLAUDE_DASHSNAP_SHARD %d/%d selects no states (%d after filtering) — all covered by lower shards",
			shard.Index, shard.Total, matrixSize)
	}
	if shard.Enabled() {
		t.Logf("dashsnap: shard %d/%d — capturing %d of %d states", shard.Index, shard.Total, len(states), matrixSize)
	}

	// Millisecond granularity so two runs in the same second don't overwrite;
	// the shard suffix keeps concurrent shard outputs apart even then.
	outDir := filepath.Join(dashSnapOutRoot(t), time.Now().Format("20060102-150405.000")+shard.Suffix())
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States:  states,
	})
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}

	sheet := filepath.Join(outDir, "index.html")
	var failed []string
	var elapsed time.Duration
	for _, s := range shots {
		status := "ok"
		if s.Err != "" {
			status = "FAIL: " + s.Err
			failed = append(failed, s.State.Key)
		}
		elapsed += s.Elapsed
		t.Logf("  [%-18s] %5.1fs %s", s.State.Key, s.Elapsed.Seconds(), status)
	}
	t.Logf("dashsnap: %d states, %d failed, captured in %s", len(shots), len(failed), elapsed.Round(time.Second))
	t.Logf("contact sheet: file://%s", sheet)

	if n := busyRetireEscapes.Load(); n != 0 {
		t.Errorf("retire-busy: %d retire request(s) escaped the browser fetch hold and reached the daemon — "+
			"the InitJS seam no longer matches the production request path (rejected without mutating)", n)
	}
	if len(failed) > 0 {
		t.Fatalf("dashsnap: %d/%d states failed to capture: %s (sheet: %s)",
			len(failed), len(shots), strings.Join(failed, ", "), sheet)
	}
}

// dashSnapOutRoot resolves the repo-root dashsnap-out/ dir (gitignored). The test
// runs with cwd = the package dir (pkg/claude/agentd), so walk up to the module
// root; fall back to the package dir if the walk fails.
func dashSnapOutRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "dashsnap-out")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(wd, "dashsnap-out")
}

// ---------------------------------------------------------------------------
// Fixture — 2 groups, tagged online/offline members (incl. a tf:<template> chip),
// task links, an owner, plus templates/profiles/roles so the palette dock has
// content to render and drag.
// ---------------------------------------------------------------------------

const (
	tfTemplate   = "frontend-squad" // the template whose card the summon states drag
	otherGroup   = "infra-crew"
	linearTask   = "https://linear.app/tofutools/issue/JOH-386"
	otherTaskURL = "https://github.com/tofutools/tclaude/pull/386"

	// The single-agent retire dialog states (TCL-491) each pin one transaction
	// phase to its own fixture target so no state can mutate what another
	// state's skin pass still needs:
	//   - retireWorktreeConv: idle defaults only — never submitted.
	//   - retireBusyConv: submitted, but its retire POST is held open in the
	//     browser (see retireBusyFetchHoldJS) and never reaches the daemon.
	//   - retireErrorConv: a resolvable conv that is NOT a live agent, so the
	//     real daemon answers the submission with a non-mutating 409.
	//   - retireDanglingConv: an enrollment with no conversation data, so the
	//     real daemon answers 409 {dangling:true} (read-only) and the dialog
	//     hands off to the shell confirm — whose OK is never pressed.
	retireWorktreeConv = "c2000000-0000-4000-8000-000000000003"
	retireBusyConv     = "c2000000-0000-4000-8000-000000000004"
	retireErrorConv    = "e4910000-0000-4000-8000-000000000001"
	retireDanglingConv = "da491000-0000-4000-8000-000000000001"

	// badgesConv is the TCL-613 activity-badge row: an agent whose turn has
	// ended but which still has a sub-agent and two background shell
	// commands running, so the Groups roster draws both 🤖+N and ⚙+N.
	badgesConv = "b6130000-0000-4000-8000-000000000001"

	retireWorktreeDir = "/tmp/lbl-in-wt"
	retireBusyDir     = "/tmp/lbl-in-busy"
	retireErrorDir    = "/tmp/lbl-retire-err"
)

// dashMemberSpec is one seeded member.
type dashMemberSpec struct {
	convID             string
	label              string // TCLAUDE_SESSION_ID / session row id
	tmux               string
	title              string
	role               string
	status             string // "" leaves the SaveSession default ("running"); else SetSessionStatus
	online             bool   // false → HaveAliveSession then MarkOffline
	owner              bool
	tags               []string
	taskURL, taskLabel string
}

func seedDashSnapFixture(t *testing.T, f *testharness.Flow) {
	t.Helper()

	fe := f.HaveGroup(tfTemplate)
	infra := f.HaveGroup(otherGroup)

	feMembers := []dashMemberSpec{
		{convID: "f1000000-0000-4000-8000-000000000001", label: "lbl-fe-lead", tmux: "tmux-fe-lead",
			title: "fe-lead", role: "lead", status: "running", online: true, owner: true,
			tags: []string{"tf:" + tfTemplate, "ui"}, taskURL: linearTask, taskLabel: "JOH-386"},
		{convID: "f1000000-0000-4000-8000-000000000002", label: "lbl-fe-dev1", tmux: "tmux-fe-dev1",
			title: "fe-dev-forms", role: "dev", status: "awaiting_input", online: true,
			tags: []string{"tf:" + tfTemplate, "reviewer"}},
		{convID: "f1000000-0000-4000-8000-000000000003", label: "lbl-fe-dev2", tmux: "tmux-fe-dev2",
			title: "fe-dev-charts", role: "dev", status: "working", online: true,
			tags: []string{"tf:" + tfTemplate}},
		{convID: "f1000000-0000-4000-8000-000000000004", label: "lbl-fe-off", tmux: "tmux-fe-off",
			title: "fe-dev-legacy", role: "dev", online: false,
			tags: []string{"tf:" + tfTemplate}},
		// TCL-613 — the activity-badge row: an agent whose own turn has
		// ended but which still has a sub-agent AND background shell
		// commands running. Its ledgers are stamped by
		// seedActivityBadgesDashSnap below.
		{convID: badgesConv, label: "lbl-fe-bg", tmux: "tmux-fe-bg",
			title: "fe-dev-watcher", role: "dev", status: "main_agent_idle", online: true,
			tags: []string{"tf:" + tfTemplate}},
	}
	infraMembers := []dashMemberSpec{
		{convID: "c2000000-0000-4000-8000-000000000001", label: "lbl-in-lead", tmux: "tmux-in-lead",
			title: "infra-lead", role: "lead", status: "running", online: true, owner: true,
			tags: []string{"backend"}, taskURL: otherTaskURL, taskLabel: "PR #386"},
		{convID: "c2000000-0000-4000-8000-000000000002", label: "lbl-in-dev", tmux: "tmux-in-dev",
			title: "infra-dev-db", role: "dev", online: false,
			tags: []string{"backend", "sqlite"}},
		// The retire-dialog states' row-opened targets (TCL-491). Their cwds
		// ("/tmp/"+label) carry canned removable-worktree probes; see
		// seedRetireDialogDashSnap for why each state gets its own agent.
		{convID: retireWorktreeConv, label: "lbl-in-wt", tmux: "tmux-in-wt",
			title: "infra-dev-worktree", role: "dev", status: "running", online: true},
		{convID: retireBusyConv, label: "lbl-in-busy", tmux: "tmux-in-busy",
			title: "infra-dev-busy", role: "dev", status: "running", online: true},
	}

	for _, m := range feMembers {
		seedMember(t, f, fe.ID, tfTemplate, m)
	}
	for _, m := range infraMembers {
		seedMember(t, f, infra.ID, otherGroup, m)
	}
	base := time.Now().Add(-8 * time.Minute)
	if _, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: fe.ID, FromConv: feMembers[0].convID, ToConv: feMembers[1].convID,
		Subject: "Review the Preact migration", Body: "Please verify the Messages reader at https://example.com/review.", CreatedAt: base,
	}); err != nil {
		t.Fatalf("seed dashboard agent message: %v", err)
	}
	if _, err := db.InsertHumanMessage(&db.HumanMessage{
		FromConv: feMembers[1].convID, FromTitle: feMembers[1].title,
		Subject: "Profile editor parity", Body: "The form and wizard-theme checks are ready for review.\n\nPlease test the focused controls and resize gutters.",
		CreatedAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("seed dashboard human message: %v", err)
	}
	if _, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "dashsnap-release-ritual", OwnerConv: feMembers[0].convID,
		TargetKind: db.CronTargetGroup, GroupID: fe.ID, TargetRole: "dev",
		CronExpr: "0 9 * * 1-5", Subject: "Release readiness",
		Body:    "Report final checks and blockers before the weekday release window.",
		Enabled: true, CreatedAt: base.Add(4 * time.Minute),
	}); err != nil {
		t.Fatalf("seed dashboard cron job: %v", err)
	}

	// Nest infra-crew under frontend-squad so the Groups tab renders the group
	// TREE (n-level groups-in-groups, JOH-392): infra-crew draws inside
	// frontend-squad's <details>, above its own member list. The groups-nested
	// scenario self-checks that structure.
	if _, err := db.SetAgentGroupParent(infra.ID, tfTemplate); err != nil {
		t.Fatalf("nest %s under %s: %v", otherGroup, tfTemplate, err)
	}

	// Record deployment provenance on frontend-squad (mission + source_template)
	// so isDeployedForce is true and the group renders its task-force info card
	// (renderForceBlock) — the surface the 🎯 hide/show toggle acts on. Without
	// this the card (and its toggle) never render, so the fold states below would
	// have nothing to fold.
	if _, err := db.SetAgentGroupDeployMeta(tfTemplate, "Ship the new dashboard charts", tfTemplate); err != nil {
		t.Fatalf("set deploy meta on %s: %v", tfTemplate, err)
	}

	seedActivityBadgesDashSnap(t)
	seedPalette(t, f)
	seedProcessDashSnap(t, f)
	seedUsageHistoryDashSnap(t)
	seedRetireDialogDashSnap(t, f)
}

// seedActivityBadgesDashSnap stamps the two activity ledgers onto the
// fe-dev-watcher row (TCL-613), so the Groups roster renders 🤖+1 and ⚙+2
// beside an agent whose own turn has ended — the case the badges exist for.
//
// The ledgers are written straight onto the session row rather than driven
// through hooks because a fixture must be deterministic: the row's pid stays
// 0, so the daemon's background-shell liveness reconcile reports "cannot
// tell" and falls back to the ledger's own TTL-filtered view instead of
// retiring entries whose processes never existed on this host.
func seedActivityBadgesDashSnap(t *testing.T) {
	t.Helper()
	row, err := db.LoadSession("lbl-fe-bg")
	if err != nil || row == nil {
		t.Fatalf("seedActivityBadgesDashSnap: load session: %v", err)
	}
	now := time.Now()
	row.SubagentCount = 1
	row.SubagentsJSON = db.SubagentSet{
		"ag-explore": {Type: "Explore", Seen: now},
	}.Encode()
	row.BgShellsJSON = db.BgShellSet(nil).
		Add("task-dev", "npm run dev --port 4321", now).
		Add("task-watch", "go test ./... -run TestWatch -count=1", now).
		Encode()
	row.Status = "main_agent_idle"
	row.StatusDetail = "1 subagents, 2 background shells running"
	if err := db.SaveSession(row); err != nil {
		t.Fatalf("seedActivityBadgesDashSnap: save session: %v", err)
	}
}

// seedRetireDialogDashSnap stages the single-agent retire dialog states
// (TCL-491). The two roster members above cover the idle and busy phases; this
// adds the two 409-answered targets plus the deterministic worktree probes.
// Every retire state either never submits, is held open in the browser, or is
// answered by a real non-mutating daemon 409 — so the default-skin pass leaves
// the fixture exactly as the wizard-skin pass expects to find it.
func seedRetireDialogDashSnap(t *testing.T, f *testharness.Flow) {
	t.Helper()

	// A resolvable plain conversation that was never an agent: retiring it makes
	// the REAL daemon answer 409 "not a live agent … nothing to retire" without
	// mutating anything, which the error state uses for its inline-error/retry
	// phase instead of a stubbed response.
	f.HaveConvWithTitle(retireErrorConv, "retire-error-target")
	f.HaveAliveSession(retireErrorConv, "lbl-retire-err", "tmux-retire-err", retireErrorDir)

	// A dangling enrollment — an active agent row whose conversation data is
	// gone. Retire answers the read-only 409 {dangling:true}, handing the dialog
	// off to the shell confirm (the state never presses its OK).
	f.HaveEnrolledAgent(retireDanglingConv)

	// Canned worktree probes through the same fake-git seam the cleanup flow
	// tests use: the three retire targets report their cwd as a removable linked
	// worktree; every other directory stays kind "none", so no unrelated dialog
	// grows a worktree row.
	installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		retireWorktreeDir: {Root: retireWorktreeDir, Branch: "tcl-491-idle-probe", Kind: "linked"},
		retireBusyDir:     {Root: retireBusyDir, Branch: "tcl-491-busy-probe", Kind: "linked"},
		retireErrorDir:    {Root: retireErrorDir, Branch: "tcl-491-error-probe", Kind: "linked"},
	})
}

func seedUsageHistoryDashSnap(t *testing.T) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour).Truncate(15 * time.Minute)
	claudeFiveHour := []float64{72, 18, 25, 31}
	claudeWeekly := []float64{40, 42, 45, 48}
	codexWeekly := []float64{55, 59, 62, 66}
	for i := range claudeFiveHour {
		observedAt := base.Add(time.Duration(i) * 15 * time.Minute)
		if _, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderAnthropic, ObservedAt: observedAt, Source: "dashsnap",
			Windows: []db.SubscriptionUsageWindow{
				{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: claudeFiveHour[i], ResetsAt: base.Add(5 * time.Hour)},
				{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: claudeWeekly[i], ResetsAt: base.Add(6 * 24 * time.Hour)},
			},
		}); err != nil {
			t.Fatalf("seed Claude usage history: %v", err)
		}
		if _, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderOpenAI, ObservedAt: observedAt, Source: "dashsnap",
			Windows: []db.SubscriptionUsageWindow{
				{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: codexWeekly[i], ResetsAt: base.Add(5 * 24 * time.Hour)},
			},
		}); err != nil {
			t.Fatalf("seed Codex usage history: %v", err)
		}
	}
}

func seedProcessDashSnap(t *testing.T, f *testharness.Flow) {
	t.Helper()
	requireNoError := func(label string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	requireNoError("enable Processes", config.Save(&config.Config{
		Features:  &config.FeaturesConfig{Processes: true},
		Dashboard: &config.DashboardConfig{DefaultDirectoryPicker: config.DefaultDirectoryPickerWeb},
	}))
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	fs, err := store.NewFS(root)
	requireNoError("create process store", err)
	// Worklist fixtures FIRST: they run an engine tick to mint the human
	// obligations, and the tick must not see (and advance to completion) the
	// hand-crafted release-42 run created below.
	seedProcessWorklistDashSnap(t, root)
	required := true
	tmpl := &model.Template{
		APIVersion:  model.APIVersion,
		Kind:        model.Kind,
		ID:          "release-train",
		Name:        "Release train",
		Description: "Plan, implement, review, and ship a dashboard release.",
		Params: map[string]model.Param{
			"issue":       {Type: "string", Description: "Tracker issue to implement", Required: &required},
			"retries":     {Type: "number", Description: "Maximum implementation passes", Default: 2},
			"shipPreview": {Type: "boolean", Description: "Publish a preview before release", Default: true},
		},
		Start: "begin",
		Nodes: map[string]model.Node{
			"begin": {Type: model.NodeTypeStart, Next: model.Next{"pass": "ship"}},
			"ship":  {Type: model.NodeTypeEnd, Result: "success"},
		},
		Layout: &model.Layout{Nodes: map[string]model.LayoutNode{
			"begin": {X: 80, Y: 120}, "ship": {X: 360, Y: 120},
		}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	requireNoError("seed process template", err)
	initial := state.New("dashsnap-release-42", record.Ref, record.Ref, []state.NodeInit{
		{ID: "begin", Type: model.NodeTypeStart, Status: state.NodeStatusReady},
		{ID: "ship", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{
		ID: "dashsnap-release-42", TemplateRef: record.Ref,
		CreatedAt: time.Now().Add(-12 * time.Minute),
	}, initial)
	requireNoError("seed process run", err)
}

// seedProcessWorklistDashSnap populates the Worklist sub-view (TCL-297): two
// pending human decisions (operator + oncall assignees, one with a visible
// contact/nudge schedule) minted by a real engine tick, and a corrupt run so
// the amber degraded-runs strip renders in every worklist state.
func seedProcessWorklistDashSnap(t *testing.T, root string) {
	t.Helper()
	createEngineRun(t, root, "dashsnap-approve-7", decisionTemplate("dashsnap-approve", model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve the release-train cut?",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:oncall"},
	}), false)
	createEngineRun(t, root, "dashsnap-signoff-8", decisionTemplate("dashsnap-signoff", model.Performer{
		Kind: model.PerformerHuman, Profile: "oncall", Ask: "Sign off the incident follow-up?",
	}), false)
	createEngineRun(t, root, "dashsnap-parallel-any-9", &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "dashsnap-parallel-any",
		Name:       "Parallel-any deployment checks",
		Start:      "fork",
		Nodes: map[string]model.Node{
			"fork": {Type: model.NodeTypeParallel, Next: model.Next{"primary": "primary-wait", "independent": "independent-wait"}},
			"primary-wait": {
				Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "24h"}, Next: model.Next{"pass": "accepted"},
			},
			"independent-wait": {
				Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "24h"}, Next: model.Next{"pass": "accepted"},
			},
			"accepted": {Type: model.NodeTypeEnd, Join: model.JoinAny, Result: "completed"},
		},
	}, false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	if err != nil {
		t.Fatalf("worklist engine host: %v", err)
	}
	if _, err := agentd.RunProcessEngineTickForTest(t.Context(), host); err != nil {
		t.Fatalf("worklist engine tick: %v", err)
	}
	corrupt := filepath.Join(root, "runs", "dashsnap-corrupt-run")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatalf("worklist corrupt run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "run.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatalf("worklist corrupt run.json: %v", err)
	}
	// A schema-8 run seeded AFTER the engine tick through the epoch-v8
	// constructor (createEngineRun would mint a legacy run): its runtime
	// stays unattached, so it truthfully has zero worklist items (keeping the
	// badge fixtures stable) while the viewer renders the full safe
	// adaptation summary and unlock panel.
	epochTemplate := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "dashsnap-epoch",
		Name:       "Adaptable release hold",
		Start:      "hold",
		Nodes: map[string]model.Node{
			"hold": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "go-ahead"}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	epochSource, err := model.CanonicalYAML(epochTemplate)
	if err != nil {
		t.Fatalf("epoch template source: %v", err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatalf("epoch store: %v", err)
	}
	epochRecord, err := fs.PutTemplate(t.Context(), epochTemplate)
	if err != nil {
		t.Fatalf("epoch template: %v", err)
	}
	if _, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "dashsnap-epoch-8", TemplateRef: epochRecord.Ref}, epochSource); err != nil {
		t.Fatalf("epoch run: %v", err)
	}
}

func seedMember(t *testing.T, f *testharness.Flow, groupID int64, group string, m dashMemberSpec) {
	t.Helper()
	// Title (display name), live session, membership (which mints the actor).
	f.HaveConvWithTitle(m.convID, m.title)
	f.HaveAliveSession(m.convID, m.label, m.tmux, "/tmp/"+m.label)
	f.HaveMemberWithRole(group, m.convID, m.role)

	if m.status != "" {
		f.SetSessionStatus(m.convID, m.status)
	}
	if !m.online {
		f.MarkOffline(m.tmux)
	}
	if m.owner {
		if err := db.AddAgentGroupOwner(groupID, m.convID, "dashsnap"); err != nil {
			t.Fatalf("seedMember owner %s: %v", m.convID, err)
		}
	}

	agentID, err := db.AgentIDForConv(m.convID)
	if err != nil || agentID == "" {
		t.Fatalf("seedMember: no actor for %s: %v", m.convID, err)
	}
	if len(m.tags) > 0 {
		if err := db.ReplaceAgentTags(agentID, m.tags); err != nil {
			t.Fatalf("seedMember tags %s: %v", m.convID, err)
		}
	}
	if m.taskURL != "" {
		if _, err := db.SetAgentTaskRef(agentID, m.taskURL, m.taskLabel); err != nil {
			t.Fatalf("seedMember task %s: %v", m.convID, err)
		}
	}
}

// seedPalette fills the dock's three sections: templates (via the real
// POST /v1/templates endpoint), spawn profiles and roles (direct db writes).
func seedPalette(t *testing.T, f *testharness.Flow) {
	t.Helper()

	// Templates — the summon states drag the tfTemplate card, so it must exist.
	mkTemplate(t, f, tfTemplate, []templateAgentSpec{
		{Name: "lead", Role: "lead", IsOwner: true},
		{Name: "dev", Role: "dev"},
	})
	mkTemplate(t, f, otherGroup, []templateAgentSpec{
		{Name: "lead", Role: "lead", IsOwner: true},
		{Name: "dev", Role: "dev", Wave: 1},
	})
	mkTemplate(t, f, "review-panel", []templateAgentSpec{
		{Name: "reviewer-a", Role: "reviewer"},
		{Name: "reviewer-b", Role: "reviewer"},
	})

	// Spawn profiles.
	for _, p := range []db.SpawnProfile{
		{Name: "opus-fast", Descr: "Opus, fast, auto-review", Model: "claude-opus-4-8", Effort: "high"},
		{Name: "sonnet-review", Descr: "Sonnet reviewer", Model: "claude-sonnet-5", Effort: "medium"},
	} {
		if _, err := db.CreateSpawnProfile(&p); err != nil && !errors.Is(err, db.ErrSpawnProfileNameTaken) {
			t.Fatalf("seedPalette profile %s: %v", p.Name, err)
		}
	}

	// Roles. Creating a template above already auto-registers each agent's role
	// (lead/dev/reviewer) in the roles registry, so tolerate the name collision —
	// the point is only that the dock's Roles section has content.
	for _, r := range []db.Role{
		{Name: "lead", Descr: "Coordinates the squad", Brief: "You lead the group."},
		{Name: "dev", Descr: "Implements features", Brief: "You implement features."},
		{Name: "reviewer", Descr: "Cold-reviews diffs", Brief: "You review diffs cold."},
	} {
		if _, err := db.CreateRole(&r); err != nil && !errors.Is(err, db.ErrRoleNameTaken) {
			t.Fatalf("seedPalette role %s: %v", r.Name, err)
		}
	}
}

func mkTemplate(t *testing.T, f *testharness.Flow, name string, agents []templateAgentSpec) {
	t.Helper()
	rec := humanReq(t, f, "POST", "/v1/templates", map[string]any{
		"name":   name,
		"descr":  "canned " + name + " circle",
		"agents": agents,
	})
	if rec.Code != 201 {
		t.Fatalf("mkTemplate %s: code=%d body=%s", name, rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// State matrix — {default, wizard} × {groups, dock-open, dock-collapsed, summon
// normal/reinforce/copy}. Driven via DOM clicks / classList / synthetic DnD,
// because no dashboard module exports anything on `window`.
// ---------------------------------------------------------------------------

func dashSnapStates() []dashsnap.State {
	var states []dashsnap.State
	for _, skin := range []struct {
		id     string
		wizard bool
	}{{"default", false}, {"wizard", true}} {
		for _, s := range baseStates() {
			s.Key = skin.id + "-" + s.Key
			s.Title = skin.id + " · " + s.Title
			s.Wizard = skin.wizard
			states = append(states, s)
		}
	}
	return states
}

func baseStates() []dashsnap.State {
	clipboardModifier := "Control"
	if runtime.GOOS == "darwin" {
		clipboardModifier = "Meta"
	}
	// showGroups activates the Groups tab. expandGroups opens every real group's
	// <details> so the member rows (tags, task links, online/offline, owner) show;
	// collapseGroups closes them for a header-only view.
	const showGroups = `document.querySelector('nav [data-tab="groups"]').click();`
	const expandGroups = `document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){d.open=true;});`
	const collapseGroups = `document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){d.open=false;});`
	// The 🎯 force-fold state is stored in dashPrefs, which persists SERVER-side
	// in the run's shared SQLite — so once ANY state folds frontend-squad's card
	// the fold leaks into every later state (unlike group open/close, which each
	// state re-forces via `d.open`). The card's visibility is a render-time read
	// of that pref, not a DOM toggle we can override, so a card-showing state must
	// reconcile it: click the 🎯 toggle iff the card is currently absent (folded).
	// A no-op when the card is already open. Prepended to every state that expects
	// the open card so ordering (and the folded state below) can't taint it.
	const ensureForceOpen = `window.__groupsForceReady = (async function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  if (det && det.open && !det.querySelector(':scope > .subtable > .group-force-block')) {
    var b = det.querySelector('.force-fold-btn');
    if (b) {
      b.click();
      await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
    }
  }
})();`
	states := []dashsnap.State{
		{
			Key:     "startup-cls",
			Title:   "Stable startup layout",
			Caption: "The initial theme, persisted dock geometry, Preact shell and first snapshot become visible as one settled frame; buffered Chrome layout shifts stay below 0.01.",
			JS: `return new Promise(function(resolve, reject) {
  if (!PerformanceObserver.supportedEntryTypes.includes('layout-shift')) {
    reject(new Error('Chrome does not expose layout-shift performance entries'));
    return;
  }
  var score = 0;
  var sources = [];
  var observer = new PerformanceObserver(function(list) {
    for (var entry of list.getEntries()) {
      if (entry.hadRecentInput) continue;
      score += entry.value;
      for (var source of entry.sources) {
        if (!source.node) continue;
        sources.push(source.node.id ? '#' + source.node.id : source.node.className || source.node.tagName);
      }
    }
  });
  observer.observe({type: 'layout-shift', buffered: true});
  setTimeout(function() {
    observer.disconnect();
    if (score > 0.01) {
      reject(new Error('startup CLS ' + score.toFixed(4) + ' from ' + sources.slice(0, 12).join(', ')));
      return;
    }
    resolve();
  }, 50);
});`,
			SettleMS: 100,
		},
		{
			Key:     "shell-normal",
			Title:   "Preact shell — populated",
			Caption: "TCL-360: accepted snapshot data fills the activity, usage, status, badges and footer shell without changing the plain/wizard header geometry.",
			JS: showGroups + collapseGroups + `document.body.classList.remove('dock-open');` + `
  for (var id of ['global-activity','usage','status','messages-badge','meta','notify-global','command-palette-btn']) {
    if (!document.getElementById(id)) throw new Error('shell-normal: missing #' + id);
  }`,
			SettleMS: 300,
		},
		{
			Key:     "shell-notifications",
			Title:   "Preact shell — notification popover",
			Caption: "TCL-360 populated form gate: the real notification GET fills native checkboxes, labels, help titles and Config shortcut inside the plain/wizard shell.",
			JS: showGroups + collapseGroups + `return (async function(){
  var bell = document.querySelector('#notify-global');
  if (!bell || bell.hidden) throw new Error('shell-notifications: bell missing or not ready');
  bell.click();
  var deadline = Date.now() + 3000;
  while (!document.querySelector('#notify-pop.open') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  var pop = document.querySelector('#notify-pop.open');
  if (!pop) throw new Error('shell-notifications: popover did not open');
  if (pop.querySelectorAll('input[type="checkbox"]').length !== 8) throw new Error('shell-notifications: native checkbox catalog changed');
  if (!pop.querySelector('#notify-pop-config')) throw new Error('shell-notifications: Config shortcut missing');
})();`,
			SettleMS: 200,
		},
		{
			Key:     "shell-palette",
			Title:   "Preact shell — command palette",
			Caption: "TCL-360 keyboard shell gate: the launcher opens the modal-overlay, focuses the combobox and keeps its accessible active-option wiring in both themes.",
			JS: showGroups + collapseGroups + `return (async function(){
  document.querySelector('#command-palette-btn').click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var modal = document.querySelector('#command-palette-modal.show');
  var input = document.querySelector('#palette-input');
  if (!modal || !input) throw new Error('shell-palette: overlay did not open');
  if (document.activeElement !== input) throw new Error('shell-palette: combobox did not take focus');
  if (!input.getAttribute('aria-activedescendant')) throw new Error('shell-palette: active option is not announced');
})();`,
			SettleMS: 200,
		},
		{
			Key:     "shell-feedback",
			Title:   "Preact shell — error toast and confirmation",
			Caption: "TCL-360 error/dialog gate: the shared error toast and confirmation render together with preserved copy, danger action and initial focus under plain/wizard styling.",
			JS: showGroups + collapseGroups + `return (async function(){
  var feedback = await import('/static/js/refresh.js');
  feedback.toast('Representative shell error', true);
  void feedback.confirmModal({title:'Confirm shell action', body:'This checks the shared confirmation surface.', meta:'representative metadata', okLabel:'Continue'});
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (!document.querySelector('#toast.show.error')) throw new Error('shell-feedback: error toast missing');
  if (!document.querySelector('#confirm-modal.show')) throw new Error('shell-feedback: confirmation missing');
  if (document.activeElement !== document.querySelector('#confirm-ok')) throw new Error('shell-feedback: confirm action did not take focus');
})();`,
			SettleMS: 200,
		},
		{
			Key:     "shell-disconnected",
			Title:   "Preact shell — disconnected",
			Caption: "TCL-360 connection gate: two rejected-poll reports drive the shared connection Signal, accessible reconnect overlay and theme-compatible shell presentation.",
			JS: showGroups + collapseGroups + `return (async function(){
  var connection = await import('/static/js/connection.js');
  connection.noteDisconnected();
  connection.noteDisconnected();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var overlay = document.querySelector('#disconnect-overlay.show');
  if (!overlay || !overlay.querySelector('[role="alert"]')) throw new Error('shell-disconnected: accessible overlay missing');
})();`,
			SettleMS: 200,
		},
		{
			Key:      "bounded-directory-picker",
			Title:    "Bounded Preact — filtered directory picker",
			Caption:  "The shared host-directory navigator used by every Browse… action, with the path's unfinished final component filtering and highlighting the existing folder pane in default/wizard chrome.",
			JS:       directoryPickerDashSnapJS(),
			SettleMS: 300,
		},
		{
			Key:      "bounded-messages-populated",
			Title:    "Bounded Preact — Messages populated",
			Caption:  "Messages island keeps all three panes independently scrollable, the sidebar footer toggles visible, and the client clear of the fixed dashboard footer in both skins.",
			JS:       boundedMessagesJS(),
			SettleMS: 600,
		},
		{
			Key:     "message-access-populated",
			Title:   "Message/access dialogs — populated compose",
			Caption: "TCL-454: the Preact-owned scoped composer renders the live roster, role/class filter, populated draft, and sole migrated modal id in the same viewport and both skins.",
			JS: `return (async function(){
  var dialogs = await import('/static/js/message-access-dialog-controller.js');
  dialogs.openMessageCreateModal({from:'fe-lead', targetMode:'group', groupName:'frontend-squad'});
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  function input(id, value) { var el=document.getElementById(id); el.value=value; el.dispatchEvent(new Event('input',{bubbles:true})); }
  input('message-create-subject','Release readiness');
  input('message-create-role','dev');
  input('message-create-body','Please report final checks and any blockers.');
  var modal=document.querySelector('#message-create-modal.show');
  if (!modal || document.querySelectorAll('#message-create-modal').length !== 1) throw new Error('message-access-populated: composer ownership failed');
  if (!modal.querySelector('#message-create-members-count') || !modal.querySelector('#message-create-role')) throw new Error('message-access-populated: scoped controls missing');
})();`,
			SettleMS: 250,
		},
		{
			Key:     "message-access-error",
			Title:   "Message/access dialogs — validation error",
			Caption: "TCL-454 error state: validation remains inline inside the Preact composer without closing or retargeting the authoritative launch.",
			JS: `return (async function(){
  var dialogs = await import('/static/js/message-access-dialog-controller.js');
  dialogs.openMessageCreateModal({target:'fe-dev-forms'});
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  document.querySelector('#message-create-submit').click();
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var error=document.querySelector('#message-create-error');
  if (!error || !error.textContent.trim()) throw new Error('message-access-error: inline validation missing');
})();`,
			SettleMS: 250,
		},
		{
			Key:     "message-access-busy",
			Title:   "Message/access dialogs — busy grant",
			Caption: "TCL-454 busy-state visual gate: the Preact sudo surface keeps its selected catalog visible while the primary action is blocked and relabelled.",
			JS: `return (async function(){
  var dialogs = await import('/static/js/message-access-dialog-controller.js');
  dialogs.openSudoGrantModal('fe-dev-forms');
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  document.querySelector('#sudo-grant-select-all').click();
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var submit=document.querySelector('#sudo-grant-submit');
  if (!submit || !document.querySelector('#sudo-grant-slugs input:not([disabled]):checked')) throw new Error('message-access-busy: sudo selection missing');
  submit.disabled=true;
  submit.textContent='Granting…';
})();`,
			SettleMS: 250,
		},
		{
			Key:     "message-access-stacked",
			Title:   "Message/access dialogs — stacked chooser",
			Caption: "TCL-454 layering gate: the separately keyed shared chooser stacks over the populated composer without recreating or obscuring its draft.",
			JS: `return (async function(){
  var dialogs = await import('/static/js/message-access-dialog-controller.js');
  dialogs.openMessageCreateModal({from:'fe-lead', target:'fe-dev-forms'});
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var body=document.querySelector('#message-create-body');
  body.value='Draft retained beneath the chooser.';
  body.dispatchEvent(new Event('input',{bubbles:true}));
  document.querySelector('#message-create-from-pick').click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (!document.querySelector('#message-create-modal.show') || !document.querySelector('#cron-pick-target-modal.show')) throw new Error('message-access-stacked: both keyed layers are not visible');
  if (document.querySelector('#message-create-body') !== body || body.value.indexOf('Draft retained') !== 0) throw new Error('message-access-stacked: parent draft was recreated');
})();`,
			SettleMS: 250,
		},
		{
			Key:      "bounded-jobs-normal",
			Title:    "Bounded Preact — Jobs normal",
			Caption:  "Jobs island completed its initial request and rendered its normal fixture state.",
			JS:       boundedTabJS("jobs", "#cron-create-open"),
			SettleMS: 500,
		},
		{
			Key:      "jobs-cron-create-populated",
			Title:    "Jobs cron dialog — populated create",
			Caption:  "TCL-456 create gate: the Jobs-owned form shows a group target, role/class filter, cron-expression explanation, enabled state, and retained message draft in plain and wizard chrome.",
			JS:       jobsCronCreateDashSnapJS(),
			SettleMS: 500,
		},
		{
			Key:      "jobs-cron-edit-prefill",
			Title:    "Jobs cron dialog — edit prefill",
			Caption:  "TCL-456 edit gate: a real seeded Jobs row opens with its identity metadata, group and role/class target, schedule expression, subject, body, and enabled state prefilled.",
			JS:       jobsCronRowDialogDashSnapJS("edit", false),
			SettleMS: 500,
		},
		{
			Key:      "jobs-cron-duplicate-picker",
			Title:    "Jobs cron dialog — duplicate with target chooser",
			Caption:  "TCL-456 duplicate/layering gate: the copied draft keeps its source metadata and -copy name while the Jobs-owned agent chooser stacks above it without losing the parent form.",
			JS:       jobsCronRowDialogDashSnapJS("duplicate", true),
			SettleMS: 500,
		},
		{
			Key:      "bounded-plugins-normal",
			Title:    "Bounded Preact — Plugins normal",
			Caption:  "Plugins island completed its request and rendered the seeded Excalidraw catalog card.",
			JS:       boundedTabJS("plugins", `.plugin-catalog-card[data-key="catalog-excalidraw-mcp"]`),
			SettleMS: 500,
		},
		{
			Key:     "debug-alchemy",
			Title:   "Debug / Alchemy diagnostics",
			Caption: "The poll-timing cards retain their readable default skin while wizard mode adds the Alchemist's Observatory heading, violet panels and a gilded latency trace without recolouring categorical phase data.",
			JS: `return (async function(){
  document.querySelector('nav [data-tab="debug"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.debug-card'); i++) {
    await new Promise(function(resolve){ setTimeout(resolve, 50); });
  }
  var card = document.querySelector('.debug-card');
  var line = document.querySelector('.debug-spark-line');
  var title = document.querySelector('.debug-wizard-title');
  if (!card || !line || !title) throw new Error('debug-alchemy: diagnostic chrome did not render');
  var wizard = document.body.classList.contains('wizard');
  var cardStyle = getComputedStyle(card);
  var lineStyle = getComputedStyle(line);
  var titleStyle = getComputedStyle(title);
  // The invisible 14px keyboard hit targets must stay invisible in BOTH
  // themes. They are plain <line>s inside the marker groups, so a wizard rule
  // written as '.usage-reset-mark line' outspecifies the transparent hit-target
  // rule and paints an opaque band across the data. Checked by computed style
  // because that is the only way to see the cascade actually resolve.
  document.querySelectorAll('.usage-marker-hit-target, .usage-point-hit-target, .usage-forecast-hit-target').forEach(function(target){
    var stroke = getComputedStyle(target).stroke;
    if (stroke !== 'rgba(0, 0, 0, 0)' && stroke !== 'transparent' && stroke !== 'none') {
      throw new Error('usage-mana: hit target became visible (stroke ' + stroke + ') — a marker rule is outspecifying the transparent hit-target rule');
    }
  });
  if (wizard) {
    if (titleStyle.display === 'none') throw new Error('debug-alchemy: wizard title is hidden');
    if (!cardStyle.backgroundImage.includes('gradient')) throw new Error('debug-alchemy: wizard card lacks gradient chrome');
    if (lineStyle.stroke !== 'rgb(217, 180, 90)') throw new Error('debug-alchemy: wizard sparkline is not gilded');
  } else {
    if (titleStyle.display !== 'none') throw new Error('debug-alchemy: wizard title leaked into plain mode');
    if (cardStyle.backgroundImage !== 'none') throw new Error('debug-alchemy: wizard card chrome leaked into plain mode');
    if (lineStyle.stroke !== 'rgb(57, 135, 229)') throw new Error('debug-alchemy: plain sparkline changed colour');
  }
})();`,
			SettleMS: 300,
		},
		{
			Key:     "usage-mana",
			Title:   "Usage / mana reserves",
			Caption: "The quota graphs keep their readable default skin while wizard mode adds The Mana Reserves nameplate, violet scrying pools, a mana-blue channeled trace and the replenishment/prophecy vocabulary.",
			// Reuses the shared bounded-tab setup/settle rather than re-rolling the
			// usage_tab_visible + click + ready-poll dance, then asserts on top of it.
			JS: `return (async function(){
  await (async function(){ ` + boundedTabJS("usage", ".usage-series-card") + ` })();
  var card = document.querySelector('.usage-series-card');
  var line = document.querySelector('.usage-observed-line');
  var title = document.querySelector('.usage-wizard-title');
  var legend = document.querySelector('.usage-chart-legend');
  if (!card || !line || !title || !legend) throw new Error('usage-mana: reserve chrome did not render');
  var wizard = document.body.classList.contains('wizard');
  var cardStyle = getComputedStyle(card);
  var lineStyle = getComputedStyle(line);
  var titleStyle = getComputedStyle(title);
  if (wizard) {
    if (titleStyle.display === 'none') throw new Error('usage-mana: wizard nameplate is hidden');
    if (!cardStyle.backgroundImage.includes('gradient')) throw new Error('usage-mana: wizard card lacks scrying-pool chrome');
    // The channeled trace must be the same mana blue as a lit .ctx-mana segment.
    if (lineStyle.stroke !== 'rgb(77, 208, 225)') throw new Error('usage-mana: channeled trace is not mana blue');
    if (!legend.textContent.includes('Channeled')) throw new Error('usage-mana: legend did not take the wizard voice');
    if (legend.textContent.includes('Observed')) throw new Error('usage-mana: plain legend copy survived the theme flip');
  } else {
    if (titleStyle.display !== 'none') throw new Error('usage-mana: wizard nameplate leaked into plain mode');
    if (cardStyle.backgroundImage !== 'none') throw new Error('usage-mana: wizard card chrome leaked into plain mode');
    if (lineStyle.stroke !== 'rgb(88, 166, 255)') throw new Error('usage-mana: plain observed line changed colour');
    if (!legend.textContent.includes('Observed')) throw new Error('usage-mana: plain legend copy changed');
    if (legend.textContent.includes('Channeled')) throw new Error('usage-mana: wizard copy leaked into plain mode');
  }
})();`,
			SettleMS: 400,
		},
		{
			Key:     "links-management",
			Title:   "Preact links / arcane channels manager",
			Caption: "The bounded Links controls and keyed list retain the regular management vocabulary while wizard mode exposes channel, weave, rebind and sever copy.",
			JS: showGroups + `return (async function(){
  document.querySelector('.filter-bar-cog .cog-btn').click();
  document.querySelector('#links-manage-open').click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var host = document.querySelector('#links-feature-root[data-island-owner="links"]');
  var create = host && host.querySelector('#link-new-open');
  var empty = document.querySelector('#links-list .empty');
  if (!host || !create || !empty) throw new Error('links-management: bounded Links surface did not render');
  var wizard = document.body.classList.contains('wizard');
  var regularCopy = create.querySelector('.theme-copy-regular');
  var wizardCopy = create.querySelector('.theme-copy-wizard');
  var emptyRegular = empty.querySelector('.theme-copy-regular');
  var emptyWizard = empty.querySelector('.theme-copy-wizard');
  if (wizard) {
    if (getComputedStyle(wizardCopy).display === 'none' || wizardCopy.textContent.trim() !== '+ weave channel') throw new Error('links-management: wizard create copy missing');
    if (getComputedStyle(emptyWizard).display === 'none' || getComputedStyle(emptyRegular).display !== 'none') throw new Error('links-management: wizard empty copy missing');
  } else {
    if (getComputedStyle(regularCopy).display === 'none' || regularCopy.textContent.trim() !== '+ new link') throw new Error('links-management: regular create copy missing');
    if (getComputedStyle(emptyRegular).display === 'none' || getComputedStyle(emptyWizard).display !== 'none') throw new Error('links-management: regular empty copy missing');
  }
})();`,
			SettleMS: 250,
		},
		{
			Key:     "spawn-harness-policy",
			Title:   "Cross-harness spawn policy matrix",
			Caption: "The global policy matrix uses dashboard-native controls in the regular skin and becomes the cross-realm ward book, with matching vocabulary and arcane chrome, in wizard mode.",
			JS: showGroups + `return (async function(){
  document.querySelector('.filter-bar-cog .cog-btn').click();
  var launcher = document.querySelector('#spawn-harness-policy-open');
  var wizard = document.body.classList.contains('wizard');
  var regularCopy = launcher.querySelector('.theme-copy-regular');
  var wizardCopy = launcher.querySelector('.theme-copy-wizard');
  if (wizard) {
    if (getComputedStyle(wizardCopy).display === 'none' || getComputedStyle(regularCopy).display !== 'none') throw new Error('spawn-harness-policy: wizard cog copy missing');
  } else if (getComputedStyle(regularCopy).display === 'none' || getComputedStyle(wizardCopy).display !== 'none') {
    throw new Error('spawn-harness-policy: regular cog copy missing');
  }
  launcher.click();
  var deadline = Date.now() + 3000;
  while (!document.querySelector('#spawn-harness-policy-modal select') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  var modal = document.querySelector('#spawn-harness-policy-modal .cron-create-modal');
  var title = document.querySelector('#spawn-harness-policy-title');
  var select = document.querySelector('#spawn-harness-policy-modal select');
  if (!modal || !title || !select) throw new Error('spawn-harness-policy: matrix did not render');
  select.value = 'deny';
  select.dispatchEvent(new Event('change', {bubbles:true}));
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var textarea = document.querySelector('#spawn-harness-policy-modal textarea');
  if (!textarea) throw new Error('spawn-harness-policy: denial reason control did not render');
  var surface = getComputedStyle(modal);
  var control = getComputedStyle(select);
  var matrixWrap = document.querySelector('#spawn-harness-policy-modal .spawn-harness-matrix-wrap');
  var targetHeaders = Array.from(document.querySelectorAll('#spawn-harness-policy-modal thead th')).slice(1);
  var targetWidths = targetHeaders.map(function(header){ return Math.round(header.getBoundingClientRect().width); });
  if (getComputedStyle(modal).resize !== 'both') throw new Error('spawn-harness-policy: dialog is not resizable');
  if (matrixWrap.scrollWidth > matrixWrap.clientWidth + 1) throw new Error('spawn-harness-policy: two-harness matrix scrolls at its natural width');
  if (new Set(targetWidths).size !== 1) throw new Error('spawn-harness-policy: target columns differ: ' + targetWidths.join(', '));
  var colgroup = document.querySelector('#spawn-harness-policy-modal colgroup');
  var fakeCol = document.createElement('col');
  fakeCol.className = 'spawn-harness-target';
  colgroup.appendChild(fakeCol);
  var fakeHeader = document.createElement('th');
  fakeHeader.textContent = 'Third harness';
  document.querySelector('#spawn-harness-policy-modal thead tr').appendChild(fakeHeader);
  var fakeCells = [];
  document.querySelectorAll('#spawn-harness-policy-modal tbody tr').forEach(function(row){
    var cell = document.createElement('td');
    cell.className = 'spawn-harness-same';
    cell.textContent = 'always allowed';
    row.appendChild(cell);
    fakeCells.push(cell);
  });
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var manyWidths = Array.from(document.querySelectorAll('#spawn-harness-policy-modal thead th')).slice(1)
    .map(function(header){ return Math.round(header.getBoundingClientRect().width); });
  if (new Set(manyWidths).size !== 1) throw new Error('spawn-harness-policy: 3-harness target columns differ: ' + manyWidths.join(', '));
  if (matrixWrap.scrollWidth <= matrixWrap.clientWidth + 1) throw new Error('spawn-harness-policy: 3-harness matrix does not expose horizontal scrolling');
  fakeCells.forEach(function(cell){ cell.remove(); });
  fakeHeader.remove();
  fakeCol.remove();
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  if (wizard) {
    if (title.textContent.trim() !== 'Global cross-realm summons') throw new Error('spawn-harness-policy: wizard title missing');
    if (!surface.backgroundImage.includes('gradient')) throw new Error('spawn-harness-policy: wizard surface lacks gradient chrome');
    if (control.backgroundColor !== 'rgb(20, 15, 40)') throw new Error('spawn-harness-policy: wizard select is ' + control.backgroundColor);
  } else {
    if (title.textContent.trim() !== 'Global cross-harness spawn policy') throw new Error('spawn-harness-policy: regular title changed');
    if (surface.backgroundImage !== 'none') throw new Error('spawn-harness-policy: wizard chrome leaked into regular mode');
    if (control.backgroundColor !== 'rgb(13, 17, 23)') throw new Error('spawn-harness-policy: regular select is ' + control.backgroundColor);
  }
})();`,
			SettleMS: 250,
		},
		{
			Key:     "spawn-harness-policy-narrow",
			Title:   "Cross-harness spawn policy — narrow viewport",
			Caption: "At 560px the dialog remains inside the viewport while the fixed-width matrix becomes a usable horizontal scroll region.",
			Width:   560,
			Height:  900,
			JS: showGroups + `return (async function(){
  document.querySelector('.filter-bar-cog .cog-btn').click();
  document.querySelector('#spawn-harness-policy-open').click();
  var deadline = Date.now() + 3000;
  while (!document.querySelector('#spawn-harness-policy-modal select') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  var modal = document.querySelector('#spawn-harness-policy-modal .cron-create-modal');
  var matrixWrap = document.querySelector('#spawn-harness-policy-modal .spawn-harness-matrix-wrap');
  if (!modal || !matrixWrap) throw new Error('spawn-harness-policy-narrow: dialog did not render');
  if (window.innerWidth !== 560) throw new Error('spawn-harness-policy-narrow: viewport is ' + window.innerWidth);
  var rect = modal.getBoundingClientRect();
  if (rect.left < -1 || rect.right > window.innerWidth + 1) throw new Error('spawn-harness-policy-narrow: dialog escapes viewport');
  if (matrixWrap.clientWidth < 1 || matrixWrap.scrollWidth <= matrixWrap.clientWidth) throw new Error('spawn-harness-policy-narrow: matrix is not a usable scroll region');
})();`,
			SettleMS: 250,
		},
		{
			Key:      "bounded-costs-normal",
			Title:    "Bounded Preact — Costs normal",
			Caption:  "Costs island completed its request and rendered controls for the fixture's empty-cost span.",
			JS:       boundedTabJS("costs", "#costs-factor"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-usage-normal",
			Title:    "Bounded Preact — Usage forecasts",
			Caption:  "Subscription quota history renders provider/window line charts, a detected nonzero reset, and post-reset forecasts.",
			JS:       boundedTabJS("usage", ".usage-series-card"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-access-normal",
			Title:    "Bounded Preact — Access normal",
			Caption:  "Access island rendered its normal fixture state with keyboard-navigable sub-tabs.",
			JS:       boundedTabJS("access", ".access-subnav"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-logs-normal",
			Title:    "Bounded Preact — Logs normal",
			Caption:  "Logs island completed its request and rendered controls for the fixture's empty log.",
			JS:       boundedTabJS("logs", "#logs-refresh"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-audit-normal",
			Title:    "Bounded Preact — Audit normal",
			Caption:  "Audit island completed its request and rendered controls for the fixture's empty audit log.",
			JS:       boundedTabJS("audit", "#audit-outcome"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-config-normal",
			Title:    "Bounded Preact — Config normal",
			Caption:  "Config island completed its load and rendered the existing form semantics behind its Preact owner.",
			JS:       boundedTabJS("config", "#cfg-save"),
			SettleMS: 500,
		},
		{
			Key:      "bounded-jobs-empty",
			Title:    "Bounded Preact — Jobs empty filter",
			Caption:  "Representative empty state produced through the real Jobs filter input.",
			JS:       boundedJobsEmptyJS(),
			SettleMS: 500,
		},
		{
			Key:      "bounded-logs-error",
			Title:    "Bounded Preact — Logs error",
			Caption:  "Representative request failure rendered as an accessible alert.",
			JS:       boundedLogsErrorJS(),
			SettleMS: 300,
		},
		{
			Key:      "bounded-logs-poll-focus",
			Title:    "Bounded Preact — Logs focus across poll",
			Caption:  "Self-checked: filter focus, value, and selection survive a completed dashboard snapshot poll.",
			JS:       boundedLogsPollFocusJS(),
			SettleMS: 100,
		},
		{
			Key:     "bounded-config-dialog-focus",
			Title:   "Bounded Preact — Config dialog focus",
			Caption: "Self-checked: the diff dialog takes focus and a real Tab key remains contained inside it.",
			JS:      boundedConfigDialogJS(),
			Actions: []dashsnap.BrowserAction{
				{Kind: "key", Key: "tab"},
				{Kind: "eval", JS: `var d=document.querySelector('#config-diff-modal'); if(!d || !d.contains(document.activeElement)) throw new Error('Tab focus escaped Config dialog');`},
			},
			SettleMS: 200,
		},
		{
			Key:      "action-dialog-clone-agent",
			Title:    "Action dialogs — clone agent",
			Caption:  "Preact-owned clone dialog preserves its worktree picker, copy-history default, and resize grip while using scoped plain or wizard form chrome.",
			JS:       actionDialogJS(`module.openCloneAgentDialog("f1000000-0000-4000-8000-000000000001", "fe-lead", "/tmp/lbl-fe-lead");`, "#clone-agent-modal", ""),
			SettleMS: 500,
		},
		{
			Key:     "action-dialog-reincarnate-force",
			Title:   "Action dialogs — force reincarnate",
			Caption: "Preact-owned reincarnate dialog in its destructive force mode, including controlled mode switching, required handoff copy, and busy-ready submit chrome.",
			JS: actionDialogJS(`module.openReincarnateAgentDialog("f1000000-0000-4000-8000-000000000001", "fe-lead");`, "#reincarnate-agent-modal", `
  var force = document.querySelector('#reincarnate-agent-modal input[value="force"]');
  force.click();
  await Promise.resolve();
  var followup = document.querySelector('#reincarnate-agent-followup');
  followup.value = 'Continue the Preact dashboard migration from the current worktree.';
  followup.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText' }));`),
			SettleMS: 300,
		},
		{
			Key:      "action-dialog-nest-group",
			Title:    "Action dialogs — nest group",
			Caption:  "Preact-owned group nesting dialog preserves the parent selector, explanatory copy, focus boundary, and wizard-compatible scoped chrome.",
			JS:       actionDialogJS(`module.openNestGroupDialog({group: "infra-crew"});`, "#group-nest-modal", ""),
			SettleMS: 300,
		},
		{
			Key:      "processes-templates",
			Title:    "Processes — templates",
			Caption:  "Feature-gated Processes tab with a populated versioned template list, dark-themed actions, and the library-scoped Edit-with-agent entry point.",
			JS:       processTabJS("templates", `[data-process-template="release-train"]`),
			SettleMS: 900,
		},
		{
			Key:     "processes-template-description-edit",
			Title:   "Processes — inline description edit",
			Caption: "TCL-600: the Templates list edits a template description in place, with the click-to-edit affordance swapped for a focused, skin-themed editor.",
			JS:      processTabJS("templates", `[data-process-template="release-train"]`),
			// `return` matters, and so does the frame wait: the editor swaps in
			// through a Preact state update, so a synchronous read right after
			// the click always misses it. See processTabJS.
			Actions: []dashsnap.BrowserAction{{Kind: "eval", JS: `return (async function(){
  var edit=document.querySelector('[data-process-description-edit="release-train"]');
  if(!edit) throw new Error('inline description affordance missing');
  if(!edit.getAttribute('aria-label')) throw new Error('inline description affordance has no accessible name');
  edit.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input=document.querySelector('[data-process-description-input="release-train"]');
  if(!input) throw new Error('clicking the description did not open an editor');
  if(document.activeElement!==input) throw new Error('the description editor did not take focus');
})();`}},
			SettleMS: 900,
		},
		{
			Key:     "processes-scribe-library-entry",
			Title:   "Processes — library scribe entry",
			Caption: "TCL-434 capstone: the Processes list exposes one visible library-scoped scribe action with skin-coherent language before any template is opened.",
			JS:      processTabJS("templates", `#process-scribe-library`),
			Actions: []dashsnap.BrowserAction{{Kind: "eval", JS: `var button=document.querySelector('#process-scribe-library');
  if(!button) throw new Error('library-scoped process scribe action missing');
  var rect=button.getBoundingClientRect();
  if(rect.width<80||rect.height<24) throw new Error('library-scoped process scribe action is not meaningfully visible');
  var plain=button.querySelector('.process-scribe-plain'),wizard=button.querySelector('.process-scribe-wizard');
  var active=document.body.classList.contains('wizard')?wizard:plain;
  var inactive=document.body.classList.contains('wizard')?plain:wizard;
  if(!active||getComputedStyle(active).display==='none') throw new Error('active-skin process scribe language is hidden');
  if(!inactive||getComputedStyle(inactive).display!=='none') throw new Error('inactive-skin process scribe language leaked');
  if(document.body.classList.contains('wizard')&&!active.textContent.includes('process scribe')) throw new Error('wizard process scribe language missing');
  if(!document.body.classList.contains('wizard')&&!active.textContent.includes('Edit with agent')) throw new Error('plain process scribe language missing');`}},
			SettleMS: 900,
		},
		{
			Key:      "processes-runs",
			Title:    "Processes — runs",
			Caption:  "Processes Runs sub-view with a populated live run row, status, current activity, and viewer action.",
			JS:       processTabJS("runs", `[data-process-run="dashsnap-release-42"]`),
			SettleMS: 900,
		},
		{
			Key:      "process-viewer-rich",
			Title:    "Processes — rich checkpoint view",
			Caption:  "Schema-7 checkpoint view with exact pinned topology, current routing overlays, paginated detail tabs, authority boundary, and a sanitized timeline.",
			JS:       processViewerStateJS("dashsnap-parallel-any-9", true),
			SettleMS: 1200,
		},
		{
			Key:      "process-viewer-legacy-unavailable",
			Title:    "Processes — legacy routing unavailable",
			Caption:  "Legacy run view fails closed: exact pinned topology remains visible while the routing overlay and checkpoint detail tables explicitly report their unavailable state.",
			JS:       processViewerStateJS("dashsnap-approve-7", false),
			SettleMS: 1200,
		},
		{
			Key:      "process-viewer-epoch-summary",
			Title:    "Processes — schema-8 safe summary",
			Caption:  "Schema-8 run renders the honest epoch_v8_summary restriction, the adaptation summary (lineage, structural totals, authority state chips, bounded timeline), and the memory-only unlock draft panel; exact topology stays restricted.",
			JS:       processViewerEpochStateJS("dashsnap-epoch-8"),
			SettleMS: 1200,
		},
		{
			Key:      "management-profiles",
			Title:    "Management — spawn profiles",
			Caption:  "Preact-owned spawn-profile manager with filtering, transfer actions, keyed cards, and create/edit/delete entry points.",
			JS:       managementModalJS("/static/js/modal-profiles.js", "openProfilesManageModal", "#profiles-manage-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-profile-editor",
			Title:    "Management — new spawn profile",
			Caption:  "Spawn-profile editor with harness-driven fields, tri-state defaults, permissions boundary, validation, and save lifecycle.",
			JS:       managementModalJS("/static/js/modal-profiles.js", "openProfileEditor", "#profile-editor-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-roles",
			Title:    "Management — role library",
			Caption:  "Preact-owned role library with stable role cards and canonical brief/launch/permission editing.",
			JS:       managementModalJS("/static/js/modal-roles.js", "openRolesManageModal", "#roles-manage-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-role-editor",
			Title:    "Management — new role",
			Caption:  "Role editor with nested permission controls and stable spawn-profile references.",
			JS:       managementModalJS("/static/js/modal-roles.js", "openRoleEditor", "#role-editor-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-sandbox-profiles",
			Title:    "Management — sandbox profiles",
			Caption:  "Preact-owned sandbox-policy manager with redacted capability cards, import/export, and the sandbox-scribe boundary.",
			JS:       managementModalJS("/static/js/sandbox-profiles.js", "openSandboxProfilesManageModal", "#sandbox-profiles-manage-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-sandbox-editor",
			Title:    "Management — new sandbox profile",
			Caption:  "Structured filesystem/environment policy editor with raw JSON escape hatch, dry-run confirmation, and save-in-flight state.",
			JS:       managementModalJS("/static/js/sandbox-profiles.js", "openSandboxProfileEditor", "#sandbox-profile-editor-modal"),
			SettleMS: 700,
		},
		{
			Key:      "management-sandbox-editor-exclusion-help",
			Title:    "Management — sandbox exclusion help",
			Caption:  "Compact filesystem-restriction rows — checkbox plus short label — with one row's [?] disclosure expanded over its description, warning, audited paths, and provenance.",
			JS:       sandboxExclusionHelpJS(),
			SettleMS: 700,
		},
		{
			Key:      "processes-worklist",
			Title:    "Processes — worklist (My work)",
			Caption:  "Worklist My-work view: a decision row with the nudge schedule line, advertised approve/reject actions with the required comment input, the actionable-count sub-nav badge, and the amber degraded-runs strip.",
			JS:       worklistTabJS("my-work", `#process-panel-worklist .wl-row`),
			SettleMS: 900,
		},
		{
			Key:      "processes-worklist-waiting",
			Title:    "Processes — worklist (Waiting on)",
			Caption:  "Worklist Waiting-on view grouped by whom the work waits on: 👤 operator and 👤 oncall group heads over their pending items.",
			JS:       worklistTabJS("waiting-on", `#process-panel-worklist .wl-group-head`),
			SettleMS: 900,
		},
		{
			Key:      "processes-worklist-empty-view",
			Title:    "Processes — worklist (empty view)",
			Caption:  "Worklist Needs-review view with no matching items: the per-view empty state counts pending items in other views, and the degraded strip stays visible (unreadable runs are never silently dropped).",
			JS:       worklistTabJS("review", `#process-panel-worklist .process-placeholder`),
			SettleMS: 900,
		},
		{
			Key:      "process-editor-palette",
			Title:    "Process editor — palette open",
			Caption:  "Template editor over release-train: header (version badge, undo/redo/save), palette dock with primitives + snippets, graph canvas, inspector hint strip.",
			JS:       processEditorStateJS(``),
			SettleMS: 1100,
		},
		processEditorLayeringState("process-editor-layering", "Process editor — interaction layering"),
		processConnectionFeedbackState(
			"process-editor-connection-valid",
			"Process editor — valid connection target",
			".process-node-ports[data-node-id=\"begin\"] .process-port-out",
			".process-node-ports[data-node-id=\"ship\"] .process-port-in",
			"valid", "Drop to connect begin to ship.",
		),
		processConnectionFeedbackState(
			"process-editor-connection-invalid",
			"Process editor — invalid connection target reason",
			".process-node-ports[data-node-id=\"begin\"] .process-port-in",
			".process-node-ports[data-node-id=\"ship\"] .process-port-in",
			"invalid", "Connect this input to an output port or another node body.",
		),
		{
			Key:     "process-editor-conditional-overlays",
			Title:   "Process editor — conditional node information",
			Caption: "TCL-565: clean nodes have no decorative top-right circle while a real validation diagnostic keeps its glyph, tooltip, node-level accessible disclosure, selection behavior, and unchanged connector geometry in regular and wizard skins.",
			JS: processEditorStateJS(`var beforeSave=JSON.stringify(ed.model.saveBody());
  var layoutGeometry=function(){var layout=ed.graph.layoutSnapshot();return {bounds:layout.bounds,nodes:layout.nodes.map(function(n){return {id:n.id,x:n.x,y:n.y,width:n.width,height:n.height,layer:n.layer,pinned:n.pinned};}),edges:layout.edges.map(function(e){return {id:e.id,from:e.from,to:e.to,path:e.path,label:e.label};})};};
  var beforeGeometry=JSON.stringify(layoutGeometry());
  var portGeometry=function(id){return Array.from(document.querySelectorAll('.process-node-ports[data-node-id="'+id+'"] .process-port')).map(function(port){return [port.dataset.port,port.getAttribute('cx'),port.getAttribute('cy'),port.getAttribute('r'),port.getAttribute('role'),port.getAttribute('tabindex'),port.getAttribute('aria-label')];});};
  var beforePorts=JSON.stringify({start:portGeometry('begin'),end:portGeometry('ship')});
  if(document.querySelector('.process-overlay-anchor')) throw new Error('clean editor rendered an empty overlay placeholder');
  var diagnostic={severity:'error',code:'E_BEGIN',scope:'node',targetId:'begin',message:'Beginning requires operator review'};
  ed.validation.applyDiagnostics([diagnostic]);
  await editorPaint();
  var begin=document.querySelector('.process-node[data-node-id="begin"]');
  var ship=document.querySelector('.process-node[data-node-id="ship"]');
  var marker=begin&&begin.querySelector('.process-overlay-anchor');
  if(!marker||!marker.classList.contains('has-overlay')) throw new Error('diagnostic overlay anchor missing');
  if(ship&&ship.querySelector('.process-overlay-anchor')) throw new Error('clean sibling gained an overlay anchor');
  if(marker.getAttribute('aria-hidden')!=='true'||marker.hasAttribute('role')||marker.hasAttribute('tabindex')) throw new Error('overlay became a separate accessibility action');
  if(!begin.getAttribute('aria-label').includes('E_BEGIN: Beginning requires operator review')) throw new Error('node accessible diagnostic missing');
  var tooltip=marker.querySelector('.process-overlay-tooltip');
  if(!tooltip||!tooltip.textContent.includes('Beginning requires operator review')) throw new Error('diagnostic tooltip missing');
  var ringStyle=getComputedStyle(marker.querySelector('.process-overlay-ring'));
  if(ringStyle.strokeDasharray!=='none') throw new Error('populated overlay retained placeholder dashes: '+ringStyle.strokeDasharray);
  if(JSON.stringify(layoutGeometry())!==beforeGeometry) throw new Error('overlay changed graph layout geometry');
  if(JSON.stringify({start:portGeometry('begin'),end:portGeometry('ship')})!==beforePorts) throw new Error('overlay changed connector geometry or accessibility');
  marker.dispatchEvent(new MouseEvent('click',{bubbles:true}));
  if(ed.selection?.type!=='node'||ed.selection.id!=='begin'||!begin.classList.contains('is-selected')) throw new Error('overlay click stopped selecting its node');
  begin.focus();
  if(document.activeElement!==begin||getComputedStyle(tooltip).display==='none') throw new Error('node focus stopped disclosing its diagnostic tooltip');
  ed.validation.applyDiagnostics([]);
  await editorPaint();
  if(document.querySelector('.process-overlay-anchor')) throw new Error('cleared diagnostic left an overlay anchor');
  ed.validation.applyDiagnostics([diagnostic]);
  await editorPaint();
  if(!document.querySelector('.process-node[data-node-id="begin"] .process-overlay-anchor')) throw new Error('diagnostic overlay did not restore for capture');
  if(JSON.stringify(ed.model.saveBody())!==beforeSave) throw new Error('overlay lifecycle changed the template round trip');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-inside-labels",
			Title:   "Process editor — bounded labels inside every node",
			Caption: "TCL-566: all six editor node kinds keep long and Unicode names inside fixed semantic shapes, with readable overflow and both connector-port bands unobscured in regular and wizard skins.",
			JS: processEditorStateJS(`var specs = [
	    {id:'start-label',type:'start',name:'Start WWWWWWW 起点',x:130,y:120},
	    {id:'task-label',type:'task',name:'WWWWWWWWWWWWWWWWW Implement-国際化🙂withoutspaces-and-extra-detail-bounded-overflow',x:380,y:120},
	    {id:'decision-label',type:'decision',name:'WWWWWWWWWWWW レビュー結果を確認しますか',x:690,y:135},
	    {id:'parallel-label',type:'parallel',name:'並列分岐🙂long-name',x:150,y:390},
	    {id:'wait-label',type:'wait',name:'Wait for signal 待機',x:420,y:390},
	    {id:'end-label',type:'end',name:'Done 完了',x:690,y:390}
  ];
  ed.model.template.nodes = Object.fromEntries(specs.map(function(spec){ return [spec.id,{type:spec.type,name:spec.name}]; }));
  ed.model.template.start = 'start-label';
  ed.model.layout.nodes = Object.fromEntries(specs.map(function(spec){ return [spec.id,{x:spec.x,y:spec.y}]; }));
  ed.model.edges = [
    {from:'',outcome:'start',to:'start-label'},
    {from:'start-label',outcome:'next',to:'task-label'},
    {from:'task-label',outcome:'review',to:'decision-label'},
    {from:'decision-label',outcome:'split',to:'parallel-label'},
    {from:'parallel-label',outcome:'wait',to:'wait-label'},
    {from:'wait-label',outcome:'done',to:'end-label'}
  ];
  ed.refresh({fit:true});
  await editorPaint(); await editorPaint();
  var rgb = function(value) {
    var parts = String(value).match(/[\d.]+/g);
    if (!parts || parts.length < 3) throw new Error('unparseable graph colour: '+value);
    return parts.slice(0,3).map(Number);
  };
  var luminance = function(value) {
    return rgb(value).map(function(channel){ channel/=255; return channel<=.04045?channel/12.92:Math.pow((channel+.055)/1.055,2.4); })
      .reduce(function(total,channel,index){ return total+channel*[.2126,.7152,.0722][index]; },0);
  };
  var contrast = function(a,b) { var x=luminance(a),y=luminance(b); return (Math.max(x,y)+.05)/(Math.min(x,y)+.05); };
	  specs.forEach(function(spec) {
    var node = document.querySelector('.process-node[data-node-id="'+spec.id+'"]');
    var ports = document.querySelector('.process-node-ports[data-node-id="'+spec.id+'"]');
    var shape = node && node.querySelector('.process-node-shape');
    var label = node && node.querySelector('.process-node-label-inside');
    var clip = node && node.querySelector('.process-node-label-clip rect');
    var input = ports && ports.querySelector('.process-port-in');
    var output = ports && ports.querySelector('.process-port-out');
    if (!node||!ports||!shape||!label||!clip||!input||!output) throw new Error(spec.type+' inside-label fixture incomplete');
    if (node.querySelector('.process-node-label-peripheral')) throw new Error(spec.type+' restored a peripheral label');
    if (!node.getAttribute('aria-label').startsWith(spec.name+', '+spec.type)) throw new Error(spec.type+' lost its full accessible name');
    if (input.getAttribute('aria-label')!=='Input port for '+spec.name||output.getAttribute('aria-label')!=='Output port for '+spec.name) throw new Error(spec.type+' port accessible name changed');
    var shapeBox=shape.getBBox(),x=Number(clip.getAttribute('x')),y=Number(clip.getAttribute('y')),
      width=Number(clip.getAttribute('width')),height=Number(clip.getAttribute('height'));
    if (x<shapeBox.x-.1||x+width>shapeBox.x+shapeBox.width+.1||y<shapeBox.y-.1||y+height>shapeBox.y+shapeBox.height+.1) throw new Error(spec.type+' label frame escaped the fixed shape bbox');
	    var inBottom=Number(input.getAttribute('cy'))+Number(input.getAttribute('r'));
	    var outTop=Number(output.getAttribute('cy'))-Number(output.getAttribute('r'));
	    if (y<=inBottom||y+height>=outTop) throw new Error(spec.type+' label frame overlaps an input/output port band');
	    if (label.getAttribute('clip-path')!=='url(#'+clip.parentElement.id+')') throw new Error(spec.type+' label is not hard-clipped to its verified interior frame');
	    Array.from(label.querySelectorAll('tspan')).forEach(function(line) {
	      if (line.getComputedTextLength()>width+.5) throw new Error(spec.type+' line relies on incidental clip overflow: '+line.textContent);
	    });
	    var ratio=contrast(getComputedStyle(label).fill,getComputedStyle(shape).fill);
    if (ratio<4.5) throw new Error(spec.type+' label contrast is '+ratio.toFixed(2)+':1');
  });
  ['start-label','end-label'].forEach(function(id) {
    var node=document.querySelector('.process-node[data-node-id="'+id+'"]'),clip=node.querySelector('.process-node-label-clip rect'),ports=document.querySelector('.process-node-ports[data-node-id="'+id+'"]');
    if (Number(clip.getAttribute('y'))<=Number(ports.querySelector('.process-port-in').getAttribute('cy'))+6) throw new Error(id+' smallest-shape input band is not clear');
    if (Number(clip.getAttribute('y'))+Number(clip.getAttribute('height'))>=Number(ports.querySelector('.process-port-out').getAttribute('cy'))-6) throw new Error(id+' smallest-shape output band is not clear');
  });`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-commands",
			Title:   "Process editor — contextual commands",
			Caption: "TCL-435: the shared dashboard command palette contributes selection-aware graph operations, searchable plain copy, disabled reasons, and the documented editor launcher.",
			JS: processEditorStateJS(`ed.setSelection({type: 'node', id: 'begin'});
  ed.openCommands();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input = document.querySelector('#palette-input');
  if (!input || !document.querySelector('#command-palette-modal.show')) throw new Error('process command palette did not open');
  input.value = 'selection'; input.dispatchEvent(new InputEvent('input', {bubbles:true}));
  if (!document.querySelector('.palette-item[aria-disabled="false"]')) throw new Error('enabled selection command missing');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-scribe-selection-preview",
			Title:   "Process editor — scribe selection preview",
			Caption: "TCL-434 capstone: selection handoff shows an editable request beside bounded, read-only stable graph identity before the scoped scribe is reused or summoned.",
			JS: processEditorStateJS(`ed.setSelection({type: 'node', id: 'begin'});
  void ed.requestScribe('selection');
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var preview = document.querySelector('#process-scribe-preview-modal');
  if (!preview || !preview.classList.contains('show')) throw new Error('process scribe selection preview did not open');
  if (!preview.querySelector('.process-scribe-prompt')) throw new Error('editable scribe request missing');
  var context = preview.querySelector('.process-scribe-context-preview');
  if (!context || !context.textContent.includes('current-selection') || !context.textContent.includes('begin')) {
    throw new Error('bounded stable selection identity missing from scribe preview');
  }`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-edge-drop-chooser",
			Title:   "Process editor — connector-drop chooser",
			Caption: "TCL-433: releasing a connector on empty canvas anchors the searchable, keyboard-accessible complete node vocabulary at the intended graph coordinate.",
			JS: processEditorStateJS(`var r=document.querySelector('.process-graph-svg').getBoundingClientRect();
  var client={clientX:r.left+r.width*0.72,clientY:r.top+r.height*0.64};
  var p=ed.graph.clientToGraph(client.clientX,client.clientY);
  ed.openConnectedNodeChooser({nodeId:'begin',port:'out'},p,client);
  await Promise.resolve();
  var chooser=document.querySelector('.process-node-chooser');
  if(!chooser||document.activeElement!==chooser.querySelector('.process-node-chooser-input')) throw new Error('edge-drop chooser did not open or focus');
  var commandIDs=Array.from(chooser.querySelectorAll('[role="option"]')).map(function(option){return option.dataset.commandId;}).sort();
  var expectedIDs=['process.create.decision','process.create.end','process.create.parallel','process.create.start','process.create.task','process.create.wait'];
  if(JSON.stringify(commandIDs)!==JSON.stringify(expectedIDs)) throw new Error('edge-drop chooser vocabulary incomplete: '+JSON.stringify(commandIDs));`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-browser-input-edge-drop",
			Title:   "Process editor — trusted input-port edge drop",
			Caption: "Real Chrome input: an input-port pointer drag releases on native-hit-tested empty canvas, disables impossible source types, opens required task configuration, and undo removes the node plus edge atomically.",
			JS: processEditorStateJS(`window.__browserEd=ed;
  await editorPaint();
  var svg=document.querySelector('.process-graph-svg');
  var port=document.querySelector('.process-node-ports[data-node-id="ship"] .process-port-in');
  if(!svg||!port) throw new Error('input-port browser fixture is missing');
  var svgRect=svg.getBoundingClientRect(),nativeElementFromPoint=document.elementFromPoint.bind(document);
  var drop=null;
  for(var yi=2;yi<=8&&!drop;yi+=1){
    for(var xi=2;xi<=8;xi+=1){
      var candidate={x:svgRect.left+svgRect.width*xi/10,y:svgRect.top+svgRect.height*yi/10};
      var hit=nativeElementFromPoint(candidate.x,candidate.y);
      if(hit&&svg.contains(hit)&&!hit.closest('[data-node-id],[data-edge-index],[data-port]')){drop=candidate;break;}
    }
  }
  if(!drop) throw new Error('no native-hit-tested empty canvas point found');
  window.__edgeDrop={drop:drop,nodes:Object.keys(ed.model.template.nodes).length,edges:ed.model.edges.length,undo:ed.model.undoStack.length,hitTests:0,lastHit:null};
  document.elementFromPoint=function(x,y){
    var hit=nativeElementFromPoint(x,y);window.__edgeDrop.hitTests+=1;window.__edgeDrop.lastHit=hit;return hit;
  };`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down-at", JS: `var r=document.querySelector('.process-node-ports[data-node-id="ship"] .process-port-in').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`},
				{Kind: "move-to-at", JS: `return window.__edgeDrop.drop;`, Steps: 6},
				{Kind: "mouse-up"},
				{Kind: "eval", JS: `var state=window.__edgeDrop,chooser=document.querySelector('.process-node-chooser');
  if(state.hitTests!==1||!state.lastHit||!document.querySelector('.process-graph-svg').contains(state.lastHit)) throw new Error('pointerup did not use elementFromPoint on the SVG');
  if(!chooser) throw new Error('trusted empty-canvas pointerup did not open the chooser');
  var end=chooser.querySelector('[data-command-id="process.create.end"]');
  if(!end||end.getAttribute('aria-disabled')!=='true'||!end.textContent.includes('cannot have outgoing edges')) throw new Error('input direction did not clearly disable End');
  var hint=end.querySelector('.process-node-chooser-hint'),hintStyle=getComputedStyle(hint),optionStyle=getComputedStyle(end),surfaceStyle=getComputedStyle(chooser);
  if(optionStyle.opacity!=='1'||hintStyle.opacity!=='1') throw new Error('disabled reason is opacity-dimmed');
  var rgb=function(value){var parts=value.match(/[\d.]+/g).slice(0,3).map(Number);return parts;};
  var luminance=function(value){return rgb(value).map(function(channel){channel/=255;return channel<=.04045?channel/12.92:Math.pow((channel+.055)/1.055,2.4);}).reduce(function(total,channel,index){return total+channel*[.2126,.7152,.0722][index];},0);};
  var foreground=luminance(hintStyle.color),background=luminance(surfaceStyle.backgroundColor),contrast=(Math.max(foreground,background)+.05)/(Math.min(foreground,background)+.05);
  if(contrast<4.5) throw new Error('disabled reason contrast is '+contrast.toFixed(2)+':1');`},
				{Kind: "click", Selector: `.process-node-chooser [data-command-id="process.create.task"]`},
				{Kind: "eval", JS: `var ed=window.__browserEd,state=window.__edgeDrop;
  if(!document.querySelector('.process-node-modal .process-node-detail')) throw new Error('configuration-required task editor did not open');
  if(Object.keys(ed.model.template.nodes).length!==state.nodes+1||ed.model.edges.length!==state.edges+1) throw new Error('node and edge were not both created');
  if(ed.model.undoStack.length!==state.undo+1) throw new Error('connected insertion was not one undo operation');
  var created=Object.keys(ed.model.template.nodes).find(function(id){return !['begin','ship'].includes(id);});
  var edge=ed.model.edges.find(function(candidate){return candidate.from===created&&candidate.to==='ship';});
  if(!created||ed.model.node(created).type!=='task'||!edge) throw new Error('input-port drop did not create new-source → existing-target direction');
  window.__edgeDrop.created=created;`},
				{Kind: "click", Selector: ".process-node-modal .process-node-cancel"},
				{Kind: "click", Selector: `.process-editor-header button[title="Undo (Ctrl+Z)"]`},
				{Kind: "eval", JS: `var ed=window.__browserEd,state=window.__edgeDrop;
  if(document.querySelector('.process-node-modal')) throw new Error('configuration editor did not close');
  if(ed.model.node(state.created)||Object.keys(ed.model.template.nodes).length!==state.nodes||ed.model.edges.length!==state.edges) throw new Error('one undo did not remove the connected node and edge');
  if(ed.model.undoStack.length!==state.undo) throw new Error('atomic undo did not restore the prior history depth');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-commands-wizard",
			Title:   "Process editor — contextual commands (wizard)",
			Caption: "The same TCL-435 command registry under the wizard skin: arcane labels remain searchable by ordinary vocabulary and disabled context stays visibly explained.",
			Wizard:  true,
			JS: processEditorStateJS(`ed.setSelection(null);
  ed.openCommands();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input = document.querySelector('#palette-input');
  if (!input || !document.querySelector('#command-palette-modal.show')) throw new Error('wizard process command palette did not open');
  input.value = 'selection'; input.dispatchEvent(new InputEvent('input', {bubbles:true}));
  if (!document.querySelector('.palette-item.disabled[aria-disabled="true"]')) throw new Error('wizard disabled selection reason missing');`),
			SettleMS: 1100,
		},
		{
			Key:      "process-editor-selected",
			Title:    "Process editor — node selected",
			Caption:  "A selected node: accent outline on the canvas and the inspector strip showing type, id, label input, and dark-themed action buttons.",
			JS:       processEditorStateJS(`ed.setSelection({type: 'node', id: 'begin'});`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-browser-node-click",
			Title:   "Process editor — trusted node click + Delete",
			Caption: "Real Chrome input: an inspector edit commits on blur, the next node click remains selected after pointer capture retargeting, and Delete confirms/removes that node.",
			JS: processEditorStateJS(`ed.setSelection({type:'node',id:'begin'}); window.__browserEd=ed;
  await editorPaint();
  var input=document.querySelector('.process-editor-inspector .process-inspector-input');
  if(!input) throw new Error('node inspector input missing');
  input.value='';`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "click", Selector: ".process-inspector-input"},
				{Kind: "input", Selector: ".process-inspector-input", Text: "Renamed begin"},
				{Kind: "click", Selector: `.process-node[data-node-id="ship"] .process-node-shape`},
				{Kind: "eval", JS: `var ed=window.__browserEd;
  var selection=ed.snapshot().selection;
  if(selection?.type!=='node'||selection.id!=='ship') throw new Error('trusted node click did not select ship');
  if(!document.querySelector('[data-node-id="ship"].is-selected')) throw new Error('ship highlight missing');
  if(ed.model.node('begin').name!=='Renamed begin') throw new Error('inspector blur change did not commit');`},
				{Kind: "key", Key: "Delete"},
				{Kind: "eval", JS: `if(!document.querySelector('.process-editor-modal')) throw new Error('Delete did not open node confirmation');`},
				{Kind: "click", Selector: ".process-editor-modal .confirm-danger"},
				{Kind: "eval", JS: `if(window.__browserEd.model.node('ship')) throw new Error('confirmed node Delete did not remove ship');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-focus-contract",
			Title:   "Process editor — frame skipped + exact dialog focus",
			Caption: "Real Chrome input: sequential Tab skips the editor frame/SVG/sink, dialog cancel restores the exact graph item, and an editor opened from body focus returns to body.",
			JS:      processEditorStateJS(`window.__focusEd=ed; await editorPaint();`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "eval", JS: `var root=document.querySelector('.process-graph'),svg=root.querySelector('.process-graph-svg'),sink=root.querySelector('.process-graph-keyboard-sink');
  if(root.hasAttribute('tabindex')) throw new Error('editor frame retained a tabindex');
  if(svg.getAttribute('tabindex')!=='-1'||svg.getAttribute('focusable')!=='false') throw new Error('SVG viewport is not excluded from traversal');
  if(!sink||sink.getAttribute('tabindex')!=='-1') throw new Error('programmatic shortcut sink contract missing');
  var controls=Array.from(document.querySelectorAll('.process-editor-palette button')).filter(function(button){return !button.disabled&&button.offsetParent!==null;});
  if(!controls.length) throw new Error('no visible palette control before the canvas');
  controls[controls.length-1].focus();window.__focusFrame={root:root,svg:svg,sink:sink};`},
				{Kind: "key", Key: "Tab"},
				{Kind: "eval", JS: `var state=window.__focusFrame,active=document.activeElement;
  if(active===state.root||active===state.svg||active===state.sink) throw new Error('Tab landed on the editor frame, SVG, or shortcut sink');
  if(!state.root.contains(active)) throw new Error('Tab did not enter an element inside the graph');
  var node=document.querySelector('.process-node[data-node-id="begin"]');node.focus();window.__focusInvoker=node;`},
				{Kind: "key", Key: "Delete"},
				{Kind: "eval", JS: `if(!document.querySelector('#process-editor-choice-modal')) throw new Error('Delete confirmation did not open');`},
				{Kind: "key", Key: "Escape"},
				{Kind: "eval", JS: `if(document.activeElement!==window.__focusInvoker) throw new Error('dialog cancel did not restore the exact graph item');
  document.activeElement.blur();if(document.activeElement!==document.body) throw new Error('fixture could not establish body focus');
  window.__bodyFocusChoice=window.__focusEd.choiceModal({title:'Body focus check',body:'Restore no editor focus.',choices:[{key:'apply',label:'Apply',primary:true}]});`},
				{Kind: "key", Key: "Escape"},
				{Kind: "eval", JS: `if(document.querySelector('#process-editor-choice-modal')) throw new Error('body-focus dialog did not close');
  if(document.activeElement!==document.body) throw new Error('dialog teardown invented an editor focus target');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-copy-paste",
			Title:   "Process editor — trusted cursor-centered copy/paste",
			Caption: "TCL-569 real Chrome input in regular and wizard skins: native clipboard ownership survives while cursor paste uses the current pan/zoom, same-target repeats cascade, and pointer exit falls back to canvas center.",
			JS: processEditorStateJS(`window.__browserEd=ed;
  ed.setSelection({type:'multi',items:[{type:'node',id:'begin'},{type:'node',id:'ship'}]});
  await editorPaint();
	  ed.graph.resetZoom();ed.graph.zoomBy(1.65);ed.graph.centerOn(310.25,250.5);
	  var transformed=ed.graph.viewSnapshot();
	  if(Math.abs(transformed.k-1.65)>.001||(Math.abs(transformed.x)<.01&&Math.abs(transformed.y)<.01)) throw new Error('clipboard fixture did not establish non-default pan/zoom: '+JSON.stringify(transformed));
  var begin=document.querySelector('.process-node[data-node-id="begin"]');
  if(!begin) throw new Error('clipboard fixture node missing');
  begin.focus({preventScroll:true});
  if(document.activeElement!==begin) throw new Error('clipboard fixture did not take graph focus');
  var title=document.querySelector('.process-editor-title'),range=document.createRange(),selection=getSelection();
  window.__nativeCopyText=title.textContent;range.selectNodeContents(title);selection.removeAllRanges();selection.addRange(range);
  window.__clipboardBefore={nodes:Object.keys(ed.model.template.nodes).length,edges:ed.model.edges.length,undo:ed.model.undoStack.length,dirty:ed.model.dirty};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "c"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var probe=document.createElement('textarea');probe.id='clipboard-native-probe';document.body.appendChild(probe);getSelection().removeAllRanges();probe.focus();`},
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "v"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var probe=document.querySelector('#clipboard-native-probe');
  if(probe.value!==window.__nativeCopyText) throw new Error('highlighted read-only text lost native clipboard ownership');
	  var ed=window.__browserEd,before=window.__clipboardBefore;
	  if(Object.keys(ed.model.template.nodes).length!==before.nodes||ed.model.edges.length!==before.edges||ed.model.undoStack.length!==before.undo||ed.model.dirty!==before.dirty) throw new Error('native clipboard path mutated editor history/save state');
  probe.remove();var begin=document.querySelector('.process-node[data-node-id="begin"]');begin.focus({preventScroll:true});`},
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "c"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var status=document.querySelector('.process-editor-status')?.textContent||'';
				  if(!status.includes('Copied 2 nodes')) throw new Error('trusted graph shortcut did not copy the selected nodes: '+status);
				  var svg=document.querySelector('.process-graph-svg'),r=svg.getBoundingClientRect();
				  window.__clipboardCursor={x:r.left+r.width*.71,y:r.top+r.height*.37};`},
				{Kind: "move-to-at", JS: `return window.__clipboardCursor;`, Steps: 5},
				{Kind: "eval", JS: `var ed=window.__browserEd,p=window.__clipboardCursor;
				  if(!ed.canvasPointer) throw new Error('trusted canvas move was not observed');
				  window.__clipboardTarget=ed.graph.clientToGraph(p.x,p.y);`},
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "v"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var ed=window.__browserEd,target=window.__clipboardTarget,svg=document.querySelector('.process-graph-svg'),r=svg.getBoundingClientRect(),view=ed.graph.viewSnapshot();
				  window.__clipboardCursor={x:r.left+view.x+target.x*view.k,y:r.top+view.y+target.y*view.k};`},
				{Kind: "move-to-at", JS: `return window.__clipboardCursor;`, Steps: 3},
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "v"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var ed=window.__browserEd,before=window.__clipboardBefore,target=window.__clipboardTarget;
  for(var id of ['begin-2','ship-2','begin-3','ship-3']) if(!ed.model.node(id)) throw new Error('repeated paste missing '+id);
  if(!ed.model.findEdge('begin-2','pass')||ed.model.findEdge('begin-2','pass').to!=='ship-2') throw new Error('first paste did not remap its internal edge');
  if(!ed.model.findEdge('begin-3','pass')||ed.model.findEdge('begin-3','pass').to!=='ship-3') throw new Error('second paste did not remap its internal edge');
	  var center=function(ids){var nodes=ed.graph.layoutSnapshot().nodes.filter(function(node){return ids.includes(node.id);});return {x:(Math.min.apply(null,nodes.map(function(n){return n.x-n.width/2;}))+Math.max.apply(null,nodes.map(function(n){return n.x+n.width/2;})))/2,y:(Math.min.apply(null,nodes.map(function(n){return n.y-n.height/2;}))+Math.max.apply(null,nodes.map(function(n){return n.y+n.height/2;})))/2};};
	  var rendered=center(['begin-2','ship-2']);
	  if(Math.abs(rendered.x-target.x)>.01||Math.abs(rendered.y-target.y)>.01) throw new Error('first paste rendered-node bounds are not centered at trusted cursor');
  var first=ed.model.layout.nodes['begin-2'],second=ed.model.layout.nodes['begin-3'];
	  if(Math.abs(second.x-first.x-36)>.01||Math.abs(second.y-first.y-36)>.01) throw new Error('repeated paste did not cascade by 36px: '+JSON.stringify({first:first,second:second,anchor:ed.pasteAnchor,repeat:ed.pasteRepeat,pointer:ed.canvasPointer,view:ed.graph.viewSnapshot(),rect:document.querySelector('.process-graph-svg').getBoundingClientRect()}));
	  var r=document.querySelector('.process-graph-svg').getBoundingClientRect();window.__clipboardFallback=ed.graph.canvasCenter();window.__clipboardOutside={x:Math.max(2,r.left-16),y:r.top+r.height/2};`},
				{Kind: "move-to-at", JS: `return window.__clipboardOutside;`, Steps: 4},
				{Kind: "eval", JS: `if(window.__browserEd.canvasPointer) throw new Error('pointer exit did not invalidate cursor authority');`},
				{Kind: "key-down", Key: clipboardModifier},
				{Kind: "key", Key: "v"},
				{Kind: "key-up", Key: clipboardModifier},
				{Kind: "eval", JS: `var ed=window.__browserEd,before=window.__clipboardBefore,fallback=window.__clipboardFallback;
	  for(var id of ['begin-4','ship-4']) if(!ed.model.node(id)) throw new Error('fallback paste missing '+id);
	  if(Object.keys(ed.model.template.nodes).length!==before.nodes+6) throw new Error('cursor/repeat/fallback paste node count drifted');
	  if(ed.model.edges.length!==before.edges+3) throw new Error('only internal copied edges should be pasted');
	  if(!ed.model.findEdge('begin-4','pass')||ed.model.findEdge('begin-4','pass').to!=='ship-4') throw new Error('fallback paste did not remap its internal edge');
	  var nodes=ed.graph.layoutSnapshot().nodes.filter(function(node){return ['begin-4','ship-4'].includes(node.id);});
	  var rendered={x:(Math.min.apply(null,nodes.map(function(n){return n.x-n.width/2;}))+Math.max.apply(null,nodes.map(function(n){return n.x+n.width/2;})))/2,y:(Math.min.apply(null,nodes.map(function(n){return n.y-n.height/2;}))+Math.max.apply(null,nodes.map(function(n){return n.y+n.height/2;})))/2};
	  if(Math.abs(rendered.x-fallback.x)>.01||Math.abs(rendered.y-fallback.y)>.01) throw new Error('pointer-exit paste did not reset at visible canvas center');
	  if(ed.model.undoStack.length!==before.undo+3) throw new Error('each paste was not one history transaction');
	  var selected=ed.snapshot().selection?.items||[];
	  if(selected.length!==2||!selected.some(function(item){return item.id==='begin-4';})||!selected.some(function(item){return item.id==='ship-4';})) throw new Error('fallback paste did not select its new nodes');
	  if(document.activeElement?.dataset?.nodeId!=='begin-4') throw new Error('fallback paste did not restore graph focus');
	  var status=document.querySelector('.process-editor-status')?.textContent||'';
	  if(!status.includes('Pasted 2 nodes')) throw new Error('trusted Ctrl-V status missing');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-snippet-name-input",
			Title:   "Process editor — themed snippet name input",
			Caption: "TCL-570 real Chrome proof: the accessible snippet-name field uses the active dashboard skin across placeholder, hover, focus-visible, populated, disabled and invalid states, with computed contrast assertions.",
			JS: processEditorStateJS(`window.__browserEd=ed;
  await ed.loadCustomSnippets();
  ed.setSelection({type:'multi',items:[{type:'node',id:'begin'},{type:'node',id:'ship'}]});
  await editorPaint();`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "eval", JS: `var save=document.querySelector('.process-palette-save');
  if(!save||save.disabled) throw new Error('snippet save action unavailable: '+JSON.stringify(window.__browserEd.snapshot().snippets));
  save.scrollIntoView({block:'center'});`},
				{Kind: "click", Selector: ".process-palette-save"},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input'),modal=document.querySelector('#process-snippet-name-modal .modal');
	if(!input) throw new Error('snippet name dialog did not open');
	if(!modal) throw new Error('snippet name dialog surface missing');
  var label=modal.querySelector('label[for="process-snippet-name-input"]');
  if(!label||input.getAttribute('aria-describedby')!=='process-snippet-name-help process-snippet-name-error') throw new Error('snippet field accessible naming contract drifted');
  if(input.placeholder!=='e.g. Release review'||input.autocomplete!=='off') throw new Error('snippet field placeholder/autocomplete contract drifted');
  input.blur();
  var rgb=function(value){var parts=(value.match(/[\d.]+/g)||[]).slice(0,3).map(Number);if(parts.length!==3) throw new Error('unparseable color '+value);return parts;};
  var luminance=function(value){return rgb(value).map(function(channel){channel/=255;return channel<=.04045?channel/12.92:Math.pow((channel+.055)/1.055,2.4);}).reduce(function(total,channel,index){return total+channel*[.2126,.7152,.0722][index];},0);};
  window.__snippetContrast=function(a,b){var x=luminance(a),y=luminance(b);return (Math.max(x,y)+.05)/(Math.min(x,y)+.05);};
	var style=getComputedStyle(input),placeholder=getComputedStyle(input,'::placeholder');
	if(style.backgroundColor==='rgb(255, 255, 255)'||style.backgroundColor==='rgba(0, 0, 0, 0)') throw new Error('snippet field fell back to UA background');
	if(window.__snippetContrast(style.color,style.backgroundColor)<4.5) throw new Error('snippet field text contrast below 4.5:1');
	if(window.__snippetContrast(placeholder.color,style.backgroundColor)<4.5) throw new Error('snippet placeholder contrast below 4.5:1');
	if(window.__snippetContrast(style.borderColor,style.backgroundColor)<3||window.__snippetContrast(style.borderColor,getComputedStyle(modal).backgroundColor)<3) throw new Error('snippet normal boundary contrast below 3:1');
	window.__snippetModalBackground=getComputedStyle(modal).backgroundColor;
	window.__snippetNormalBorder=style.borderColor;`},
				{Kind: "move-to-at", JS: `var r=document.querySelector('#process-snippet-name-input').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`, Steps: 3},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input');
	var style=getComputedStyle(input);
	if(style.borderColor===window.__snippetNormalBorder) throw new Error('snippet field hover affordance missing');
	if(window.__snippetContrast(style.borderColor,style.backgroundColor)<3||window.__snippetContrast(style.borderColor,window.__snippetModalBackground)<3) throw new Error('snippet hover boundary contrast below 3:1');`},
				{Kind: "click", Selector: "#process-snippet-name-input"},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input'),style=getComputedStyle(input),modal=getComputedStyle(document.querySelector('#process-snippet-name-modal .modal'));
  if(document.activeElement!==input||style.outlineStyle==='none'||parseFloat(style.outlineWidth)<2) throw new Error('snippet focus-visible ring missing');
  if(window.__snippetContrast(style.outlineColor,modal.backgroundColor)<3) throw new Error('snippet focus ring contrast below 3:1');`},
				{Kind: "input", Selector: "#process-snippet-name-input", Text: "Release review Release review Release review Release review Release review Release review Release review"},
				{Kind: "click", Selector: "#process-snippet-name-modal .primary"},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input'),error=document.querySelector('#process-snippet-name-error');
	if(document.activeElement===input||input.getAttribute('aria-invalid')!=='true'||!error.textContent.includes('80 characters')) throw new Error('blurred snippet invalid state missing');
	window.__snippetInvalidBorder=getComputedStyle(input).borderColor;`},
				{Kind: "move-to-at", JS: `var r=document.querySelector('#process-snippet-name-input').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`, Steps: 3},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input');
	if(getComputedStyle(input).borderColor!==window.__snippetInvalidBorder) throw new Error('hover overrode the blurred snippet invalid border');`},
				{Kind: "click", Selector: "#process-snippet-name-input"},
				{Kind: "eval", JS: `var input=document.querySelector('#process-snippet-name-input'),error=document.querySelector('#process-snippet-name-error'),style=getComputedStyle(input),errorStyle=getComputedStyle(error),modal=getComputedStyle(document.querySelector('#process-snippet-name-modal .modal'));
	if(input.getAttribute('aria-invalid')!=='true'||!error.textContent.includes('80 characters')) throw new Error('snippet invalid/inline-error state missing');
  if(style.borderColor===window.__snippetNormalBorder||style.outlineColor!==style.borderColor) throw new Error('snippet invalid focus treatment missing');
  if(window.__snippetContrast(style.outlineColor,modal.backgroundColor)<3) throw new Error('snippet invalid focus contrast below 3:1');
  if(window.__snippetContrast(errorStyle.color,errorStyle.backgroundColor)<4.5) throw new Error('snippet inline error contrast below 4.5:1');
  input.disabled=true;var disabled=getComputedStyle(input);
  if(disabled.backgroundColor==='rgb(255, 255, 255)'||window.__snippetContrast(disabled.color,disabled.backgroundColor)<4.5) throw new Error('snippet disabled state is unthemed or below 4.5:1');
  input.disabled=false;input.focus();input.setSelectionRange(input.value.length,input.value.length);
  if(input.selectionStart!==input.value.length||input.selectionEnd!==input.value.length) throw new Error('native snippet caret/selection behavior drifted');
  var rect=input.getBoundingClientRect(),modalRect=document.querySelector('#process-snippet-name-modal .modal').getBoundingClientRect();
  if(rect.width<240||rect.left<modalRect.left||rect.right>modalRect.right) throw new Error('snippet input does not fit its zoom-safe modal field');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-custom-snippets",
			Title:   "Process editor — custom snippet library",
			Caption: "TCL-567 real Chrome input in regular and wizard skins: a named selected subgraph persists through the authenticated library, inserts through the canonical remap/history path, then revision-CAS rename/delete leave built-ins stable.",
			JS: processEditorStateJS(`window.__browserEd=ed;
  await ed.loadCustomSnippets();
  ed.setSelection({type:'multi',items:[{type:'node',id:'begin'},{type:'node',id:'ship'}]});
  await editorPaint();
  var saveSnippet=document.querySelector('.process-palette-save');
  if(!saveSnippet||saveSnippet.disabled) throw new Error('custom snippet save action unavailable: '+JSON.stringify(ed.snapshot().snippets));
  window.__customBefore={nodes:Object.keys(ed.model.template.nodes).length,edges:ed.model.edges.length,undo:ed.model.undoStack.length,builtins:document.querySelectorAll('.process-palette-card.is-built-in').length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "eval", JS: `document.querySelector('.process-palette-save').scrollIntoView({block:'center'});`},
				{Kind: "click", Selector: ".process-palette-save"},
				{Kind: "eval", JS: `if(!document.querySelector('#process-snippet-name-input')) throw new Error('custom snippet name dialog did not open; status='+(document.querySelector('.process-editor-status')?.textContent||''));`},
				{Kind: "input", Selector: "#process-snippet-name-input", Text: "Browser reusable"},
				{Kind: "click", Selector: "#process-snippet-name-modal .primary"},
				{Kind: "eval", JS: `return new Promise(function(resolve,reject){var stop=Date.now()+3000;(function poll(){
  var card=document.querySelector('.process-palette-card.is-custom');
  if(card&&card.textContent.includes('Browser reusable')) return resolve(true);
  if(Date.now()>stop) return reject(new Error('custom snippet create did not settle'));
  setTimeout(poll,25);})();});`},
				{Kind: "click", Selector: ".process-palette-card.is-custom .process-palette-insert"},
				{Kind: "eval", JS: `var ed=window.__browserEd,before=window.__customBefore;
  if(Object.keys(ed.model.template.nodes).length!==before.nodes+2) throw new Error('custom insert did not add two nodes');
  if(ed.model.edges.length!==before.edges+1) throw new Error('custom insert did not retain exactly the internal edge');
  if(ed.model.undoStack.length!==before.undo+1) throw new Error('custom insert was not one history transaction');
  var selected=ed.snapshot().selection?.items||[];
  if(selected.length!==2) throw new Error('custom insert did not select remapped nodes');
  if(document.activeElement?.dataset?.nodeId!==selected[0].id&&document.activeElement?.dataset?.nodeId!==selected[1].id) throw new Error('custom insert did not restore graph focus');`},
				{Kind: "click", Selector: ".process-palette-card.is-custom .process-palette-manage:not(.process-palette-delete)"},
				{Kind: "input", Selector: "#process-snippet-name-input", Text: "Browser renamed"},
				{Kind: "click", Selector: "#process-snippet-name-modal .primary"},
				{Kind: "eval", JS: `return new Promise(function(resolve,reject){var stop=Date.now()+3000;(function poll(){
  var card=document.querySelector('.process-palette-card.is-custom');
  if(card&&card.textContent.includes('Browser renamed')) return resolve(true);
  if(Date.now()>stop) return reject(new Error('custom snippet rename did not settle'));
  setTimeout(poll,25);})();});`},
				{Kind: "click", Selector: ".process-palette-card.is-custom .process-palette-delete"},
				{Kind: "click", Selector: "#process-editor-choice-modal .confirm-danger"},
				{Kind: "eval", JS: `return new Promise(function(resolve,reject){var stop=Date.now()+3000;(function poll(){
  if(!document.querySelector('.process-palette-card.is-custom')) return resolve(true);
  if(Date.now()>stop) return reject(new Error('custom snippet delete did not settle'));
  setTimeout(poll,25);})();});`},
				{Kind: "eval", JS: `if(document.querySelectorAll('.process-palette-card.is-built-in').length!==window.__customBefore.builtins) throw new Error('custom lifecycle mutated built-in palette');
  if(!document.querySelector('.process-palette-state')?.textContent.includes('No custom snippets')) throw new Error('custom empty state missing after delete');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-edge-click",
			Title:   "Process editor — trusted edge click + Delete",
			Caption: "Real Chrome input: pointer capture still resolves the clicked connector, opens its edge inspector, and confirmed Delete removes exactly that edge.",
			JS:      processEditorStateJS(`window.__browserEd=ed; ed.setSelection(null);`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "click", Selector: ".process-edge .process-edge-hit"},
				{Kind: "eval", JS: `var ed=window.__browserEd;
  var selection=ed.snapshot().selection;
  if(selection?.type!=='edge') throw new Error('trusted edge click did not select an edge');
  if(!document.querySelector('.process-editor-inspector .process-inspector-kind')?.textContent.includes('edge')) throw new Error('edge inspector missing');
  window.__edgeKey=[selection.from,selection.outcome];`},
				{Kind: "key", Key: "Delete"},
				{Kind: "eval", JS: `if(!document.querySelector('.process-editor-modal')) throw new Error('Delete did not open edge confirmation');`},
				{Kind: "click", Selector: ".process-editor-modal .confirm-danger"},
				{Kind: "eval", JS: `var ed=window.__browserEd,k=window.__edgeKey;
          if(ed.model.findEdge(k[0],k[1])) throw new Error('confirmed edge Delete did not remove clicked edge');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-modifier-click",
			Title:   "Process editor — trusted modifier multi-select",
			Caption: "Real Chrome input: Ctrl-click toggles a node and connector into the same selection while marquee remains node-only.",
			JS:      processEditorStateJS(`window.__browserEd=ed; ed.setSelection(null);`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "click", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "eval", JS: `return new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); }).then(function(){
  var selection=window.__browserEd.snapshot().selection;
  if(selection?.type!=='node'||selection.id!=='begin') throw new Error('trusted node click did not settle before modifier gesture');
});`},
				{Kind: "key-down", Key: "Control"},
				{Kind: "click", Selector: ".process-edge .process-edge-hit"},
				{Kind: "key-up", Key: "Control"},
				{Kind: "eval", JS: `var selection=window.__browserEd.snapshot().selection,items=selection?.items||[];
  if(selection?.type!=='multi'||items.length!==2) throw new Error('trusted modifier click did not create two-item selection');
  if(document.querySelectorAll('.process-graph-svg .is-selected').length!==2) throw new Error('multi-selection highlights out of sync');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-drag-live",
			Title:   "Process editor — live multi-drag connectors",
			Caption: "Real Chrome drag held mid-frame: both selected nodes move, their internal connector/label/arrow geometry follows, and the model remains unmodified until release.",
			JS: processEditorStateJS(`window.__browserEd=ed; var items=ed.graph.layoutSnapshot().nodes.map(function(n){return {type:'node',id:n.id};}); ed.setSelection({type:'multi',items:items});
  window.__dragBefore={path:document.querySelector('.process-edge-path').getAttribute('d'),
    begin:document.querySelector('.process-node[data-node-id="begin"]').getAttribute('transform'),
    ship:document.querySelector('.process-node[data-node-id="ship"]').getAttribute('transform'),
    rev:ed.model.rev,undo:ed.model.undoStack.length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 120, DY: 65, Steps: 6},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
  if(!ed.graph.interactionSnapshot().active) throw new Error('drag interaction is not active mid-frame');
  if(document.querySelector('.process-edge-path').getAttribute('d')===b.path) throw new Error('connector stayed frozen mid-drag');
  if(document.querySelector('.process-node[data-node-id="begin"]').getAttribute('transform')===b.begin) throw new Error('primary selected node stayed frozen mid-drag');
  if(document.querySelector('.process-node[data-node-id="ship"]').getAttribute('transform')===b.ship) throw new Error('second selected node stayed frozen mid-drag');
  if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('drag frame mutated model/undo');`},
			},
			SettleMS: 100,
		},
		{
			Key:     "process-editor-browser-drag-commit",
			Title:   "Process editor — atomic drag commit",
			Caption: "Real Chrome drag release commits the selected nodes once, renders node/connector geometry from the committed adapter layout, and consumes exactly one undo slot.",
			JS: processEditorStateJS(`window.__browserEd=ed; var layout=ed.graph.layoutSnapshot(),items=layout.nodes.map(function(n){return {type:'node',id:n.id};}); ed.setSelection({type:'multi',items:items});
  var begin=layout.nodes.find(function(node){return node.id==='begin';}),ship=layout.nodes.find(function(node){return node.id==='ship';});
  window.__dragBefore={path:document.querySelector('.process-edge-path').getAttribute('d'),begin:{x:begin.x,y:begin.y},ship:{x:ship.x,y:ship.y},rev:ed.model.rev,undo:ed.model.undoStack.length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 100, DY: 55, Steps: 5},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('pre-release model mutation');`},
				{Kind: "mouse-up"},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
  if(ed.model.undoStack.length!==b.undo+1) throw new Error('drag release was not one atomic undo step');
  if(ed.graph.interactionSnapshot().active) throw new Error('drag interaction survived release');
  var layout=ed.graph.layoutSnapshot(),begin=layout.nodes.find(function(node){return node.id==='begin';}),ship=layout.nodes.find(function(node){return node.id==='ship';});
  if((begin.x===b.begin.x&&begin.y===b.begin.y)||(ship.x===b.ship.x&&ship.y===b.ship.y)) throw new Error('drag release did not commit every selected node');
  if(document.querySelector('.process-node[data-node-id="begin"]').getAttribute('transform')!=='translate('+begin.x+' '+begin.y+')') throw new Error('begin node DOM diverged from committed adapter layout');
  if(document.querySelector('.process-node[data-node-id="ship"]').getAttribute('transform')!=='translate('+ship.x+' '+ship.y+')') throw new Error('ship node DOM diverged from committed adapter layout');
  if(document.querySelector('.process-edge-path').getAttribute('d')===b.path) throw new Error('connector did not reroute from the committed layout');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-drag-cancel",
			Title:   "Process editor — cancelled drag restore",
			Caption: "A browser drag followed by pointercancel restores node and connector geometry completely and leaves model revision/undo untouched.",
			JS: processEditorStateJS(`window.__browserEd=ed; window.__dragBefore={
    path:document.querySelector('.process-edge-path').getAttribute('d'),
    transform:document.querySelector('.process-node[data-node-id="begin"]').getAttribute('transform'),
    rev:ed.model.rev,undo:ed.model.undoStack.length};
  window.__dragPointerID=null;
  document.querySelector('.process-graph-svg').addEventListener('pointerdown',function(event){window.__dragPointerID=event.pointerId;},{once:true,capture:true});`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 90, DY: 50, Steps: 5},
				{Kind: "eval", JS: `var ed=window.__browserEd;
  if(!ed.graph.interactionSnapshot().active) throw new Error('drag interaction missing before cancel');
  if(window.__dragPointerID==null) throw new Error('drag pointer id was not observed');
  document.querySelector('.process-graph-svg').dispatchEvent(new PointerEvent('pointercancel',{pointerId:window.__dragPointerID,bubbles:true}));`},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
  if(ed.graph.interactionSnapshot().active) throw new Error('drag interaction survived cancel');
  if(document.querySelector('.process-edge-path').getAttribute('d')!==b.path) throw new Error('cancel did not restore connector');
  if(document.querySelector('.process-node[data-node-id="begin"]').getAttribute('transform')!==b.transform) throw new Error('cancel did not restore node');
  if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('cancel mutated model/undo');`},
			},
			SettleMS: 300,
		},
		{
			Key:      "process-editor-legacy-edge-error-row",
			Title:    "Process editor — legacy edge rejection with a maximal unbroken outcome",
			Caption:  "TCL-583 recovery guidance in real Chrome: an edge outcome may legally be 512 unbroken characters, and the full-width error row must wrap it inside the editor mount so the trailing recovery clause stays visible without any pointer-only tooltip.",
			JS:       processEditorLegacyEdgeErrorRowJS,
			SettleMS: 1100,
		},
		{
			Key:      "process-editor-legacy-edge-error-row-wizard",
			Title:    "Process editor — legacy edge rejection error row (wizard)",
			Caption:  "The same TCL-583 error row under the wizard skin, which restyles the header but must not override the error row's containment.",
			Wizard:   true,
			JS:       processEditorLegacyEdgeErrorRowJS,
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-marquee-multi",
			Title:   "Process editor — marquee multi-selection",
			Caption: "A left-drag marquee selects several nodes at once; every selected node has an accent outline and the inspector summarizes the current set.",
			JS: processEditorStateJS(`var items = ed.graph.layoutSnapshot().nodes.map(function(node){ return {type:'node',id:node.id}; }); ed.setSelection({type:'multi',items:items});
  await editorPaint();
  var box = document.createElementNS('http://www.w3.org/2000/svg','rect'); box.setAttribute('class','process-marquee'); box.setAttribute('x','40'); box.setAttribute('y','20'); box.setAttribute('width','430'); box.setAttribute('height','360'); document.querySelector('.process-graph-viewport').append(box);
  if(document.querySelectorAll('.process-node.is-selected').length!==items.length) throw new Error('marquee selection highlights are incomplete');
  if(!document.querySelector('.process-editor-inspector')?.textContent.includes(items.length+' items')) throw new Error('marquee selection inspector summary missing');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-template-settings",
			Title:   "Process editor — template name mid-edit",
			Caption: "Template metadata editor mid-rename: immutable id plus a focused, changed-but-uncommitted display name alongside description and documentation.",
			JS: processEditorStateJS(`ed.setSelection({type: 'template'});
  await editorPaint();
  var nameInput = document.querySelector('[aria-label="Template display name"]');
  if (!nameInput) throw new Error('template display-name input missing');
  nameInput.value = 'Release train — renamed';
  nameInput.focus();
  nameInput.select();`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-params",
			Title:   "Process editor — template params",
			Caption: "TCL-300 params editor: parameter names, free-form types, descriptions, explicit defaults, and required state in one atomic editor transaction.",
			JS: processEditorStateJS(`await ed.openParamsSettings();
  if (!document.querySelector('.process-param-dialog [data-process-param="issue"]')) throw new Error('params dialog did not open');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-instantiate-dialog",
			Title:   "Process template — instantiate exact version",
			Caption: "TCL-300 instantiate dialog: exact content-addressed ref plus required string, defaulted number, and defaulted boolean inputs.",
			JS: processEditorStateJS(`await ed.requestInstantiate();
  if (!document.querySelector('.process-instantiate-dialog [data-process-param-input="issue"]')) throw new Error('instantiate dialog did not open');`),
			SettleMS: 1100,
		},
		{
			Key:      "process-editor-dirty",
			Title:    "Process editor — dirty",
			Caption:  "After adding a task node and pinning a move: the ● modified badge lights, Save arms, and undo becomes available.",
			JS:       processEditorStateJS(`ed.model.addNode('task', {x: 470, y: 120, name: 'Review'}); ed.model.moveNode('begin', 140, 260); ed.refresh({fit: true});`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-scribe-dirty-guard",
			Title:   "Process editor — scribe dirty guard",
			Caption: "Edit-with-agent on a dirty editor requires an explicit Save first or Discard local edits choice; Cancel remains available and no external authoring starts behind the dialog.",
			JS: processEditorStateJS(`ed.model.addNode('task', {x: 470, y: 120, name: 'Local draft'}); ed.refresh({fit: true});
  void ed.requestScribe(); await Promise.resolve();
  if (!document.querySelector('.process-editor-modal')) throw new Error('scribe dirty guard did not open');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-external-clean",
			Title:   "Process editor — attributed clean agent change",
			Caption: "TCL-437: a clean editor shows the exact agent-authored version plus a concise graph/source review and a non-destructive Apply update action.",
			JS: processEditorStateJS(`var external = ed.model.saveBody();
  external.template.description = 'dashsnap external clean ' + Date.now();
  var response = await fetch('/v1/process/templates/release-train', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(external)});
  var saved = await response.json();
  if (!response.ok || !saved.ref) throw new Error('external clean save failed: ' + JSON.stringify(saved));
  ed.observeExternalHead(saved);
  await ed.loadExternalReview();
  ed.observeExternalHead = function(){ return this.externalChange; };
  ed.externalChange.actor = 'agent:agt_11111111111111111111111111111111';
  ed.toggleExternalReview(); await editorPaint();
  if (ed.externalChange.kind !== 'clean' || !ed.externalChange.review) throw new Error('clean external review state missing');
  if (!document.querySelector('.process-external-review') || !document.querySelector('.process-editor-external .primary')?.textContent.includes('Apply update')) throw new Error('clean external review controls missing');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-external-dirty",
			Title:   "Process editor — conflicting agent change",
			Caption: "TCL-437: a dirty editor is never overwritten; review explains the agent change and the CAS-backed Reload & discard / Keep editing decision.",
			JS: processEditorStateJS(`var external = ed.model.saveBody();
  external.template.description = 'dashsnap external dirty ' + Date.now();
  var response = await fetch('/v1/process/templates/release-train', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(external)});
  var saved = await response.json();
  if (!response.ok || !saved.ref) throw new Error('external dirty save failed: ' + JSON.stringify(saved));
  ed.model.addNode('task', {x: 470, y: 120, name: 'Local draft'}); ed.refresh({fit: true});
  ed.observeExternalHead(saved);
  await ed.loadExternalReview();
  ed.observeExternalHead = function(){ return this.externalChange; };
  ed.externalChange.actor = 'agent:agt_22222222222222222222222222222222';
  ed.toggleExternalReview(); await editorPaint();
  var externalButtons=Array.from(document.querySelectorAll('.process-editor-external button'));
  if (ed.externalChange.kind !== 'dirty' || !ed.externalChange.review || !externalButtons.some(function(button){return button.textContent.includes('Keep editing');})) throw new Error('dirty external review actions missing');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-wizard",
			Title:   "Process editor — wizard skin",
			Caption: "The same editor (palette + selection + dirty) under the wizard skin: violet chrome, gold accents, explicitly themed cards and controls.",
			Wizard:  true,
			JS: processEditorStateJS(`ed.model.addNode('task', {x: 470, y: 120, name: 'Review'}); ed.refresh({fit: true});
  ed.setSelection({type: 'template'});`),
			SettleMS: 1100,
		},
		{
			Key:      "process-node-dialog-task-agent",
			Title:    "Process node dialog — task, agent work performer",
			Caption:  "The compound task's stage editor (TCL-298): plan stage with approval policy, agent work performer (profile/prompt/model/effort + contact schedule), ordered checks, review gate, retry policy, captures, and the read-only edges summary.",
			JS:       nodeDialogStateJS(`await ed.openNodeSettings('implement'); await editorPaint();` + nodeDialogSelfCheck("agent")),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-resized",
			Title:   "Process node dialog — resized workspace",
			Caption: "TCL-419: the standard persisted resize affordance expands the compound task editor into a two-column workspace; the scroll body and fixed action row remain usable in both skins.",
			JS: nodeDialogStateJS(`await ed.openNodeSettings('implement'); await editorPaint();` + nodeDialogSelfCheck("agent") + `
  var dialog = document.querySelector('.process-node-dialog');
  if (getComputedStyle(dialog).resize !== 'both') throw new Error('node dialog resize affordance is not active');
  dialog.style.width = '1000px'; dialog.style.height = '760px';
  var detail = dialog.querySelector('.process-node-detail');
  if (getComputedStyle(detail).columnCount !== '2') throw new Error('wide node form did not reflow into columns');`),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-task-human",
			Title:   "Process node dialog — task, human work performer",
			Caption: "The same shared performer editor keyed to human: ask text, choices, assignee — scrolled to the work section. No per-kind component forks (uniform performer contract).",
			JS: nodeDialogStateJS(`ed.model.updateNode('implement', function(n){
    n.performer = {kind: 'human', profile: 'operator', ask: 'Apply the manual registry update', choices: ['done', 'blocked'], assignee: 'johan'};
  });
  await ed.openNodeSettings('implement'); await editorPaint();` + nodeDialogSelfCheck("human") + nodeDialogScrollToWork),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-task-program",
			Title:   "Process node dialog — task, program work performer",
			Caption: "The shared performer editor keyed to program: command + per-line arguments and the explicit ⚠ command-execution security note (§10) — scrolled to the work section.",
			JS: nodeDialogStateJS(`ed.model.updateNode('implement', function(n){
    n.performer = {kind: 'program', profile: 'ci', run: 'go', args: ['test', './...']};
  });
  await ed.openNodeSettings('implement'); await editorPaint();` + nodeDialogSelfCheck("program") + `
  if (!document.querySelector('.process-node-security-note')) throw new Error('program security note missing');` + nodeDialogScrollToWork),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-decision",
			Title:   "Process node dialog — decision node",
			Caption: "Decision node dialog: the decider performer (human, with choices) and the read-only choices → edges mapping pointing at the canvas for topology edits.",
			JS: nodeDialogStateJS(`await ed.openNodeSettings('escalate'); await editorPaint();` + `
  var choiceHead = Array.from(document.querySelectorAll('.process-node-section-title')).find(function(el){ return el.textContent === 'choices → edges'; });
  if (!choiceHead) throw new Error('decision dialog missing the choices → edges section');`),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-card-readonly",
			Title:   "Process node detail card — read-only mode",
			Caption: "The exact same component in view mode (the viewer's node detail card): read-only badge, every control disabled, zero duplicated markup — the §9 unlock later flips this flag back to edit.",
			JS: nodeDialogStateJS(`ed.model.config.nodeEditable = function(){ return false; };
  await ed.openNodeSettings('implement'); await editorPaint();
  if (!document.querySelector('.process-node-readonly-badge')) throw new Error('read-only badge missing');
  if (!document.querySelector('.process-node-detail.is-readonly')) throw new Error('detail card is not in read-only mode');
  var enabled = document.querySelector('.process-node-detail select:not(:disabled), .process-node-detail input:not(:disabled), .process-node-detail textarea:not(:disabled)');
  if (enabled) throw new Error('read-only card left a control enabled: ' + enabled.className);`),
			SettleMS: 1200,
		},
		{
			Key:     "process-editor-validation",
			Title:   "Process editor — live validation",
			Caption: "Live validation (TCL-299/397): an orphaned task carries the ✕ error badge with an ×3 count, the extra 'later' start edge gets a ⚠ dead-edge badge at its label, and keyboard focus exposes the node-local diagnostic detail while the issues panel lists every finding.",
			// The edits go through the editor's real refresh() choke point, so the
			// badges come from a genuine debounce → POST /v1/process/validate →
			// decorate round against the daemon, not injected fixtures.
			JS: processEditorStateJS(`ed.model.addNode('task', {x: 470, y: 40, name: 'Orphan'});
  ed.model.addEdge('begin', 'later', 'ship');
  ed.refresh({fit: true});
  var vDeadline = Date.now() + 6000;
  var vReady = function() {
    return document.querySelector('.process-overlay-anchor.overlay-error')
      && document.querySelector('.process-edge-badge-warning')
      && document.querySelector('.process-issues-list .process-issue');
  };
  while (!vReady() && Date.now() < vDeadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 60); });
  }
  if (!vReady()) throw new Error('validation badges/panel did not render');
  ed.validation.panel.open = true;
		document.querySelector('.process-issues-list .process-issue').click();
		document.querySelector('.process-node[data-node-id="task"]').focus();`),
			SettleMS: 2500,
		},
		{
			Key:     "groups",
			Title:   "Groups tab",
			Caption: "Groups tab, members expanded: the task-force info card (mission/roles) atop frontend-squad, its 🎯 hide-info toggle in the action row, tf: chips, owner ★, online + offline, task links.",
			JS:      showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open');`,
		},
		{
			Key:     "groups-wizard",
			Title:   "Groups tab — wizard",
			Caption: "The same viewport in wizard mode: native party shells, activity familiars, force quest card, group actions, hierarchy, and member-table adapter preserve the established re-skin.",
			JS: showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open', 'wizard');
document.dispatchEvent(new CustomEvent('tclaude:wizard', {detail:{active:true}}));`,
			SettleMS: 300,
		},
		{
			Key:     "task-link-editor",
			Title:   "Task link editor",
			Caption: "The operator's Task/Quest editor: an existing short link stays navigable, its hover/focus pencil opens the prefilled URL + optional display-name dialog, and wizard mode applies the quill/violet/parchment treatment.",
			JS: showGroups + expandGroups + `document.body.classList.remove('dock-open');` + `return (async function(){
  var edit = document.querySelector('.task-edit-icon[data-current]');
  if (!edit) throw new Error('task-link-editor: populated task edit control missing');
  edit.click();
  // The task-link dialog is now Preact-owned, so its markup lands on the next
  // render rather than synchronously with the click.
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var modal = document.querySelector('#task-link-modal.show');
  var url = document.querySelector('#task-link-url');
  var label = document.querySelector('#task-link-label');
  if (!modal || !url || !label) throw new Error('task-link-editor: dialog missing');
  if (!url.value.startsWith('http')) throw new Error('task-link-editor: URL was not prefilled');
  if (!label.value) throw new Error('task-link-editor: explicit label was not prefilled');
})();`,
			SettleMS: 250,
		},
		{
			Key:     "groups-view-menu",
			Title:   "Groups tab — Preact view controls",
			Caption: "TCL-357 (self-checked): the Preact-owned view popover preserves every visibility and member-column control, with the same default/wizard styling and native checkbox behavior.",
			JS: showGroups + collapseGroups + `document.body.classList.remove('dock-open');` + `return (async function(){
  var button = document.querySelector('#filter-groups-view-btn');
  if (!button) throw new Error('groups-view: trigger missing');
  button.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var menu = document.querySelector('#filter-groups-view-menu.open');
  if (!menu) throw new Error('groups-view: menu did not open');
  if (button.getAttribute('aria-expanded') !== 'true') throw new Error('groups-view: trigger aria-expanded is stale');
  if (menu.querySelectorAll(':scope > label.filter-toggle').length !== 6) throw new Error('groups-view: visibility toggles missing');
  if (!menu.querySelector('#filter-groups-cols input[type="checkbox"]')) throw new Error('groups-view: member-column toggles missing');
})();`,
		},
		{
			Key:     "groups-inline-editor",
			Title:   "Groups tab — native inline editor boundary",
			Caption: "TCL-465 (self-checked): the native description editor replaces only its keyed trigger while the group shell stays connected and its summary parks DnD.",
			JS: showGroups + expandGroups + `document.body.classList.add('dock-open');` + `return (async function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  var chip = det && det.querySelector(':scope > summary .group-descr');
  if (!chip) throw new Error('groups-inline-editor: description chip missing');
  chip.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input = det.querySelector(':scope > summary .group-descr-input');
  if (!input) throw new Error('groups-inline-editor: input did not open');
  if (chip.isConnected) throw new Error('groups-inline-editor: native trigger remained as a second writer');
  if (!det.isConnected) throw new Error('groups-inline-editor: stable group shell was detached');
  if (det.querySelector(':scope > summary').draggable) throw new Error('groups-inline-editor: group DnD was not parked');
})();`,
		},
		{
			Key:     "groups-inline-editor-blur",
			Title:   "Groups tab — inline editor blur focus",
			Caption: "TCL-357 (self-checked): leaving a transient group editor restores its Preact-managed chip without stealing focus from the human's destination.",
			JS: showGroups + expandGroups + `document.body.classList.add('dock-open');` + `return (async function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  var chip = det && det.querySelector(':scope > summary .group-descr');
  var destination = document.querySelector('#filter-groups');
  if (!chip || !destination) throw new Error('groups-inline-editor-blur: controls missing');
  chip.click();
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var input = det.querySelector(':scope > summary .group-descr-input');
  if (!input) throw new Error('groups-inline-editor-blur: input did not open');
  destination.focus();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.activeElement !== destination) throw new Error('groups-inline-editor-blur: focus was stolen back from destination');
  if (input.isConnected || chip.hidden) throw new Error('groups-inline-editor-blur: editor did not restore its managed chip');
})();`,
		},
		{
			// The 🎯 fold toggle: frontend-squad is a deployed force (fixture sets
			// its mission), so its .group-force-block renders by default. Clicking
			// the toggle in the action row must hide the card and leave the button
			// in its .folded accent reading "show info". ensureForceOpen first, so
			// this state starts from a known-open card even if a prior state (or the
			// other skin's run of THIS state) already folded the persisted pref.
			// Self-checking (throws) so a broken fold fails the run instead of
			// passing as a silent "ok".
			Key:     "force-folded",
			Title:   "Groups tab — task-force card folded",
			Caption: "The 🎯 toggle (self-checked): frontend-squad's info card is hidden; the accented 🎯 show-info button in the action row is the way back.",
			JS: showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open');` + `return (async function(){
  await window.__groupsForceReady;
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  if (!det) throw new Error('force-folded: frontend-squad not found');
  if (!det.querySelector(':scope > .subtable > .group-force-block')) throw new Error('force-folded: expected an open force card before folding');
  var btn = det.querySelector('.force-fold-btn');
  if (!btn) throw new Error('force-folded: no 🎯 toggle button in the action row');
  btn.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var det2 = document.querySelector('details[data-group-key="frontend-squad"]');
  if (det2.querySelector(':scope > .subtable > .group-force-block')) throw new Error('force-folded: card still present after folding');
  var btn2 = det2.querySelector('.force-fold-btn.folded');
  if (!btn2) throw new Error('force-folded: toggle did not enter its .folded state');
})();`,
		},
		{
			// TCL-613 — the activity badges. fe-dev-watcher's own turn has
			// ended, but it still has a sub-agent and two background shell
			// commands running, so its state cell must carry BOTH 🤖+1 and
			// ⚙+2 and a busy (not idle) idle + work pill. Self-checking (throws) so a
			// regression to a plain "idle" row — the exact bug TCL-613 fixes
			// — fails the run instead of passing as a silent "ok".
			Key:     "groups-activity-badges",
			Title:   "Groups tab — sub-agent + background-shell badges",
			Caption: "TCL-613 (self-checked): an agent whose turn ended while a sub-agent and two background shell commands keep running — a compact green idle + work pill beside 🤖+1 and ⚙+2, so the counts are not repeated.",
			JS: showGroups + expandGroups + `(function(){
  var row = document.querySelector('tr[data-dnd-conv="` + badgesConv + `"]');
  if (!row) throw new Error('activity-badges: fe-dev-watcher row not found');
  var sub = row.querySelector('.activity-badge.badge-subagents');
  var shells = row.querySelector('.activity-badge.badge-bg-shells');
  if (!sub) throw new Error('activity-badges: no sub-agent badge');
  if (!shells) throw new Error('activity-badges: no background-shell badge');
  if (sub.textContent.indexOf('+1') < 0) throw new Error('activity-badges: sub-agent badge reads ' + sub.textContent);
  if (shells.textContent.indexOf('+2') < 0) throw new Error('activity-badges: shell badge reads ' + shells.textContent);
  if (!/background shell command/.test(shells.title)) throw new Error('activity-badges: shell badge tooltip missing');
  // The pill must read busy, not idle: an agent waiting on background
  // work is exactly what TCL-613 stops rendering as plain idle.
  var pill = row.querySelector('.state-pill');
  if (!pill) throw new Error('activity-badges: no state pill');
  if (!pill.classList.contains('state-working')) {
    throw new Error('activity-badges: expected a busy pill, got "' + pill.textContent + '"');
  }
  if (pill.textContent.trim() !== 'idle + work') {
    throw new Error('activity-badges: expected compact idle + work pill, got "' + pill.textContent + '"');
  }
  if (!/background shell/.test(pill.title)) {
    throw new Error('activity-badges: compact pill lost its full-detail tooltip: ' + pill.title);
  }
})();`,
		},
		{
			// JOH-392 — the group TREE. infra-crew is nested under frontend-squad
			// in the fixture, so it must render INSIDE frontend-squad's .subtable,
			// in a .group-subgroups block that sits ABOVE the parent's own member
			// table. Self-checking (throws) so a broken tree fails the run instead
			// of passing as a silent "ok".
			Key:     "groups-nested",
			Title:   "Groups tab — nested subgroup",
			Caption: "JOH-392 (self-checked): infra-crew nested inside frontend-squad — drawn in the parent's body above its member list; collapse the parent to hide the whole subtree.",
			JS: showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open');` + `(function(){
  var parent = document.querySelector('details[data-group-key="frontend-squad"]');
  var child = document.querySelector('details[data-group-key="infra-crew"]');
  if (!parent) throw new Error('groups-nested: frontend-squad not found');
  if (!child) throw new Error('groups-nested: infra-crew not found');
  var sub = parent.querySelector(':scope > .subtable > .group-subgroups');
  if (!sub) throw new Error('groups-nested: no .group-subgroups under frontend-squad');
  if (!sub.contains(child)) throw new Error('groups-nested: infra-crew is not nested inside frontend-squad');
  var table = parent.querySelector(':scope > .subtable > table');
  if (table && !(sub.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING)) {
    throw new Error('groups-nested: subgroups must render above the parent member list');
  }
})();`,
		},
		{
			// TCL-330 — keyboard operability of the quick-option chips.
			// Self-checking (throws) so a regression back to click-only fails
			// the run instead of passing as a silent "ok". The state JS focuses
			// a collapsed group's 🧠 chip and awaits the :focus-within qo-text
			// reveal (the keyboard mirror of the hover fold); the Actions then
			// drive REAL Chrome key input: Enter must open the same profile
			// picker a click does, and Escape must close it and hand focus back
			// to the chip.
			Key:     "groups-chip-keyboard",
			Title:   "Groups tab — quick-chip keyboard operability",
			Caption: "TCL-330 (self-checked): a collapsed group's 🧠 chip holding keyboard focus — the folded label revealed via :focus-within; Enter opens the profile picker, Escape hands focus back.",
			JS: showGroups + collapseGroups + `return (async function(){
  var chip = document.querySelector('details[data-group-key="frontend-squad"] .group-default-model');
  if (!chip) throw new Error('chip-keyboard: no 🧠 chip on frontend-squad');
  if (chip.getAttribute('tabindex') !== '0' || chip.getAttribute('role') !== 'button') throw new Error('chip-keyboard: chip lost its keyboard affordance');
  chip.focus();
  if (document.activeElement !== chip) throw new Error('chip-keyboard: chip did not take focus');
  var qo = chip.querySelector('.qo-text');
  if (matchMedia('(hover: hover)').matches && document.body.classList.contains('group-quick-fold')) {
    var deadline = Date.now() + 3000;
    while (getComputedStyle(qo).opacity !== '1' && Date.now() < deadline) {
      await new Promise(function(resolve){ setTimeout(resolve, 60); });
    }
    if (getComputedStyle(qo).opacity !== '1') throw new Error('chip-keyboard: focus-within did not reveal the folded chip label');
  }
})();`,
			Actions: []dashsnap.BrowserAction{
				{Kind: "key", Key: "Enter"},
				{Kind: "eval", JS: `return (async function(){
  var deadline = Date.now() + 3000;
  var select = document.querySelector('.group-default-profile-select');
  while ((!select || document.activeElement !== select) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 60); });
    select = document.querySelector('.group-default-profile-select');
  }
  if (!select) throw new Error('chip-keyboard: Enter did not open the profile picker');
  if (document.activeElement !== select) throw new Error('chip-keyboard: profile picker did not take focus');
})();`},
				{Kind: "key", Key: "Escape"},
				{Kind: "eval", JS: `return (async function(){
  var deadline = Date.now() + 3000;
  var ae = document.activeElement;
  while ((!ae || ae.dataset.editorKey !== 'group:frontend-squad:default_profile') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
    ae = document.activeElement;
  }
  if (!ae || !ae.classList.contains('group-default-model')) throw new Error('chip-keyboard: Escape did not hand focus back to the chip');
})();`},
			},
			SettleMS: 400,
		},
		{
			Key:   "dock-open",
			Title: "Palette dock open",
			// JOH-390 items 4/5/7: the dock head hosts the re-homed groups-toolbar
			// globals ("+ new group" + ⚙ cog on row 1, the 🧠 default-profile and
			// 🛡 sandbox-profile controls on row 2); the profiles heading is spelled out ("Agent profiles" /
			// "Familiar patterns"); the top-bar Palette toggle is gone.
			Caption: "Palette dock expanded (groups collapsed): re-homed + new group / ⚙ cog / 🧠 default-profile / 🛡 sandbox-profile in the head, full 'Agent profiles' heading, no top-bar toggle.",
			JS:      showGroups + collapseGroups + `document.body.classList.add('dock-open');`,
		},
		{
			Key:   "dock-collapsed",
			Title: "Palette dock collapsed",
			// JOH-390 item 4: collapsed, the re-homed controls render back in the
			// toolbar exactly as before (the only reopen affordance is the edge tab).
			Caption: "Palette dock collapsed, members expanded — main list reclaims the width; + new group / ⚙ cog / 🧠 default-profile / 🛡 sandbox-profile are back in the toolbar.",
			JS:      showGroups + expandGroups + ensureForceOpen + `document.body.classList.remove('dock-open');`,
		},
		{
			// JOH-388 req 3 — the WIDE case: a block far wider than the viewport
			// must scroll fully CLEAR of the fixed dock. Self-checking (see
			// scrollClearJS): rejects if the tail stays hidden, so a regression
			// fails the run rather than passing as a silent "ok".
			Key:      "dock-wide-scroll",
			Title:    "Palette dock + wide content, scrolled right",
			Caption:  "Req 3 (self-checked): dock open, a 3000px block scrolled to max-right — its green END marker lands at the dock's left edge; the state throws if the tail stays hidden.",
			JS:       scrollClearJS(3000, "wide"),
			SettleMS: 400,
		},
		{
			// JOH-388 req 3 — the BAND case: a block whose right edge falls in the
			// narrow strip between viewport-minus-dock and the viewport width. Here
			// doc.scrollWidth floors at the viewport, so an overflow-gated spacer
			// would MISS it; the shipped spacer parks off <main>'s measured content
			// edge instead, so the band clears too. Pinned here so that stays true.
			Key:      "dock-band-scroll",
			Title:    "Palette dock + band-width content, scrolled right",
			Caption:  "Req 3 band case (self-checked): a 1500px block (right edge inside the viewport but past the dock's left edge) still scrolls fully clear of the dock.",
			JS:       scrollClearJS(1500, "band"),
			SettleMS: 400,
		},
		{
			// JOH-388 req 5: each category folds on its own. Collapse Templates
			// to its header (chevron flips); Profiles + Roles stay expanded. The
			// fold is set on the live keyed <details>; Preact preserves it across
			// the 2s tick, matching the persisted-prefs path.
			Key:     "dock-section-collapsed",
			Title:   "Palette dock — one category collapsed",
			Caption: "Req 5: the Templates category collapsed to its header (chevron flipped right); Profiles + Roles stay expanded.",
			JS:      showGroups + `document.body.classList.add('dock-open');` + `var __s = document.querySelector('.dock-section[data-key="templates"]'); if (__s) __s.open = false;`,
		},
		{
			// JOH-390 item 4 — the re-homed ⚙ cog's action-menu must open fully
			// INSIDE the dock (the dock's .dock-inner is overflow:hidden, so a menu
			// that overflowed the dock's edges would be clipped). Self-checking (see
			// cogMenuClearJS): rejects if the open menu spills past #agent-dock's
			// bounds, so a regression fails the run instead of passing as a silent
			// "ok". Also visually shows the re-homed controls in the dock head.
			Key:      "dock-controls-menu",
			Title:    "Palette dock — re-homed cog menu open",
			Caption:  "Item 4 (self-checked): + new group / ⚙ cog re-homed into the dock head, the cog's menu opened — it stays fully within the dock (throws if it clips past the dock edges).",
			JS:       cogMenuClearJS(),
			SettleMS: 350,
		},
		{
			// The dock is Groups-tab-only: on any other tab the whole shell —
			// panel AND edge toggle — is gone and no page space is reserved.
			// Self-checking (see jobsNoDockJS): switches to the Jobs tab with
			// dock-open FORCED on, then asserts #agent-dock computes display:none
			// and its edge toggle isn't laid out — so a regression that let the
			// dock leak onto another tab fails the run instead of passing silently.
			Key:      "jobs-nodock",
			Title:    "Jobs tab — dock hidden (Groups-only)",
			Caption:  "Groups-only gate (self-checked): on the Jobs tab the palette dock is entirely gone (panel + edge toggle), even with dock-open forced; throws if any part leaks through.",
			JS:       jobsNoDockJS(),
			SettleMS: 300,
		},
		{
			// The right-side-panel preset-clone feature: a card's ⚙ opens an
			// Edit / Clone actions menu instead of jumping straight to the editor.
			// Self-checking (see cardMenuJS): rejects if the menu doesn't open or
			// clips past the dock's horizontal bounds (a narrow menu inside a
			// card, so it must fit) — a regression fails the run, not a silent ok.
			// In wizard mode Clone reads "Mirror".
			Key:      "dock-card-menu",
			Title:    "Palette dock — card actions menu open",
			Caption:  "A profile card's ⚙ opened → the Edit / Clone menu (self-checked: opens + stays inside the dock; Clone reads 'Mirror' in wizard mode).",
			JS:       cardMenuJS(),
			SettleMS: 250,
		},
		{
			// Clone → the generic new-name dialog (#clone-modal), pre-filled
			// "<name>-copy". Self-checking (see cardCloneJS): rejects if the
			// dialog doesn't open or the name isn't pre-filled. Verifies the
			// per-#id wizard chrome the operator flagged renders (an unstyled
			// dialog would show plain-dark + a white submit here).
			Key:      "dock-card-clone",
			Title:    "Clone-a-preset dialog",
			Caption:  "The profile card's ⚙ → Clone opens the new-name dialog, pre-filled '<name>-copy' (self-checked; wizard chrome under body.wizard #clone-modal).",
			JS:       cardCloneJS(),
			SettleMS: 350,
		},
		{
			Key:      "group-create-blank",
			Title:    "Group create — blank",
			Caption:  "TCL-455: the Preact-owned empty-group form keeps native controls, populated editable fields, validation surface and scoped plain/wizard chrome at one viewport.",
			JS:       groupCreateDashSnapJS(false),
			SettleMS: 300,
		},
		{
			Key:      "group-create-template",
			Title:    "Group create — template + mirror",
			Caption:  "TCL-455: a seeded circle shows its roster, task, mirrored group defaults and subgroup choice under the same Preact owner in both visual themes.",
			JS:       groupCreateDashSnapJS(true),
			SettleMS: 300,
		},
		{
			Key:      "template-manager",
			Title:    "Template manager",
			Caption:  "The Preact template manager with native filter controls, roster summaries and live deployed-force readback.",
			JS:       templateManagerJS(),
			SettleMS: 500,
		},
		{
			Key:      "template-editor",
			Title:    "Template editor",
			Caption:  "The Preact template editor preserving the existing wide layout, native controls, placeholders and nested roster styling.",
			JS:       templateEditorJS(),
			SettleMS: 500,
		},
		{
			Key:      "summon-normal",
			Title:    "Summon dialog (normal)",
			Caption:  "Template dropped on empty space → plain summon, no mode chooser.",
			JS:       summonJS(`#groups-list`, false),
			SettleMS: 800,
		},
		{
			Key:      "summon-reinforce",
			Title:    "Summon dialog (reinforce)",
			Caption:  "Template dropped on a group → mode chooser, reinforce-in-place selected.",
			JS:       summonJS(`details[data-dnd-target-group="`+tfTemplate+`"]`, false),
			SettleMS: 800,
		},
		{
			Key:      "summon-copy",
			Title:    "Summon dialog (copy)",
			Caption:  "Same drop, copy mode selected → spawn a new group in the target's image.",
			JS:       summonJS(`details[data-dnd-target-group="`+tfTemplate+`"]`, true),
			SettleMS: 800,
		},
		// --- Single-agent retire dialog (TCL-491) — the four transaction phases
		// of the Preact #retire-modal, each opened for real (row button or the
		// same controller seam the palette/DnD use) and self-checked. See the
		// retire* fixture constants for why each phase owns a separate target.
		{
			Key:      "retire-idle-worktree",
			Title:    "Retire dialog — idle removable worktree",
			Caption:  "TCL-491 (self-checked): the row-opened retire dialog after the worktree probe — shutdown and delete-worktree default ON, probed path·branch shown, primary focused, skin-correct title copy.",
			JS:       retireDialogIdleJS(),
			SettleMS: 300,
		},
		{
			Key:     "retire-busy-locked",
			Title:   "Retire dialog — busy, non-dismissible",
			Caption: "TCL-491 (self-checked): the retire POST is held open in the browser, freezing the dialog mid-submission — spinner + busy label, cancel and both choices disabled, and a real Escape keypress bounces off.",
			JS:      retireDialogBusyJS(),
			InitJS:  retireBusyFetchHoldJS(),
			Actions: []dashsnap.BrowserAction{
				{Kind: "key", Key: "escape"},
				{Kind: "eval", JS: `
if (!document.querySelector('#retire-modal.show')) throw new Error('retire-busy: Escape dismissed a busy transaction');
if (document.querySelector('#confirm-modal.show')) throw new Error('retire-busy: Escape opened a discard confirm during a busy transaction');
if (!document.querySelector('#retire-ok[aria-busy="true"]')) throw new Error('retire-busy: busy lock dropped after Escape');`},
			},
			SettleMS: 300,
		},
		{
			Key:      "retire-error-retry",
			Title:    "Retire dialog — inline HTTP error + retry",
			Caption:  "TCL-491 (self-checked): a real daemon 409 answers the submission — inline role=alert error, primary relabelled Retry, the submitted choices stay frozen while cancel re-enables.",
			JS:       retireDialogErrorJS(),
			SettleMS: 300,
		},
		{
			Key:      "retire-dangling-confirm",
			Title:    "Retire dialog — dangling entry hand-off",
			Caption:  "TCL-491 (self-checked): retiring a dangling enrollment (real 409 {dangling:true}) unmounts the transaction dialog and hands off to the shell confirm, which takes focus. Its OK is never pressed.",
			JS:       retireDialogDanglingJS(),
			SettleMS: 300,
		},
	}
	return append(states, processGraphStates()...)
}

func groupCreateDashSnapJS(withTemplate bool) string {
	preset := ""
	if withTemplate {
		preset = tfTemplate
	}
	return fmt.Sprintf(`return (async function(){
  document.querySelector('nav [data-tab="groups"]').click();
  var controller = await import('/static/js/group-create-controller.js');
  controller.openGroupCreateModal(%q);
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var modal = document.querySelector('#group-create-modal.show');
  if (!modal || document.querySelectorAll('#group-create-modal').length !== 1) throw new Error('group-create ownership failed');
  var name = modal.querySelector('#group-create-name');
  name.value = %q;
  name.dispatchEvent(new Event('input', {bubbles:true}));
  var cwd = modal.querySelector('#group-create-cwd');
  if (%t) {
    var source = modal.querySelector('#group-create-source');
    source.value = %q;
    source.dispatchEvent(new Event('change', {bubbles:true}));
    var nested = modal.querySelector('#group-create-parent');
    nested.checked = true;
    nested.dispatchEvent(new Event('change', {bubbles:true}));
    var task = modal.querySelector('#group-create-task');
    task.value = 'Ship the dashboard migration and report verification.';
    task.dispatchEvent(new Event('input', {bubbles:true}));
    if (!modal.querySelector('#group-create-template-preview .tp-row')) throw new Error('group-create roster preview missing');
    if (modal.querySelector('#group-create-task-row').hidden) throw new Error('group-create template fields hidden');
  } else {
    cwd.value = '/workspace/tclaude';
    cwd.dispatchEvent(new Event('input', {bubbles:true}));
    var cap = modal.querySelector('#group-create-max-members');
    cap.value = '6';
    cap.dispatchEvent(new Event('input', {bubbles:true}));
    if (modal.querySelector('#group-create-max-members-row').hidden) throw new Error('group-create blank cap hidden');
  }
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.activeElement !== name) name.focus();
})();`, preset, map[bool]string{false: "new-dashboard-group", true: "dashboard-party"}[withTemplate], withTemplate, otherGroup)
}

func directoryPickerDashSnapJS() string {
	return `return (async function(){
  var helpers = await import('/static/js/helpers.js');
  var existing = document.querySelector('#directory-picker-modal .modal-buttons button:not(.primary)');
  if (existing) {
    existing.click();
    await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  }
  window.__dashsnapDirectoryPicker = helpers.pickDirectory({
    startDir: '.', title: 'Select a dashboard workspace'
  });
  for (var i = 0; i < 80; i++) {
    var modal = document.querySelector('#directory-picker-modal');
    var list = modal && modal.querySelector('.directory-picker-list');
    var count = modal && modal.querySelector('.directory-picker-count');
    if (modal && list && count.textContent && !modal.querySelector('.directory-picker-path button').disabled) {
      if (modal.getAttribute('aria-hidden') === 'true') throw new Error('picker unexpectedly hidden');
      var input = modal.querySelector('#directory-picker-path');
      if (document.activeElement !== input) throw new Error('picker did not take focus');
      var first = list.querySelector('button');
      if (!first) throw new Error('picker fixture has no folders to filter');
      var name = first.lastElementChild.textContent;
      var base = first.title.slice(0, first.title.length - name.length);
      input.value = base + name.slice(0, Math.min(3, name.length));
      input.dispatchEvent(new Event('input', { bubbles: true }));
      await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
      if (!list.querySelector('button.active')) throw new Error('filtered picker has no active match');
      if (!count.textContent.includes(' of ')) throw new Error('filtered picker count missing');
      return;
    }
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  throw new Error('directory picker did not become ready');
})();`
}

// templateManagerJS opens the real Preact-owned management overlay through the
// dashboard button and self-checks the stable DOM/CSS hooks shared by both
// plain and wizard skins. The screenshot matrix applies each skin separately.
func templateManagerJS() string {
	return `document.querySelector('nav [data-tab="groups"]').click();
var __open = document.querySelector('#templates-manage-open');
if (!__open) throw new Error('template manager trigger not found');
__open.click();
return new Promise(function(resolve, reject){
  setTimeout(function(){
    var modal = document.querySelector('#templates-manage-modal.show');
    var filter = modal && modal.querySelector('#filter-templates[type=text]');
    var card = modal && modal.querySelector('.template-card[data-template="frontend-squad"]');
    if (!modal) { reject(new Error('template manager did not open: ' + (document.querySelector('#management-root')?.textContent || 'empty management root'))); return; }
    if (!filter || filter.placeholder.indexOf('template name') < 0) { reject(new Error('template filter parity check failed')); return; }
    if (!card || !card.querySelector('button[data-tact=edit]')) { reject(new Error('template card actions missing')); return; }
    resolve();
  }, 250);
});
`
}

// templateEditorJS follows the same user path as the manager screenshot, then
// opens a seeded template and verifies native form types/defaults before the
// browser captures the plain/wizard visual result.
func templateEditorJS() string {
	return `document.querySelector('nav [data-tab="groups"]').click();
var __open = document.querySelector('#templates-manage-open');
if (!__open) throw new Error('template manager trigger not found');
__open.click();
return new Promise(function(resolve, reject){
  setTimeout(function(){
    var edit = document.querySelector('#templates-manage-modal .template-card[data-template="frontend-squad"] button[data-tact=edit]');
    if (!edit) { reject(new Error('seeded template edit action missing')); return; }
    edit.click();
    setTimeout(function(){
      var modal = document.querySelector('#template-editor-modal.show');
      var name = modal && modal.querySelector('#template-editor-name[type=text]');
      var roster = modal && modal.querySelector('#template-editor-agents .template-agent-row');
      var role = roster && roster.querySelector('select.ta-role-ref');
      var profile = roster && roster.querySelector('select.ta-profile-select');
      if (!modal) { reject(new Error('template editor did not open')); return; }
      if (!name || name.value !== 'frontend-squad' || !name.placeholder) { reject(new Error('template identity/default parity check failed')); return; }
      if (!roster || !role || !profile) { reject(new Error('native roster controls missing')); return; }
      resolve();
    }, 250);
  }, 250);
});
`
}

func boundedMessagesJS() string {
	return `return import('/static/js/feature-state-registry.js').then(async function(registry) {
  var errors = [];
  window.addEventListener('error', function(event){ errors.push(String(event.error?.stack || event.error || event.message)); });
  var tab = document.querySelector('nav [data-tab="messages"]');
  if (!tab) throw new Error('missing Messages tab');
  tab.click();
  var deadline = Date.now() + 4000;
  while (Date.now() < deadline) {
    var state = registry.featureState('messages');
    var ready = state?.view?.value?.messageRequest?.phase === 'ready';
    var human = document.querySelector('#mail-sidebar .mailbox[data-id="human"]');
    if (ready && human) { human.click(); break; }
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  deadline = Date.now() + 4000;
  while (Date.now() < deadline) {
    var row = document.querySelector('#mail-list .mail-row');
    if (row) { row.click(); break; }
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  deadline = Date.now() + 4000;
  while (!document.querySelector('#mail-reader .mail-reader-body') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
  var client = document.querySelector('#tab-messages.active .mail-client');
  var reader = document.querySelector('#mail-reader .mail-reader-body');
  if (!client || !reader) {
    var state = registry.featureState('messages')?.view?.value;
    var debug = document.createElement('pre');
    debug.textContent = JSON.stringify({errors: errors, messages: state?.messages}, null, 2);
    document.querySelector('#tab-messages').prepend(debug);
    throw new Error('Messages island did not render: client=' + !!client + ' reader=' + !!reader);
  }
  var root = document.querySelector('#messages-root');
  var sidebar = document.querySelector('#mail-sidebar');
  var list = document.querySelector('#mail-list');
  var readerPane = document.querySelector('#mail-reader');
  var footer = document.querySelector('footer');
  var sidebarFoot = document.querySelector('.mail-sidebar-col .mail-sidebar-foot:last-of-type');
  if (!root || !sidebar || !list || !readerPane || !footer || !sidebarFoot) throw new Error('Messages layout fixture is incomplete');

  // Force each pane past the available height without changing production
  // fixture data. A bounded layout gives every pane its own scrollbar; an
  // unbounded mount host instead grows the whole page and fails these checks.
  [sidebar, list, readerPane].forEach(function(pane) {
    var probe = document.createElement('div');
    probe.setAttribute('data-dashsnap-scroll-probe', '');
    probe.style.cssText = 'height:1600px;min-height:1600px;width:1px;opacity:0;pointer-events:none';
    pane.appendChild(probe);
  });
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });

  var nonScrolling = [sidebar, list, readerPane].filter(function(pane) {
    return pane.scrollHeight <= pane.clientHeight;
  });
  if (nonScrolling.length) throw new Error('Messages panes are not independently scrollable: ' + nonScrolling.map(function(pane){ return pane.id; }).join(', '));
  var footerTop = footer.getBoundingClientRect().top;
  var clientBottom = client.getBoundingClientRect().bottom;
  if (clientBottom > footerTop + 1) throw new Error('Messages client overlaps the fixed footer by ' + Math.round(clientBottom - footerTop) + 'px');
  if (sidebarFoot.getBoundingClientRect().bottom > footerTop + 1) throw new Error('Sidebar footer toggles extend behind the dashboard footer');
  if (document.documentElement.scrollHeight > window.innerHeight + 1) throw new Error('Messages tab scrolls the whole document instead of its panes');
});`
}

// processGraphStates is a test-only host surface for the otherwise standalone
// graph core. Production still imports/renders nothing until the feature-gated
// Processes host does so; dashsnap dynamically imports the module and mounts it
// in a fixed overlay solely to exercise the shared SVG vocabulary in both skins.
func processGraphStates() []dashsnap.State {
	terminalTangentsGraph := `{
  nodes: [
    {id:'top',type:'task',label:'Natural output',pinned:{x:180,y:100}},
    {id:'below',type:'decision',label:'Natural input',pinned:{x:180,y:330}},
    {id:'side-a',type:'task',label:'Side source',pinned:{x:470,y:120}},
    {id:'side-b',type:'end',label:'Side target',pinned:{x:760,y:155}},
    {id:'diag-a',type:'decision',label:'Diagonal source',pinned:{x:410,y:390}},
    {id:'diag-b',type:'wait',label:'Diagonal target',pinned:{x:760,y:510}}
  ],
  edges: [
    {from:'top',to:'below',outcome:'snapped'},
    {from:'side-a',to:'side-b',outcome:'side fallback'},
    {from:'diag-a',to:'diag-b',outcome:'diagonal fallback'},
    {from:'side-b',to:'side-a',outcome:'back fallback',back:true}
  ]
}`
	return []dashsnap.State{
		{
			Key:     "process-linear",
			Title:   "Process graph — linear",
			Caption: "Graph core: start → task → wait/timer → end, including state glyph/progress anchors and the explicitly themed Fit control.",
			JS: processGraphStateJS("Small linear graph", `{
  nodes: [
    {id:'start',type:'start',label:'Start'},
    {id:'plan',type:'task',label:'Plan the change',overlay:{glyph:'✓',status:'complete'}},
    {id:'timer',type:'wait',label:'Wait for signal',overlay:{glyph:'…',status:'waiting'}},
    {id:'end',type:'end',label:'Done'}
  ],
  edges: [{from:'start',to:'plan'},{from:'plan',to:'timer'},{from:'timer',to:'end'}]
}`),
		},
		{
			Key:     "process-light",
			Title:   "Process graph — explicit light palette",
			Caption: "Standalone light color-scheme palette: shapes, edges, overlays, labels, ports, focus and Fit control remain legible on a light host.",
			JS: processGraphStateJS("Light process graph", `{
  nodes: [
    {id:'start',type:'start',label:'Start'},
    {id:'choose',type:'decision',label:'Ready?',overlay:{glyph:'?',status:'review'}},
    {id:'finish',type:'end',label:'Finish'}
  ],
  edges: [{from:'start',to:'choose'},{from:'choose',to:'finish',outcome:'yes'}]
}`, "light"),
		},
		{
			Key:     "process-decision-join",
			Title:   "Process graph — decision fan-out + join",
			Caption: "Diamond decision with labelled yes/no branches converging on an all-join; edge and node shapes remain legible without relying on colour.",
			JS: processGraphStateJS("Decision fan-out and join", `{
  nodes: [
    {id:'start',type:'start'}, {id:'gate',type:'decision',label:'Checks pass?'},
    {id:'ship',type:'task',label:'Ship change',overlay:{glyph:'▶',status:'running',attempt:2}},
    {id:'fix',type:'task',label:'Fix findings',overlay:{glyph:'!',badge:'2 issues'}},
    {id:'join',type:'task',label:'Record outcome'}, {id:'end',type:'end'}
  ],
  edges: [
    {from:'start',to:'gate'}, {from:'gate',to:'ship',outcome:'yes'},
    {from:'gate',to:'fix',outcome:'no'}, {from:'ship',to:'join',joinOnTarget:'all'},
    {from:'fix',to:'join',joinOnTarget:'all'}, {from:'join',to:'end'}
  ]
}`),
		},
		{
			Key:     "process-compound",
			Title:   "Process graph — collapsed compound",
			Caption: "Collapsed compound node with stage-stack, stage count, expand affordance, progress slot, plus ordinary start/end vocabulary.",
			JS: processGraphStateJS("Collapsed compound", `{
  nodes: [
    {id:'start',type:'start'},
    {id:'delivery',type:'task',label:'Code change with review',compound:{stages:['plan','implement','review'],collapsed:true},overlay:{glyph:'▶',progress:{current:2,total:3},retry:1}},
    {id:'end',type:'end'}
  ],
  edges: [{from:'start',to:'delivery'},{from:'delivery',to:'end'}]
}`),
		},
		{
			Key:     "process-retry",
			Title:   "Process graph — retry back-edge",
			Caption: "Sanctioned poison-escalation retry is routed outside the layers as a dashed curved return, labelled retry and never disguised as forward flow.",
			JS: processGraphStateJS("Retry return edge", `{
  nodes: [
    {id:'implement',type:'task',label:'Implement',overlay:{glyph:'▶',attempt:3,retry:2}},
    {id:'review',type:'decision',label:'Review passes?'},
    {id:'escalate',type:'task',label:'Escalate poison'}, {id:'done',type:'end'}
  ],
  edges: [
    {from:'implement',to:'review'}, {from:'review',to:'done',outcome:'approved'},
    {from:'review',to:'escalate',outcome:'poison'},
    {from:'escalate',to:'implement',outcome:'retry',back:true}
  ]
}`),
		},
		{
			Key:     "process-pinned-auto",
			Title:   "Process graph — pinned + automatic",
			Caption: "Mixed layout: the editor-owned Review position stays pinned while unpinned task/decision/end nodes auto-layout around it without overlap.",
			JS: processGraphStateJS("Pinned and automatic nodes", `{
  nodes: [
    {id:'start',type:'start'}, {id:'draft',type:'task',label:'Draft'},
    {id:'review',type:'decision',label:'Review',pinned:{x:420,y:380},overlay:{glyph:'◆',badge:'pinned'}},
    {id:'revise',type:'task',label:'Revise'}, {id:'end',type:'end'}
  ],
  edges: [
    {from:'start',to:'draft'}, {from:'draft',to:'review'},
    {from:'review',to:'revise',outcome:'changes'}, {from:'review',to:'end',outcome:'approve'},
    {from:'revise',to:'review',outcome:'resubmit',back:true},
    {from:'start',to:'end',outcome:'fast path'}
  ]
}`),
		},
		{
			Key:     "process-port-endpoints",
			Title:   "Process graph — connector terminal tangents",
			Caption: "Arrow geometry: vertical port-snapped, side-boundary, diagonal-boundary, and dashed return edges all point along their visibly rendered terminal segment.",
			JS:      processGraphStateJS("Port snapping and geometry fallbacks", terminalTangentsGraph),
		},
	}
}

func processGraphStateJS(title, graph string, colorSchemes ...string) string {
	colorScheme := "dark"
	if len(colorSchemes) > 0 && colorSchemes[0] != "" {
		colorScheme = colorSchemes[0]
	}
	return fmt.Sprintf(`return import('/static/js/process-graph.js').then(function(mod) {
  var shell = document.createElement('section');
  shell.setAttribute('data-dashsnap-process-graph', '');
  shell.style.cssText = 'position:fixed;z-index:9500;inset:72px 28px 42px;background:#0d1117;border:1px solid #4a5568;border-radius:12px;padding:14px;box-shadow:0 18px 60px rgba(0,0,0,.65);display:grid;grid-template-rows:auto 1fr;gap:10px;';
  var heading = document.createElement('h2');
  heading.textContent = %q;
  heading.style.cssText = 'margin:0;color:#e6edf3;font:650 16px system-ui';
  var host = document.createElement('div');
  host.style.cssText = 'min-height:0;height:100%%';
  shell.append(heading, host);
  document.body.append(shell);
  var graph = %s;
  var portEvents = [];
  var instance = mod.createProcessGraph(host, graph, {
    fitOnRender:false,
    colorScheme:%q,
    ariaLabel:%q,
    onPortDragStart:function(e){ portEvents.push(['start', e]); },
    onPortDragEnd:function(e){ portEvents.push(['end', e]); }
  });
  return new Promise(function(resolve, reject) {
    requestAnimationFrame(function() { requestAnimationFrame(function() {
      try {
      instance.fitToView();
      if (!host.querySelector('.process-graph-svg')) throw new Error('process graph SVG did not render');
      var hoverNode = host.querySelector('.process-node');
      hoverNode.dispatchEvent(new PointerEvent('pointermove', {bubbles:true}));
      var hoverPorts = host.querySelector('.process-node-ports[data-node-id="' + CSS.escape(hoverNode.dataset.nodeId) + '"]');
      if (!hoverPorts.classList.contains('is-node-hover')) throw new Error('node hover did not reveal sibling-layer ports');
      host.querySelector('.process-graph-svg').dispatchEvent(new PointerEvent('pointerleave'));
      if (hoverPorts.classList.contains('is-node-hover')) throw new Error('port hover affordance did not clear');
      var ports = host.querySelectorAll('.process-port');
      ports[0].focus();
      ports[0].dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', bubbles:true}));
      ports[1].dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', bubbles:true}));
      if (portEvents.length !== 2 || !portEvents[0][1].keyboard || !portEvents[1][1].keyboard) throw new Error('keyboard port hooks did not complete');
      var focusedID = host.querySelector('.process-node').dataset.nodeId;
      host.querySelector('.process-node').focus();
      instance.setGraph(graph);
      if (!document.activeElement || document.activeElement.dataset.nodeId !== focusedID) throw new Error('setGraph did not restore node focus');
      var focusedEdge = host.querySelector('.process-edge');
      var focusedEdgeID = focusedEdge.dataset.edgeId;
      focusedEdge.focus();
      focusedEdge.dispatchEvent(new MouseEvent('click', {bubbles:true}));
      var reorderedGraph = Object.assign({}, graph, {edges:[].concat(graph.edges).reverse()});
      instance.setGraph(reorderedGraph);
      if (!document.activeElement || document.activeElement.dataset.edgeId !== focusedEdgeID) throw new Error('edge focus followed an unstable array index');
      if (!host.querySelector('.process-edge.is-selected') || host.querySelector('.process-edge.is-selected').dataset.edgeId !== focusedEdgeID) throw new Error('edge selection followed an unstable array index');
      var foreignNode = document.createElement('div');
      foreignNode.dataset.nodeId = 'foreign';
      var foreignPort = document.createElement('span');
      foreignPort.dataset.port = 'out';
      foreignNode.append(foreignPort);
      shell.append(foreignNode);
      var foreignHit = instance.eventTarget({target:foreignPort});
      foreignNode.remove();
      if (foreignHit.node || foreignHit.port || foreignHit.edge) throw new Error('graph accepted a foreign interaction target');
      var errorHost = document.createElement('div');
      var sentinel = document.createElement('span');
      sentinel.textContent = 'keep me';
      errorHost.append(sentinel);
      shell.append(errorHost);
      var constructorThrew = false;
      try {
        mod.createProcessGraph(errorHost, {nodes:[{id:'duplicate'},{id:'duplicate'}],edges:[]});
      } catch (error) {
        constructorThrew = true;
      }
      if (!constructorThrew) throw new Error('invalid graph constructor did not reject');
      if (errorHost.firstChild !== sentinel) throw new Error('invalid constructor bricked its host');
      errorHost.remove();
      instance.fitToView();
      resolve();
      } catch (error) {
        reject(error);
      }
    }); });
  });
});`, title, graph, colorScheme, title)
}

// nodeDialogStateJS builds on the editor harness: it grows the seeded
// release-train template into a compound task ("implement": plan + agent
// work + ordered checks + review gate + retry + captures) and a human
// decision ("escalate") purely through the client edit model, then runs the
// state's dialog-driving JS. `return` matters — MustEval awaits the promise,
// so the self-checks (throws) gate the capture.
func nodeDialogStateJS(extraJS string) string {
	const seed = `ed.model.addNode('task', {x: 470, y: 90, id: 'implement', name: 'Implement'});
  ed.model.updateNode('implement', function(n){
    n.performer = {kind: 'agent', profile: 'dev', prompt: 'Implement the change', model: 'opus', effort: 'high',
      contact: {cadence: '10m', budget: 3, escalationTarget: 'human:operator'}};
    n.plan = {id: 'plan', approval: 'human', performer: {kind: 'agent', profile: 'dev', prompt: 'Plan the implementation'}};
    n.checks = [
      {id: 'tests', performer: {kind: 'program', run: 'go', args: ['test', './...']}},
      {id: 'cold-review', performer: {kind: 'agent', profile: 'reviewer', prompt: 'Cold-review the diff'}},
    ];
    n.review = {id: 'merge-approval', performer: {kind: 'human', profile: 'operator', ask: 'Approve merge?'}};
    n.retry = {maxAttempts: 3, onFail: 'feedback-same-session'};
    n.captures = ['diff', 'test-report'];
  });
  ed.model.addNode('decision', {x: 660, y: 240, id: 'escalate', name: 'Escalate'});
  ed.model.updateNode('escalate', function(n){
    n.performer = {kind: 'human', profile: 'operator', ask: 'Retries exhausted. Continue?', choices: ['retry', 'cancel']};
  });
  ed.model.addEdge('implement', 'pass', 'ship');
  ed.model.addEdge('implement', 'fail', 'escalate');
  ed.model.addEdge('escalate', 'cancel', 'ship');
  ed.refresh({fit: true});
  `
	return processEditorStateJS(seed + extraJS)
}

// nodeDialogSelfCheck asserts the dialog is open with the work performer's
// kind select showing `kind` — a broken dialog fails the run instead of
// passing as a silent blank capture.
func nodeDialogSelfCheck(kind string) string {
	return fmt.Sprintf(`
  if (!document.querySelector('.process-node-modal .process-node-detail')) throw new Error('node dialog did not open');
  var kinds = Array.from(document.querySelectorAll('.process-node-modal .process-node-select'));
  if (!kinds.some(function(sel){ return sel.value === %q; })) throw new Error('no performer editor shows kind %s');`, kind, kind)
}

// nodeDialogScrollToWork scrolls the dialog body so the work section's
// performer editor is the visible content in the capture.
const nodeDialogScrollToWork = `
  var workHead = Array.from(document.querySelectorAll('.process-node-section-title')).find(function(el){ return el.textContent === 'work'; });
  if (!workHead) throw new Error('work section missing');
  workHead.scrollIntoView();`

func processConnectionFeedbackState(key, title, sourceSelector, targetSelector, state, message string) dashsnap.State {
	caption := "TCL-563: a trusted connector drag exposes the shared source/valid/invalid action vocabulary, delayed bounded tooltip, accessible reason, unchanged port geometry, and readable regular/wizard contrast."
	return dashsnap.State{
		Key: key, Title: title, Caption: caption,
		JS: processEditorStateJS(fmt.Sprintf(`var source=document.querySelector(%q),target=document.querySelector(%q);
  if(!source||!target) throw new Error('connection feedback fixture ports missing');
  var geometry=function(port){return [port.getAttribute('cx'),port.getAttribute('cy'),port.getAttribute('r')].join(':');};
  window.__connectionFeedback={sourceSelector:%q,targetSelector:%q,state:%q,message:%q,
    before:Array.from(document.querySelectorAll('.process-port')).map(geometry)};`, sourceSelector, targetSelector, sourceSelector, targetSelector, state, message)),
		Actions: []dashsnap.BrowserAction{
			{Kind: "mouse-down", Selector: sourceSelector},
			{Kind: "eval", JS: `var fixture=window.__connectionFeedback,root=document.querySelector('.process-graph'),tip=root.querySelector('.process-action-tooltip');
  if(!root.classList.contains('is-connecting')) throw new Error('trusted pointerdown did not enter connection feedback');
  if(!document.querySelector(fixture.sourceSelector).classList.contains('is-connection-source')) throw new Error('source port state missing');
  if(tip.classList.contains('is-visible')) throw new Error('action tooltip appeared before its delay');`},
			{Kind: "move-to-at", JS: fmt.Sprintf(`var r=document.querySelector(%q).getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`, targetSelector), Steps: 6},
			{Kind: "eval", JS: `var fixture=window.__connectionFeedback,target=document.querySelector(fixture.targetSelector),tip=document.querySelector('.process-action-tooltip');
  if(!target.classList.contains('is-connection-'+fixture.state)) throw new Error(fixture.state+' target class missing');
  if(target.getAttribute('aria-disabled')!==String(fixture.state==='invalid')) throw new Error('target aria-disabled disagrees with feedback state');
  if(tip.classList.contains('is-visible')) throw new Error('target tooltip skipped the reasonable delay');`},
			{Kind: "move-by", DX: 0.25, DY: 0.25, Steps: 1},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){setTimeout(function(){try{
  var fixture=window.__connectionFeedback,root=document.querySelector('.process-graph'),target=document.querySelector(fixture.targetSelector),tip=root.querySelector('.process-action-tooltip');
  if(!tip.classList.contains('is-visible')||tip.textContent.trim()!==fixture.message) throw new Error('delayed target tooltip/reason missing: '+tip.textContent);
  if(tip.getAttribute('role')!=='tooltip'||target.getAttribute('aria-describedby')!==tip.id) throw new Error('tooltip accessibility relationship missing');
  if(getComputedStyle(tip).pointerEvents!=='none') throw new Error('tooltip blocks graph input');
  var rootRect=root.getBoundingClientRect(),tipRect=tip.getBoundingClientRect();
  if(tipRect.left<rootRect.left-1||tipRect.right>rootRect.right+1||tipRect.top<rootRect.top-1||tipRect.bottom>rootRect.bottom+1||tipRect.width>301) throw new Error('tooltip escaped its bounded graph surface');
  var after=Array.from(document.querySelectorAll('.process-port')).map(function(port){return [port.getAttribute('cx'),port.getAttribute('cy'),port.getAttribute('r')].join(':');});
  if(JSON.stringify(after)!==JSON.stringify(fixture.before)||after.some(function(value){return !value.endsWith(':6');})) throw new Error('feedback changed connector geometry');
  var rgb=function(value){var parts=String(value).match(/[\d.]+/g);if(!parts||parts.length<3)throw new Error('unparseable colour '+value);return parts.slice(0,3).map(Number);};
  var luminance=function(value){return rgb(value).map(function(channel){channel/=255;return channel<=.04045?channel/12.92:Math.pow((channel+.055)/1.055,2.4);}).reduce(function(total,channel,index){return total+channel*[.2126,.7152,.0722][index];},0);};
  var contrast=function(a,b){var x=luminance(a),y=luminance(b);return (Math.max(x,y)+.05)/(Math.min(x,y)+.05);};
  var targetStyle=getComputedStyle(target),rootStyle=getComputedStyle(root),tipStyle=getComputedStyle(tip);
  if(contrast(targetStyle.stroke,rootStyle.backgroundColor)<3) throw new Error('target affordance contrast below 3:1');
  if(contrast(tipStyle.color,tipStyle.backgroundColor)<4.5) throw new Error('tooltip text contrast below 4.5:1');
  var dash=targetStyle.strokeDasharray;
  if(fixture.state==='invalid'&&(dash==='none'||dash==='0px')) throw new Error('invalid target is color-only');
  if(fixture.state==='valid'&&dash!=='none'&&dash!=='0px') throw new Error('valid target retained invalid dash vocabulary');
  resolve();}catch(error){reject(error);}},820);});`},
		},
		SettleMS: 250,
	}
}

// processEditorStateJS opens the seeded release-train template in the graph
// editor (Processes tab → Templates → open) and waits for the lazily imported
// editor to mount its canvas, then runs extraJS with `ed` bound to the editor
// instance (its dashsnap/test handle) to drive selection/dirty states.
// processEditorLegacyEdgeErrorRowJS drives a real duplicate rejection whose
// offending edge carries a maximal 512-character unbroken outcome — legal per
// PROCESS_CLIPBOARD_MAX_OUTCOME — and then measures the rendered result. A
// static DOM test cannot catch this: `white-space: normal` alone will not break
// an unbroken token, so the row would silently overflow the editor mount, which
// clips it, and the trailing recovery clause would be lost with no tooltip to
// fall back on. Asserting real geometry is the only way to pin that.
var processEditorLegacyEdgeErrorRowJS = processEditorStateJS(`var outcome='x'.repeat(512);
  ed.model.template.nodes.ordinary={type:'task'};
  ed.model.template.nodes.legacyEnd={type:'end',result:'success'};
  ed.model.edges.push({from:'legacyEnd',outcome:outcome,to:'ordinary'});
  ed.model.layout.nodes.ordinary={x:180,y:300};
  ed.model.layout.nodes.legacyEnd={x:180,y:180};
  ed.refresh({fit:true});
  await editorPaint();
  ed.setSelection({type:'multi',items:[{type:'node',id:'legacyEnd'},{type:'node',id:'ordinary'}]});
  ed.duplicateSelection();
  await editorPaint();
  var status=document.querySelector('.process-editor-status');
  if(!status||!status.classList.contains('is-error')) throw new Error('the legacy duplicate rejection did not render an error status');
  if(status.textContent.indexOf(outcome)===-1) throw new Error('the maximal outcome is not present verbatim in the message');
  if(!/Deselect one of its endpoint nodes, or delete the edge first\./.test(status.textContent)) throw new Error('the trailing recovery clause is missing from the message');
  if(status.getAttribute('title')) throw new Error('recovery text must not depend on a pointer-only tooltip');
  var mount=document.querySelector('#process-editor-canvas.process-editor-mount');
  if(!mount) throw new Error('editor mount missing');
  var rowRect=status.getBoundingClientRect(),mountRect=mount.getBoundingClientRect();
  // The row itself must stay inside the clipping mount horizontally.
  if(rowRect.right>mountRect.right+1||rowRect.left<mountRect.left-1) throw new Error('the error row overflows the clipping editor mount: '+JSON.stringify({row:rowRect.toJSON(),mount:mountRect.toJSON()}));
  // And the text must actually wrap rather than run off the row: an unwrapped
  // 512-character token would make scrollWidth far exceed the client width.
  if(status.scrollWidth>status.clientWidth+1) throw new Error('the maximal unbroken outcome did not wrap inside the error row: '+JSON.stringify({scrollWidth:status.scrollWidth,clientWidth:status.clientWidth}));
  // The wrapped row must be genuinely multi-line, and every line of it visible.
  if(rowRect.height<=24) throw new Error('the error row did not grow to fit the wrapped message: '+rowRect.height);
  if(rowRect.bottom>mountRect.bottom+1) throw new Error('the wrapped error row is clipped vertically by the editor mount');
  // Header controls must remain present and reachable beside the error row.
  var action=document.querySelector('.process-editor-header .process-action');
  if(!action) throw new Error('header controls disappeared behind the error row');
  var actionRect=action.getBoundingClientRect();
  if(actionRect.width<=0||actionRect.height<=0||actionRect.right>mountRect.right+1) throw new Error('header controls were pushed out of the editor mount');`)

func processEditorStateJS(extraJS string) string {
	return fmt.Sprintf(`return (async function(){
  var nav = document.querySelector('nav [data-tab="processes"]');
  if (!nav || nav.offsetParent === null) throw new Error('Processes nav is not visible');
  nav.click();
  var sub = document.querySelector('[data-process-subtab="templates"]');
  if (!sub) throw new Error('Processes templates subtab missing');
  sub.click();
  var deadline = Date.now() + 5000;
  var openSel = 'button[data-process-action="edit"][data-id="release-train"]';
  while (!document.querySelector(openSel) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var open = document.querySelector(openSel);
  if (!open) throw new Error('release-train open button did not render');
  open.click();
  while (!document.querySelector('#process-editor-canvas .process-graph-svg') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var ed = document.querySelector('#process-editor-canvas').__processEditor;
  if (!ed) throw new Error('process editor instance missing after mount');
  var editorPaint = function(){
    return new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  };
  %s
})();`, extraJS)
}

func processEditorLayeringState(key, title string) dashsnap.State {
	setup := processEditorStateJS(`window.__browserEd=ed;
  ed.model.layout.nodes.begin={x:330,y:230};
  ed.model.layout.nodes.ship={x:358,y:230};
  ed.model.config.nodeEditable=function(){return false;};
  ed.model.config.edgeEditable=function(){return false;};
  ed.model.config.canInsert=false;
  ed.refresh({fit:true});
  ed.validation.applyDiagnostics([{severity:'error',code:'E_LAYER',scope:'node',targetId:'begin',message:'Raised node keeps its local diagnostic'}]);
  await editorPaint(); await editorPaint();
  var root=document.querySelector('.process-graph'),svg=root.querySelector('.process-graph-svg');
  var nodeLayer=root.querySelector('.process-node-layer'),portLayer=root.querySelector('.process-port-layer');
  var begin=nodeLayer.querySelector('[data-node-id="begin"]'),ship=nodeLayer.querySelector('[data-node-id="ship"]');
  var beginBox=begin.getBoundingClientRect(),shipBox=ship.getBoundingClientRect();
  var overlap={x:(Math.max(beginBox.left,shipBox.left)+Math.min(beginBox.right,shipBox.right))/2,
    y:(Math.max(beginBox.top,shipBox.top)+Math.min(beginBox.bottom,shipBox.bottom))/2};
  if(!(Math.min(beginBox.right,shipBox.right)>Math.max(beginBox.left,shipBox.left))) throw new Error('layering fixture nodes do not overlap');
  var hit=document.elementFromPoint(overlap.x,overlap.y)?.closest('[data-node-id]');
  if(!hit||hit.dataset.nodeId!=='ship') throw new Error('canonical baseline painter order is not deterministic');
  var traversal=function(){return Array.from(nodeLayer.querySelectorAll('[tabindex="0"]')).concat(Array.from(portLayer.querySelectorAll('[tabindex="0"]'))).map(function(element){return element.closest('[data-node-id]').dataset.nodeId+':'+(element.dataset.port||'node');});};
  var geometry=function(){return {
    nodes:Array.from(nodeLayer.children).map(function(node){return [node.dataset.nodeId,node.getAttribute('transform')];}),
    ports:Array.from(portLayer.children).map(function(group){return [group.dataset.nodeId,group.getAttribute('transform'),Array.from(group.querySelectorAll('.process-port')).map(function(port){return [port.dataset.port,port.getAttribute('cx'),port.getAttribute('cy'),port.getAttribute('r')];})];}),
    edge:root.querySelector('.process-edge-path')?.getAttribute('d')||''
  };};
  window.__layering={ed:ed,root:root,svg:svg,nodeLayer:nodeLayer,portLayer:portLayer,
    overlap:overlap,exposed:{x:beginBox.left+3,y:beginBox.top+beginBox.height/2},
    traversal:JSON.stringify(traversal()),geometry:JSON.stringify(geometry()),save:JSON.stringify(ed.model.saveBody()),
    rev:ed.model.rev,undo:ed.model.undoStack.length,pointerID:null};
  svg.addEventListener('pointerdown',function(event){window.__layering.pointerID=event.pointerId;},{capture:true});`)
	assertRaised := `var state=window.__layering,ed=state.ed,root=state.root;
  var frontNode=root.querySelector('.process-front-node-layer > [data-node-id="begin"]');
  var frontPorts=root.querySelector('.process-front-port-layer > [data-node-id="begin"]');
  if(!frontNode||!frontPorts) throw new Error('pointerdown did not paint the interacted node and owned ports at the front');
  if(root.querySelector('.process-front-node-layer').getAttribute('aria-hidden')!=='true'||root.querySelector('.process-front-port-layer').getAttribute('aria-hidden')!=='true') throw new Error('front paint layers entered the accessibility tree');
  if(frontNode.matches('[role],[tabindex],[aria-label]')||frontNode.querySelector('[role],[tabindex],[aria-label]')||frontPorts.querySelector('[role],[tabindex],[aria-label]')) throw new Error('front copies duplicated accessible node/port ownership');
  if(!frontNode.querySelector('.process-overlay-anchor')) throw new Error('node-local overlay did not travel with its raised node');
  if(root.querySelector('.process-action-tooltip').closest('svg')||document.querySelector('.process-issues-panel').closest('svg')) throw new Error('global overlays moved into the node paint layer');
  if(JSON.stringify(Array.from(root.querySelector('.process-graph-viewport').children).map(function(layer){return layer.dataset.key||layer.classList.contains('process-editor-band')&&'band';}).slice(0,5))!==JSON.stringify(['edges','nodes','front-node','ports','front-ports'])) throw new Error('fixed semantic layer order changed');
  if(JSON.stringify(Array.from(state.nodeLayer.querySelectorAll('[tabindex="0"]')).concat(Array.from(state.portLayer.querySelectorAll('[tabindex="0"]'))).map(function(element){return element.closest('[data-node-id]').dataset.nodeId+':'+(element.dataset.port||'node');}))!==state.traversal) throw new Error('click history changed canonical keyboard traversal');
  var hit=document.elementFromPoint(state.overlap.x,state.overlap.y)?.closest('[data-node-id]');
  if(!hit||hit.dataset.nodeId!=='begin'||!hit.closest('.process-front-node-layer')) throw new Error('raised node does not own overlap hit-testing');
  if(JSON.stringify({nodes:Array.from(state.nodeLayer.children).map(function(node){return [node.dataset.nodeId,node.getAttribute('transform')];}),ports:Array.from(state.portLayer.children).map(function(group){return [group.dataset.nodeId,group.getAttribute('transform'),Array.from(group.querySelectorAll('.process-port')).map(function(port){return [port.dataset.port,port.getAttribute('cx'),port.getAttribute('cy'),port.getAttribute('r')];})];}),edge:root.querySelector('.process-edge-path')?.getAttribute('d')||''})!==state.geometry) throw new Error('raising changed canonical node/port/edge geometry');
  if(JSON.stringify(ed.model.saveBody())!==state.save||ed.model.rev!==state.rev||ed.model.undoStack.length!==state.undo) throw new Error('presentation layering mutated save/history state');
  if(!ed.graph.interactionSnapshot().active||state.pointerID==null||!state.svg.hasPointerCapture(state.pointerID)) throw new Error('front painting broke live SVG pointer capture');`
	return dashsnap.State{
		Key: key, Title: title,
		Caption: "TCL-492 real Chrome: partially overlapping nodes raise by last node/port interaction while edges, connector geometry, fixed overlays, model/history, canonical Tab order, focus, pointer capture, and accessible ownership remain stable.",
		JS:      setup,
		Actions: []dashsnap.BrowserAction{
			{Kind: "mouse-down-at", JS: `return window.__layering.exposed;`},
			{Kind: "eval", JS: assertRaised},
			{Kind: "mouse-up"},
			{Kind: "eval", JS: `var state=window.__layering;if(state.ed.graph.interactionSnapshot().active) throw new Error('pointer interaction survived release');
  var begin=state.nodeLayer.querySelector('[data-node-id="begin"]');state.blurs=0;begin.addEventListener('blur',function(){state.blurs+=1;});begin.focus({preventScroll:true});
  if(document.activeElement!==begin||state.blurs!==0) throw new Error('focus-driven raise detached or blurred the canonical node');`},
			{Kind: "key", Key: "Tab"},
			{Kind: "eval", JS: `var state=window.__layering,active=document.activeElement;
  if(!active||active.dataset.nodeId!=='ship'||active.closest('.process-node-layer')!==state.nodeLayer) throw new Error('Tab after raised begin did not follow canonical node order');
  if(state.root.querySelector('.process-front-node-layer > [data-node-id]')?.dataset.nodeId!=='ship') throw new Error('keyboard-focused node was not raised');`},
			{Kind: "key", Key: "Tab"},
			{Kind: "eval", JS: `var state=window.__layering,active=document.activeElement;
  if(!active||active.dataset.port!=='in'||active.closest('[data-node-id]')?.dataset.nodeId!=='begin'||active.closest('.process-port-layer')!==state.portLayer) throw new Error('Tab did not continue from canonical nodes to canonical ports');
  if(active.getAttribute('role')!=='button'||active.getAttribute('tabindex')!=='0'||!active.getAttribute('aria-label')) throw new Error('canonical port accessibility ownership changed');
  if(state.root.querySelector('.process-front-port-layer > [data-node-id]')?.dataset.nodeId!=='begin') throw new Error('keyboard-focused port did not raise its owning node');`},
			{Kind: "key", Key: "Tab"},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){var state=window.__layering,active=document.activeElement;
  try{if(!active||active.dataset.port!=='out'||active.closest('[data-node-id]')?.dataset.nodeId!=='begin') throw new Error('canonical port traversal changed after click history');
  state.ed.refresh();requestAnimationFrame(function(){requestAnimationFrame(function(){try{
    var restored=document.activeElement;if(!restored||restored.dataset.port!=='out'||restored.closest('[data-node-id]')?.dataset.nodeId!=='begin'||restored.closest('.process-port-layer')!==state.portLayer) throw new Error('rerender did not restore canonical port focus');
    if(JSON.stringify(Array.from(state.nodeLayer.querySelectorAll('[tabindex="0"]')).concat(Array.from(state.portLayer.querySelectorAll('[tabindex="0"]'))).map(function(element){return element.closest('[data-node-id]').dataset.nodeId+':'+(element.dataset.port||'node');}))!==state.traversal) throw new Error('rerender changed canonical traversal');
    var fit=state.root.querySelector('.process-fit-button');fit.focus();state.ed.graph.setGraph(state.ed.validation.decorate(state.ed.model.graph()),{resetInteractionLayering:true});
    if(state.root.querySelector('.process-front-node-layer').childElementCount||state.root.querySelector('.process-front-port-layer').childElementCount) throw new Error('explicit whole-model reset retained reused node IDs');
    state.ed.setSelection({type:'node',id:'begin'});resolve(true);
  }catch(error){reject(error);}});});}catch(error){reject(error);}});`},
		},
		SettleMS: 500,
	}
}

func processTabJS(subtab, readySelector string) string {
	// `return` matters: dashsnap's MustEval wraps the JS in a function and
	// awaits its RESULT, so only a returned promise makes the readiness
	// checks (and their throws) actually gate the state.
	return fmt.Sprintf(`return (async function(){
  var nav = document.querySelector('nav [data-tab="processes"]');
  if (!nav || nav.offsetParent === null) throw new Error('Processes nav is not visible');
  nav.click();
  var sub = document.querySelector('[data-process-subtab="%s"]');
  if (!sub) throw new Error('Processes subtab %s missing');
  sub.click();
  var deadline = Date.now() + 3000;
  while (!document.querySelector('%s') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!document.querySelector('%s')) throw new Error('Processes populated row did not render');
})();`, subtab, subtab, readySelector, readySelector)
}

// processViewerStateJS opens a seeded run through the same Runs action an
// operator uses and verifies the load-bearing authority split. baseStates is
// expanded across both skins, so every viewer state is captured in regular and
// wizard chrome without maintaining two diverging fixtures.
func processViewerStateJS(runID string, rich bool) string {
	expected := `
  var unavailable = canvas.querySelector('.process-viewer-unavailable.reason-legacy_schema');
  if (!unavailable) throw new Error('legacy routing unavailable state missing');
  if (canvas.querySelector('.process-viewer-tabs')) throw new Error('legacy viewer rendered checkpoint detail tabs');`
	if rich {
		expected = `
  if (canvas.querySelector('.process-viewer-unavailable')) throw new Error('schema-7 routing unexpectedly unavailable');
  var activeDetailTab = canvas.querySelector('.process-viewer-tabs button[role="tab"][aria-selected="true"]');
  if (!activeDetailTab || activeDetailTab.tabIndex !== 0) throw new Error('routing detail tabs missing selected roving tab');
  var activeDetailPanel = canvas.querySelector('#' + activeDetailTab.getAttribute('aria-controls'));
  if (!activeDetailPanel || activeDetailPanel.getAttribute('role') !== 'tabpanel' || activeDetailPanel.getAttribute('aria-labelledby') !== activeDetailTab.id) throw new Error('routing detail tabpanel relationship missing');
  activeDetailTab.focus();
  activeDetailTab.dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowRight', bubbles: true, cancelable: true}));
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var nextDetailTab = canvas.querySelector('.process-viewer-tabs button[role="tab"][aria-selected="true"]');
  if (!nextDetailTab || nextDetailTab === activeDetailTab || document.activeElement !== nextDetailTab || nextDetailTab.tabIndex !== 0) throw new Error('routing detail keyboard activation did not move selection and focus');
  if (!canvas.querySelector('.process-viewer-state-chips span')) throw new Error('checkpoint state counts missing');`
	}
	return fmt.Sprintf(`return (async function(){
  var nav = document.querySelector('nav [data-tab="processes"]');
  if (!nav || nav.offsetParent === null) throw new Error('Processes nav is not visible');
  nav.click();
  var sub = document.querySelector('[data-process-subtab="runs"]');
  if (!sub) throw new Error('Processes runs subtab missing');
  sub.click();
  var deadline = Date.now() + 5000;
  var openSel = 'button[data-process-action="view"][data-id="%s"]';
  while (!document.querySelector(openSel) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var open = document.querySelector(openSel);
  if (!open) throw new Error('process viewer action did not render for %s');
  open.click();
  while (!document.querySelector('#process-viewer-canvas .process-viewer-header') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var canvas = document.querySelector('#process-viewer-canvas');
  if (!canvas) throw new Error('process viewer did not mount');
  if (!canvas.querySelector('.process-viewer-authority-strip')) throw new Error('viewer authority boundary missing');
  while (!canvas.querySelector('.process-viewer-graph .process-graph-svg') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!canvas.querySelector('.process-viewer-graph .process-graph-svg')) throw new Error('exact pinned topology graph missing');
  if (!canvas.querySelector('.process-viewer-timeline')) throw new Error('sanitized timeline missing');%s
  var graphRect = canvas.querySelector('.process-viewer-graph').getBoundingClientRect();
  var timelineRect = canvas.querySelector('.process-viewer-timeline').getBoundingClientRect();
  if (graphRect.width < 100 || graphRect.height < 100) {
    var graphNode = canvas.querySelector('.process-viewer-graph');
    var graphTrace = [];
    for (var cursor = graphNode; cursor && graphTrace.length < 6; cursor = cursor.parentElement) {
      var style = getComputedStyle(cursor); var rect = cursor.getBoundingClientRect();
      graphTrace.push({tag: cursor.tagName, cls: cursor.className, display: style.display, position: style.position, width: rect.width, height: rect.height});
    }
    throw new Error('exact pinned topology graph is not visible: ' + JSON.stringify(graphTrace));
  }
  if (timelineRect.width < 100 || timelineRect.height < 20) throw new Error('sanitized timeline is not visible: ' + JSON.stringify(timelineRect.toJSON()));
})();`, runID, runID, expected)
}

// processViewerEpochStateJS opens a seeded schema-8 run and verifies the S6
// safe-summary contract: the honest restriction banner, the adaptation
// summary panel with authority state chips, the memory-only unlock draft
// field, and the absence of any exact-topology rendering.
func processViewerEpochStateJS(runID string) string {
	return fmt.Sprintf(`return (async function(){
  var nav = document.querySelector('nav [data-tab="processes"]');
  if (!nav || nav.offsetParent === null) throw new Error('Processes nav is not visible');
  nav.click();
  var sub = document.querySelector('[data-process-subtab="runs"]');
  if (!sub) throw new Error('Processes runs subtab missing');
  sub.click();
  var deadline = Date.now() + 5000;
  var openSel = 'button[data-process-action="view"][data-id="%s"]';
  while (!document.querySelector(openSel) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var open = document.querySelector(openSel);
  if (!open) throw new Error('process viewer action did not render for %s');
  open.click();
  while (!document.querySelector('#process-viewer-canvas .process-epoch-summary') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var canvas = document.querySelector('#process-viewer-canvas');
  if (!canvas) throw new Error('process viewer did not mount');
  if (!canvas.querySelector('.process-viewer-unavailable.reason-epoch_v8_summary')) throw new Error('epoch_v8_summary restriction banner missing');
  if (canvas.querySelector('.process-graph-svg')) throw new Error('schema-8 viewer must not render exact topology');
  var panel = canvas.querySelector('.process-epoch-summary');
  if (!panel) throw new Error('adaptation summary panel missing');
  if (!panel.querySelector('.process-viewer-state-chips span')) throw new Error('authority state chips missing');
  if (!panel.querySelector('#process-unlock-source')) throw new Error('unlock draft field missing');
  var label = panel.querySelector('label[for="process-unlock-source"]');
  if (!label) throw new Error('unlock draft field is unlabeled');
  var rect = panel.getBoundingClientRect();
  if (rect.width < 100 || rect.height < 60) {
    var trace = [];
    for (var cursor = panel; cursor && trace.length < 8; cursor = cursor.parentElement) {
      var style = getComputedStyle(cursor); var r = cursor.getBoundingClientRect();
      trace.push({tag: cursor.tagName, id: cursor.id, cls: String(cursor.className).slice(0, 60), display: style.display, hidden: cursor.hidden, width: r.width, height: r.height});
    }
    throw new Error('adaptation summary panel is not visible: ' + JSON.stringify(trace));
  }
})();`, runID, runID)
}

// worklistTabJS drives the Worklist sub-view into one of its filter-chip
// views and self-checks the load-bearing chrome: the ready selector for the
// view's body, the degraded-runs strip (must be visible — the seed plants a
// corrupt run), and the actionable-count badge (the seed mints exactly two
// pending human decisions).
func worklistTabJS(view, readySelector string) string {
	// `return` so MustEval awaits the promise — see processTabJS.
	return fmt.Sprintf(`return (async function(){
  var nav = document.querySelector('nav [data-tab="processes"]');
  if (!nav || nav.offsetParent === null) throw new Error('Processes nav is not visible');
  nav.click();
  var sub = document.querySelector('[data-process-subtab="worklist"]');
  if (!sub) throw new Error('Worklist subtab missing');
  sub.click();
  var deadline = Date.now() + 3000;
  var chip;
  while (!(chip = document.querySelector('button[data-worklist-view="%s"]')) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!chip) throw new Error('Worklist view chip %s missing');
  chip.click();
  deadline = Date.now() + 3000;
  while (!document.querySelector('%s') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!document.querySelector('%s')) throw new Error('Worklist state did not render');
  var strip = document.querySelector('#process-worklist-degraded');
  if (!strip || strip.hidden) throw new Error('degraded-runs strip is not visible');
  var badge = document.querySelector('#process-worklist-badge');
  if (!badge || badge.hidden || badge.textContent !== '2') {
    throw new Error('worklist badge expected 2, got ' + (badge ? badge.textContent : 'missing'));
  }
})();`, view, view, readySelector, readySelector)
}

func managementModalJS(modulePath, opener, readySelector string) string {
	return fmt.Sprintf(`return (async function(){
  var module = await import(%q);
  if (typeof module[%q] !== 'function') throw new Error('management opener missing: %s');
  module[%q](null);
  var deadline = Date.now() + 3000;
  while (!document.querySelector(%q) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!document.querySelector(%q)) throw new Error('management modal did not render: %s');
})();`, modulePath, opener, opener, opener, readySelector, readySelector, readySelector)
}

// sandboxExclusionHelpJS opens the sandbox-profile editor and expands the [?]
// on the first filesystem-restriction row, so the snapshot captures both the
// compact rows and one row's help popover at once.
func sandboxExclusionHelpJS() string {
	return `return (async function(){
  var module = await import('/static/js/sandbox-profiles.js');
  module.openSandboxProfileEditor(null);
  var deadline = Date.now() + 5000;
  while (!document.querySelector('.sbx-exclusion-row .spawn-field-help-trigger') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  // The catalog ships folded; the expanded-help state lives inside it.
  var fold = document.querySelector('#sandbox-profile-editor-exclusions-fold summary');
  if (!fold) throw new Error('exclusion fold did not render');
  fold.click();
  await new Promise(function(resolve){ setTimeout(resolve, 120); });
  // Descendants stay queryable while collapsed, so assert the fold really
  // expanded rather than screenshotting a closed section with a clicked [?].
  if (!fold.closest('details').open) throw new Error('exclusion fold did not expand');
  var trigger = document.querySelector('.sbx-exclusion-row .spawn-field-help-trigger');
  if (!trigger) throw new Error('sandbox exclusion rows did not render');
  trigger.closest('.sbx-read-exclusions').scrollIntoView({ block: 'center' });
  trigger.click();
  await new Promise(function(resolve){ setTimeout(resolve, 120); });
  if (trigger.getAttribute('aria-expanded') !== 'true') throw new Error('exclusion help did not expand');
})();`
}

func actionDialogJS(call, readySelector, extraJS string) string {
	return fmt.Sprintf(`return (async function(){
  var module = await import('/static/js/action-dialog-controller.js');
  %s
  var deadline = Date.now() + 3000;
  while (!document.querySelector(%q) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!document.querySelector(%q)) throw new Error('action dialog did not render: %s');
  %s
  var overlay = document.querySelector(%q);
  var surface = overlay && overlay.querySelector('.cron-create-modal');
  var title = surface && surface.querySelector('h3');
  var primary = surface && surface.querySelector('.modal-buttons button.primary');
  if (!surface || !title || !primary) throw new Error('action dialog chrome is incomplete: %s');
  var surfaceStyle = getComputedStyle(surface);
  var titleStyle = getComputedStyle(title);
  var primaryStyle = getComputedStyle(primary);
  if (document.body.classList.contains('wizard')) {
    if (!surfaceStyle.backgroundImage.includes('linear-gradient')) {
      throw new Error('wizard action dialog surface lacks gradient chrome');
    }
    if (surfaceStyle.borderColor !== 'rgb(122, 93, 176)') {
      throw new Error('wizard action dialog border is ' + surfaceStyle.borderColor);
    }
    if (titleStyle.color !== 'rgb(243, 230, 192)') {
      throw new Error('wizard action dialog title is ' + titleStyle.color);
    }
    if (primaryStyle.borderColor !== 'rgb(217, 180, 90)') {
      throw new Error('wizard action dialog primary border is ' + primaryStyle.borderColor);
    }
    if (overlay.id === 'reincarnate-agent-modal') {
      var modeLabel = overlay.querySelector('.reincarnate-mode label');
      var modeLabelColor = modeLabel && getComputedStyle(modeLabel).color;
      if (modeLabelColor !== 'rgb(231, 217, 245)') {
        throw new Error('wizard reincarnate mode label is ' + modeLabelColor);
      }
    }
  } else if (surfaceStyle.borderColor === 'rgb(122, 93, 176)') {
    throw new Error('wizard action dialog chrome leaked into plain mode');
  }
})();`, call, readySelector, readySelector, readySelector, extraJS, readySelector, readySelector)
}

func jobsCronCreateDashSnapJS() string {
	return `return (async function(){
  document.querySelector('nav [data-tab="jobs"]').click();
  var deadline = Date.now() + 4000;
  while (!document.querySelector('#cron-create-open') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  var open = document.querySelector('#cron-create-open');
  if (!open) throw new Error('jobs-cron-create: Jobs launcher did not render');
  open.click();
  while (!document.querySelector('#cron-create-modal.show') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  function input(id, value) {
    var el = document.getElementById(id);
    if (!el) throw new Error('jobs-cron-create: missing #' + id);
    el.value = value;
    el.dispatchEvent(new Event('input', {bubbles:true}));
  }
  input('cron-create-name', 'release-readiness');
  document.querySelector('input[name="cron-create-target-mode"][value="group"]').click();
  await Promise.resolve();
  var group = document.querySelector('#cron-create-group');
  group.value = 'frontend-squad';
  group.dispatchEvent(new Event('change', {bubbles:true}));
  await Promise.resolve();
  input('cron-create-role', 'dev');
  document.querySelector('input[name="cron-create-schedule-mode"][value="cron"]').click();
  await Promise.resolve();
  input('cron-create-cron', '*/15 * * * *');
  input('cron-create-subject', 'Release readiness');
  input('cron-create-body', 'Report final checks and blockers before the release window.');
  while (Date.now() < deadline) {
    var explanation = document.querySelector('#cron-create-cron-explain');
    if (explanation && explanation.textContent.trim() && !explanation.textContent.includes('Explaining')) break;
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  var modal = document.querySelector('#cron-create-modal.show');
  var expectedTitle = document.body.classList.contains('wizard') ? 'Bind a recurring ritual' : 'Schedule a cron job';
  if (!modal || document.querySelectorAll('#cron-create-modal').length !== 1) throw new Error('jobs-cron-create: dialog ownership failed');
  if (document.querySelector('#cron-create-title').textContent.trim() !== expectedTitle) throw new Error('jobs-cron-create: theme title mismatch');
  if (group.value !== 'frontend-squad' || document.querySelector('#cron-create-role').value !== 'dev') throw new Error('jobs-cron-create: group target draft missing');
  if (!document.querySelector('#cron-create-cron-explain').textContent.trim()) throw new Error('jobs-cron-create: schedule explanation missing');
  if (!document.querySelector('#cron-create-enabled').checked) throw new Error('jobs-cron-create: enabled default missing');
})();`
}

func jobsCronRowDialogDashSnapJS(action string, stacked bool) string {
	return fmt.Sprintf(`return (async function(){
  document.querySelector('nav [data-tab="jobs"]').click();
  var deadline = Date.now() + 4000;
  var row;
  while (!(row = document.querySelector('#jobs-list tr[data-key^="cron-"]')) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  if (!row) throw new Error('jobs-cron-%s: seeded cron row did not render');
  var action = %q;
  var button = Array.from(row.querySelectorAll('.row-actions button')).find(function(node){
    return node.textContent.trim() === action;
  });
  if (!button) throw new Error('jobs-cron-%s: row action missing');
  button.click();
  while (!document.querySelector('#cron-create-modal.show') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  var name = document.querySelector('#cron-create-name');
  var expectedName = action === 'duplicate' ? 'dashsnap-release-ritual-copy' : 'dashsnap-release-ritual';
  if (!name || name.value !== expectedName) throw new Error('jobs-cron-%s: name prefill mismatch');
  if (document.querySelector('#cron-create-group').value !== 'frontend-squad') throw new Error('jobs-cron-%s: group prefill missing');
  if (document.querySelector('#cron-create-role').value !== 'dev') throw new Error('jobs-cron-%s: role prefill missing');
  if (document.querySelector('#cron-create-cron').value !== '0 9 * * 1-5') throw new Error('jobs-cron-%s: cron expression prefill missing');
  if (document.querySelector('#cron-create-body').value.indexOf('Report final checks') !== 0) throw new Error('jobs-cron-%s: body prefill missing');
  if (%t) {
    document.querySelector('input[name="cron-create-target-mode"][value="solo"]').click();
    await Promise.resolve();
    document.querySelector('#cron-create-target-pick').click();
    while (!document.querySelector('#cron-pick-target-modal.show') && Date.now() < deadline) {
      await new Promise(function(resolve){ setTimeout(resolve, 25); });
    }
    if (!document.querySelector('#cron-pick-target-modal.show')) throw new Error('jobs-cron-%s: target chooser missing');
    if (document.querySelector('#cron-create-name') !== name || name.value !== expectedName) throw new Error('jobs-cron-%s: stacked chooser recreated the parent draft');
    if (!document.querySelector('#cron-pick-target-list .add-member-row')) throw new Error('jobs-cron-%s: target chooser candidates missing');
  } else {
    while (Date.now() < deadline) {
      var explanation = document.querySelector('#cron-create-cron-explain');
      if (explanation && explanation.textContent.trim() && !explanation.textContent.includes('Explaining')) break;
      await new Promise(function(resolve){ setTimeout(resolve, 40); });
    }
    if (!document.querySelector('#cron-create-cron-explain').textContent.trim()) throw new Error('jobs-cron-%s: stored expression explanation missing');
  }
})();`, action, action, action, action, action, action, action, action, stacked,
		action, action, action, action)
}

func boundedTabJS(tab, readySelector string) string {
	return fmt.Sprintf(`return Promise.all([
  import('/static/js/snapshot-store.js'),
  import('/static/js/feature-state-registry.js')
]).then(async function(modules) {
var store = modules[0], registry = modules[1];
var __tab = document.querySelector('nav [data-tab=%q]');
if (!__tab) throw new Error('missing bounded tab: %s');
if (%q === 'plugins' || %q === 'costs' || %q === 'usage') {
  store.dashboardState.snapshot.value = Object.assign({}, store.dashboardState.snapshot.value, {
    plugins_tab_visible: true, cost_tab_visible: true, usage_tab_visible: true
  });
  await Promise.resolve();
}
__tab.click();
return new Promise(function(resolve, reject) {
  var deadline = Date.now() + 4000;
  (function ready() {
    var node = document.querySelector(%q);
    var panel = document.querySelector('#tab-' + %q);
    var root = document.querySelector('#' + %q + '-root');
    var state = registry.featureState(%q);
    var phase = state?.request?.value?.phase || state?.phase?.value || state?.view?.value?.request?.phase;
    var settled = root && state && !root.querySelector('[role="alert"]') && !root.querySelector('[aria-busy="true"]') &&
      (%q === 'access' || phase === 'ready');
    if (node && panel?.classList.contains('active') && node.getClientRects().length && settled) { resolve(); return; }
    if (Date.now() >= deadline) { reject(new Error('bounded tab did not render: %s')); return; }
    setTimeout(ready, 25);
  })();
});
});`, tab, tab, tab, tab, tab, readySelector, tab, tab, tab, tab, tab)
}

func boundedJobsEmptyJS() string {
	return `return (async function(){
  document.querySelector('nav [data-tab="jobs"]').click();
  var deadline = Date.now() + 4000;
  while (!document.querySelector('#filter-jobs') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  var input = document.querySelector('#filter-jobs');
  if (!input) throw new Error('Jobs filter missing');
  input.focus();
  input.value = 'dashsnap-no-such-job';
  input.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText', data: 'dashsnap-no-such-job' }));
  while (!document.querySelector('#jobs-list .empty') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 50); });
  }
  var empty = document.querySelector('#jobs-list .empty');
  if (!empty) throw new Error('Jobs empty state missing');
})();`
}

func boundedLogsErrorJS() string {
	return `
document.querySelector('nav [data-tab="logs"]').click();
return import('/static/js/feature-state-registry.js').then(function(registry) {
  return new Promise(function(resolve, reject) {
    var deadline = Date.now() + 4000;
    (function ready() {
      var state = registry.featureState('logs');
      if (!state) { if (Date.now() < deadline) { setTimeout(ready, 25); return; } reject(new Error('Logs feature state unavailable')); return; }
      if (state.request.value.phase !== 'ready') { if (Date.now() < deadline) { setTimeout(ready, 25); return; } reject(new Error('Logs initial request did not settle')); return; }
      var token = state.beginRequest();
      state.failRequest(token, new Error('DashSnap simulated failure'));
      setTimeout(function() {
        var alert = document.querySelector('#logs-root [role="alert"]');
        if (!alert || !alert.textContent.includes('DashSnap simulated failure')) reject(new Error('Logs error alert missing'));
        else resolve();
      }, 50);
    })();
  });
});`
}

func boundedLogsPollFocusJS() string {
	return `
document.querySelector('nav [data-tab="logs"]').click();
return new Promise(function(resolve, reject) {
  var deadline = Date.now() + 4000;
  (function ready() {
    var input = document.querySelector('#filter-logs');
    if (!input) { if (Date.now() < deadline) { setTimeout(ready, 25); return; } reject(new Error('Logs filter missing')); return; }
    input.focus();
    input.value = 'focus-poll-proof';
    input.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText', data: 'focus-poll-proof' }));
    input.setSelectionRange(2, 8);
    var timeout = setTimeout(function() {
      document.removeEventListener('tclaude:snapshot', checked);
      reject(new Error('No dashboard snapshot completed after editing Logs filter'));
    }, 5000);
    function checked() {
      document.removeEventListener('tclaude:snapshot', checked);
      clearTimeout(timeout);
      requestAnimationFrame(function() { requestAnimationFrame(function() {
        var current = document.querySelector('#filter-logs');
        if (document.activeElement !== current) reject(new Error('Logs filter lost focus across poll'));
        else if (current.value !== 'focus-poll-proof') reject(new Error('Logs filter value changed across poll'));
        else if (current.selectionStart !== 2 || current.selectionEnd !== 8) reject(new Error('Logs selection changed across poll'));
        else resolve();
      }); });
    }
    document.addEventListener('tclaude:snapshot', checked);
  })();
});`
}

func boundedConfigDialogJS() string {
	return `
document.querySelector('nav [data-tab="config"]').click();
return import('/static/js/feature-state-registry.js').then(function(registry) {
  var state = registry.featureState('config');
  if (!state) throw new Error('Config feature state unavailable');
  void state.confirmDiff('{"theme":"dark"}\n', '{"theme":"light"}\n', false, '/tmp/config.json');
  return new Promise(function(resolve, reject) { requestAnimationFrame(function() { requestAnimationFrame(function() {
    var dialog = document.querySelector('#config-diff-modal');
    if (!dialog) reject(new Error('Config diff dialog missing'));
    else if (document.activeElement?.id !== 'config-diff-confirm') reject(new Error('Config dialog initial focus missing'));
    else resolve();
  }); }); });
});`
}

// scrollClearJS builds a self-checking JOH-388 req-3 state: on the groups tab
// with the dock open, inject a block of the given width (ending in a bright
// green END marker), let hscroll park its clearance spacer, scroll the viewport
// to max, and assert the block's right edge clears the dock's left edge. The
// returned JS ends in a Promise that REJECTS when the tail is still hidden, so
// dashsnap's awaited Eval fails the state — a req-3 regression can't slip
// through as a captured "ok". `label` distinguishes the two cases (wide/band) in
// the readout + error. Note the doubled %% for the literal `height:100%` CSS.
func scrollClearJS(blockWidth int, label string) string {
	return `document.querySelector('nav [data-tab="groups"]').click();
document.body.classList.add('dock-open');` + fmt.Sprintf(`
var __wide = document.createElement('div');
__wide.style.cssText = 'position:relative;width:%dpx;height:120px;margin-top:12px;box-sizing:border-box;padding:8px;color:#fff;font:14px monospace;background:linear-gradient(90deg,#1b3a5b,#2d6da8);';
__wide.textContent = '%s %dpx — scroll right to reveal the end →';
var __end = document.createElement('div');
__end.style.cssText = 'position:absolute;top:0;right:0;width:60px;height:100%%;background:#3fb950;color:#000;font:bold 12px monospace;padding:6px;box-sizing:border-box;';
__end.textContent = 'END';
__wide.appendChild(__end);
document.querySelector('#tab-groups').prepend(__wide);
return new Promise(function(resolve, reject){
  // hscroll parks the clearance spacer on the injection mutation (rAF-coalesced);
  // give it a beat, THEN scroll the viewport to max and assert clearance.
  setTimeout(function(){
    document.documentElement.scrollLeft = 999999;
    var r = __wide.getBoundingClientRect().right;
    var d = document.querySelector('#agent-dock').getBoundingClientRect().left;
    var cleared = r <= d + 2;
    var o = document.createElement('div');
    o.style.cssText = 'position:fixed;left:8px;bottom:36px;z-index:999;background:#000;color:#0f0;font:13px monospace;padding:6px;';
    o.textContent = 'req3 %s wide.right=' + r.toFixed(0) + ' dock.left=' + d.toFixed(0) + ' CLEARS=' + (cleared ? 'YES' : 'NO');
    document.body.appendChild(o);
    if (cleared) resolve();
    else reject(new Error('req3 %s FAIL: tail (' + r.toFixed(0) + ') still under the dock (left ' + d.toFixed(0) + ')'));
  }, 200);
});
`, blockWidth, label, blockWidth, label, label)
}

// jobsNoDockJS builds a self-checking Groups-only-dock state: switch to the Jobs
// tab, FORCE dock-open on (to prove the tab gate wins even against the open
// flag), give dock.js's Groups-pane observer a beat to re-evaluate, then assert
// the dock shell is entirely gone — #agent-dock computes display:none and
// neither it nor its edge toggle is laid out. The returned JS ends in a Promise
// that REJECTS when any part of the dock leaks onto a non-Groups tab, so
// dashsnap's awaited Eval fails the state rather than capturing a silent "ok".
//
// "Laid out" is probed with getClientRects().length, NOT offsetParent:
// #agent-dock is position:fixed (and #dock-toggle position:absolute), and Blink
// reports offsetParent === null for a fixed element even when it's fully
// visible — so an offsetParent probe on the panel is vacuous. getClientRects()
// is the robust geometry check: EMPTY when the element is display:none (the
// required off-tab state), NON-empty when it's laid out — so it also rejects a
// regression that merely SLID the panel off (translateX) instead of removing it.
func jobsNoDockJS() string {
	return `document.querySelector('nav [data-tab="jobs"]').click();
document.body.classList.add('dock-open');
return new Promise(function(resolve, reject){
  // The gate re-evaluates via a MutationObserver on the Groups pane's class
  // (fires on the next microtask after the tab click); give it a beat, then read
  // the settled layout.
  setTimeout(function(){
    var dock = document.getElementById('agent-dock');
    var toggle = document.getElementById('dock-toggle');
    var disp = dock ? getComputedStyle(dock).display : 'none';
    var dockShown = !!(dock && dock.getClientRects().length);
    var toggleShown = !!(toggle && toggle.getClientRects().length);
    var o = document.createElement('div');
    o.style.cssText = 'position:fixed;left:8px;bottom:36px;z-index:999;background:#000;color:#0f0;font:13px monospace;padding:6px;';
    o.textContent = 'groups-only dock: display=' + disp + ' dockShown=' + dockShown + ' toggleShown=' + toggleShown;
    document.body.appendChild(o);
    if (disp === 'none' && !dockShown && !toggleShown) resolve();
    else reject(new Error('dock leaked onto the Jobs tab: display=' + disp + ' dockShown=' + dockShown + ' toggleShown=' + toggleShown));
  }, 150);
});
`
}

// cogMenuClearJS builds a self-checking JOH-390 item-4 state: on the groups tab
// with the dock open, the re-homed ⚙ cog is in the dock head; click it to open
// its .action-menu and assert the menu's rendered box stays fully within
// #agent-dock (the dock's .dock-inner is overflow:hidden, so an overflowing menu
// would be clipped). The returned JS ends in a Promise that REJECTS when the menu
// spills past any dock edge, so dashsnap's awaited Eval fails the state — a
// re-home/positioning regression can't slip through as a captured "ok".
func cogMenuClearJS() string {
	return `document.querySelector('nav [data-tab="groups"]').click();
document.body.classList.add('dock-open');
return new Promise(function(resolve, reject){
  // Let the class observer re-home the controls + the head lay out, THEN open the
  // menu and measure.
  setTimeout(function(){
    var cog = document.querySelector('#dock-actions-primary .cog-btn');
    if (!cog) { reject(new Error('item4 FAIL: re-homed cog not in the dock head')); return; }
    cog.click();
    var menu = document.querySelector('#dock-actions-primary .action-menu.open');
    if (!menu) { reject(new Error('item4 FAIL: cog menu did not open')); return; }
    var m = menu.getBoundingClientRect();
    var d = document.querySelector('#agent-dock').getBoundingClientRect();
    var inside = m.left >= d.left - 2 && m.right <= d.right + 2 && m.bottom <= d.bottom + 2;
    var o = document.createElement('div');
    o.style.cssText = 'position:fixed;left:8px;bottom:36px;z-index:999;background:#000;color:#0f0;font:13px monospace;padding:6px;';
    o.textContent = 'item4 menu[' + m.left.toFixed(0) + ',' + m.right.toFixed(0) + '] dock[' + d.left.toFixed(0) + ',' + d.right.toFixed(0) + '] INSIDE=' + (inside ? 'YES' : 'NO');
    document.body.appendChild(o);
    if (inside) resolve();
    else reject(new Error('item4 FAIL: cog menu (' + m.left.toFixed(0) + '..' + m.right.toFixed(0) + ') clips past the dock (' + d.left.toFixed(0) + '..' + d.right.toFixed(0) + ')'));
  }, 200);
});
`
}

// cardMenuJS builds a self-checking state for the right-side-panel preset-clone
// feature. Two assertions, both rejecting on a miss so a regression fails the run
// instead of passing as a silent captured "ok":
//
//	(a) Up-flip clip guard — the reason the dock component measures #dock-body and
//	    NOT the viewport. Force the dock body to overflow (short max-height),
//	    scroll the LAST card to its fold, open that card's menu, and assert it
//	    stays within #dock-body's bottom. A downward drop there spills under the
//	    body's overflow:auto fold, so it only clears if the menu flipped up —
//	    which the OLD viewport-measuring code would miss (the body bottom sits a
//	    footer-height ABOVE window.innerHeight, so the spill stayed "on screen").
//	(b) Horizontal clip guard + the SCREENSHOT — restore the body, open the
//	    FIRST profile card's menu, assert it stays within #agent-dock's width (a
//	    narrow, right-anchored menu must fit), and leave it open for the capture.
func cardMenuJS() string {
	return `document.querySelector('nav [data-tab="groups"]').click();
document.body.classList.add('dock-open');
return (async function(){
  var flush = function(){ return new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); }); };
  await new Promise(function(resolve){ setTimeout(resolve, 200); });
  var body = document.querySelector('#dock-body');
  var dock = document.querySelector('#agent-dock');
  if (!body || !dock) throw new Error('card-menu FAIL: dock shell missing');
  // (a) Force overflow, drive the LAST card to the fold, open its menu up-flipped.
  var prevMax = body.style.maxHeight;
  body.style.maxHeight = '200px';
  var cards = body.querySelectorAll('.dock-card');
  var last = cards[cards.length - 1];
  if (!last) throw new Error('card-menu FAIL: no cards');
  last.scrollIntoView({block: 'end'});
  var lastCog = last.querySelector('.dock-card-manage[data-dock-act="card-menu"]');
  lastCog.click();
  await flush();
  var lastMenu = last.querySelector('.dock-card-menu.open');
  if (!lastMenu) throw new Error('card-menu FAIL: bottom card menu did not open');
  var lm = lastMenu.getBoundingClientRect();
  var bb = body.getBoundingClientRect();
  var flipped = lastMenu.classList.contains('opens-up');
  var vFit = lm.bottom <= bb.bottom + 2;
  lastCog.click(); // close
  await flush();
  body.style.maxHeight = prevMax;
  body.scrollTop = 0;
  if (!vFit) throw new Error('card-menu FAIL: bottom card menu bottom=' + lm.bottom.toFixed(0) + ' clips past #dock-body bottom=' + bb.bottom.toFixed(0) + ' (opens-up=' + flipped + ')');
  // (b) Screenshot + horizontal guard on the first profile card.
  var card = body.querySelector('.dock-card[data-dock-kind="profiles"]');
  if (!card) throw new Error('card-menu FAIL: no profile card');
  card.querySelector('.dock-card-manage[data-dock-act="card-menu"]').click();
  await flush();
  var menu = card.querySelector('.dock-card-menu.open');
  if (!menu) throw new Error('card-menu FAIL: menu did not open');
  var m = menu.getBoundingClientRect();
  var d = dock.getBoundingClientRect();
  var hFit = m.left >= d.left - 2 && m.right <= d.right + 2;
  var o = document.createElement('div');
  o.style.cssText = 'position:fixed;left:8px;bottom:36px;z-index:999;background:#000;color:#0f0;font:13px monospace;padding:6px;';
  o.textContent = 'card-menu H-INSIDE=' + (hFit ? 'YES' : 'NO') + ' bottom-card V-FIT=' + (vFit ? 'YES' : 'NO') + ' (flip=' + (flipped ? 'up' : 'down') + ')';
  document.body.appendChild(o);
  if (!hFit) throw new Error('card-menu FAIL: menu (' + m.left.toFixed(0) + '..' + m.right.toFixed(0) + ') clips past the dock (' + d.left.toFixed(0) + '..' + d.right.toFixed(0) + ')');
})();
`
}

// cardCloneJS builds a self-checking state for the clone dialog: open a profile
// card's ⚙ menu, click its Clone item, and assert the generic new-name dialog
// (#clone-modal) opens with the name pre-filled. Rejects on a miss so a broken
// clone wiring fails the run. The captured PNG also lets a human eyeball the
// per-#id wizard chrome (which string pins can't see).
func cardCloneJS() string {
	return `document.querySelector('nav [data-tab="groups"]').click();
document.body.classList.add('dock-open');
return new Promise(function(resolve, reject){
  setTimeout(function(){
    var card = document.querySelector('.dock-card[data-dock-kind="profiles"]');
    if (!card) { reject(new Error('card-clone FAIL: no profile card')); return; }
    var cog = card.querySelector('.dock-card-manage[data-dock-act="card-menu"]');
    if (cog) cog.click();
    var clone = card.querySelector('.dock-card-menu-item[data-dock-act="clone-item"]');
    if (!clone) { reject(new Error('card-clone FAIL: no Clone menu item')); return; }
    clone.click();
    // The action-dialog host reconciles the published signal on Preact's next
    // turn. Wait for that paint instead of asserting in the click stack (the
    // retired imperative modal used to mutate the DOM synchronously here).
    requestAnimationFrame(function(){
      var modal = document.querySelector('#clone-modal.show');
      if (!modal) { reject(new Error('card-clone FAIL: clone dialog did not open')); return; }
      var name = document.querySelector('#clone-modal-name');
      if (!name || !name.value) { reject(new Error('card-clone FAIL: name not pre-filled')); return; }
      resolve();
    });
  }, 200);
});
`
}

// summonJS opens the unified summon dialog by SYNTHESIZING the dock drag-and-drop
// the app's own document-level listeners expect: a dragstart on the template
// card (which the app's handler reads to set its module-private drop state), then
// dragover + drop on the target, sharing one DataTransfer so the drop handler's
// getData() sees the payload. Dropping on `#groups-list` (empty space) opens the
// plain summon; dropping on a group opens it with the reinforce/copy chooser. If
// copyMode, the copy radio is then clicked.
func summonJS(dropSel string, copyMode bool) string {
	js := fmt.Sprintf(`
document.querySelector('nav [data-tab="groups"]').click();
document.body.classList.add('dock-open');
var __card = document.querySelector('.dock-card[draggable="true"][data-dock-kind="templates"][data-dock-name=%q]');
if (!__card) throw new Error('template card not found: %s');
var __drop = document.querySelector(%q);
if (!__drop) throw new Error('drop target not found: %s');
var __dt = new DataTransfer();
function __fire(el, type) {
  var ev = new DragEvent(type, {bubbles:true, cancelable:true, dataTransfer:__dt});
  if (ev.dataTransfer !== __dt) { Object.defineProperty(ev, 'dataTransfer', {value:__dt}); }
  el.dispatchEvent(ev);
}
__fire(__card, 'dragstart');
__fire(__drop, 'dragover');
__fire(__drop, 'drop');
return new Promise(function(resolve, reject){
setTimeout(function(){
if (!document.querySelector('#template-deploy-modal.show')) {
  reject(new Error('summon dialog did not open (#template-deploy-modal.show absent)')); return;
}
`, tfTemplate, tfTemplate, dropSel, dropSel)
	if copyMode {
		js += `
var __copy = document.querySelector('#template-deploy-modal input[name=template-deploy-mode][value=copy]');
if (!__copy) { reject(new Error('copy radio not found')); return; }
__copy.click();
`
	} else {
		js += `
var __reinforce = document.querySelector('#template-deploy-modal input[name=template-deploy-mode][value=reinforce]');
if (__reinforce) {
  __reinforce.click();
  setTimeout(function(){
    var __groupName = document.querySelector('#template-deploy-group');
    if (!__groupName.readOnly || !__groupName.classList.contains('locked')) { reject(new Error('reinforce group lock parity failed')); return; }
    resolve();
  }, 50);
  return;
}
`
	}
	js += `
resolve();
}, 150);
});
`
	return js
}

// ---------------------------------------------------------------------------
// Single-agent retire dialog states (TCL-491). Each helper returns a complete
// self-checking program for one transaction phase of the Preact #retire-modal.
// The idle and busy phases open the dialog through the production per-row
// retire button on the Groups tab; the error and dangling phases open it
// through the same controller seam the command palette and DnD launchers use
// (their targets are not roster members, deliberately — see the fixture).
// ---------------------------------------------------------------------------

// retireDialogWaitJS is the shared bounded poll each retire state prepends:
// __retireWait(state, what, get) resolves with get()'s first truthy value or
// throws a labelled error, so a hung phase fails the shot with a real reason.
const retireDialogWaitJS = `var __retireWait = async function(state, what, get) {
  var deadline = Date.now() + 8000;
  for (;;) {
    var got = get();
    if (got) return got;
    if (Date.now() > deadline) throw new Error(state + ': ' + what + ' did not appear');
    await new Promise(function(resolve){ setTimeout(resolve, 30); });
  }
};`

// retireRowOpenJS opens conv's retire dialog exactly as an operator does:
// Groups tab, groups expanded, the row's retire button clicked, then waits for
// the keyed transaction owner to mount AND for the worktree probe to land its
// removable default (worktree checkbox present, ON, enabled) so a submission
// after this prologue always freezes the fully-populated choice row.
func retireRowOpenJS(stateKey, conv string) string {
	return fmt.Sprintf(`document.querySelector('nav [data-tab="groups"]').click();
document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){d.open=true;});
return (async function(){
  %s
  var btn = await __retireWait(%q, 'row retire button', function(){
    return document.querySelector('button[data-act="retire-agent"][data-conv=%q]');
  });
  btn.click();
  await __retireWait(%q, 'retire dialog', function(){ return document.querySelector('#retire-modal.show'); });
  var wt = await __retireWait(%q, 'probed removable worktree default', function(){
    var el = document.querySelector('#retire-wt');
    return el && el.checked && !el.disabled ? el : null;
  });
`, retireDialogWaitJS, stateKey, conv, stateKey, stateKey)
}

func retireDialogIdleJS() string {
	return retireRowOpenJS("retire-idle", retireWorktreeConv) + fmt.Sprintf(`
  var shutdown = document.querySelector('#retire-shutdown');
  if (!shutdown.checked || shutdown.disabled) throw new Error('retire-idle: shutdown default is not an enabled ON');
  var label = document.querySelector('#retire-wt-label').textContent;
  if (label.indexOf(%q) === -1 || label.indexOf(%q) === -1 || label.indexOf('removed after the agent exits') === -1) {
    throw new Error('retire-idle: worktree row does not show the probed path/branch: ' + label);
  }
  var ok = document.querySelector('#retire-ok');
  if (ok.disabled || ok.textContent.trim() !== 'Retire') throw new Error('retire-idle: primary is not an enabled Retire');
  if (document.activeElement !== ok) throw new Error('retire-idle: primary did not take initial focus');
  if (document.querySelector('#retire-cancel').disabled) throw new Error('retire-idle: cancel must stay enabled while idle');
  if (document.querySelector('#retire-meta').textContent.trim() !== 'infra-dev-worktree') throw new Error('retire-idle: meta label mismatch');
  var wizard = document.body.classList.contains('wizard');
  var regularTitle = document.querySelector('#retire-title .retire-title-regular');
  var wizardTitle = document.querySelector('#retire-title .retire-title-wizard');
  if (wizard) {
    if (getComputedStyle(wizardTitle).display === 'none' || getComputedStyle(regularTitle).display !== 'none') throw new Error('retire-idle: wizard title copy missing');
    if (wizardTitle.textContent.trim() !== 'Banish this familiar?') throw new Error('retire-idle: wizard title copy changed: ' + wizardTitle.textContent);
  } else {
    if (getComputedStyle(regularTitle).display === 'none' || getComputedStyle(wizardTitle).display !== 'none') throw new Error('retire-idle: regular title copy missing');
    if (regularTitle.textContent.trim() !== 'Retire this agent?') throw new Error('retire-idle: regular title copy changed: ' + regularTitle.textContent);
  }
})();`, retireWorktreeDir, "tcl-491-idle-probe")
}

// retireBusyFetchHoldJS is the busy state's InitJS: it wraps window.fetch
// BEFORE the dashboard modules capture it at bootstrap, holding exactly the
// busy target's retire POST open forever (a promise that never settles) while
// every other request passes through untouched. The request therefore never
// reaches the daemon — nothing mutates — and the dialog is frozen genuinely
// mid-submission, not painted to look that way.
func retireBusyFetchHoldJS() string {
	return fmt.Sprintf(`(function(){
  if (window.__tclaudeRetireHold) return;
  var realFetch = window.fetch;
  window.__tclaudeRetireHold = true;
  window.__tclaudeRetireHoldHits = 0;
  window.fetch = function(input) {
    var url = typeof input === 'string' ? input : (input && input.url) || '';
    if (url.indexOf('/api/agents/%s/retire') !== -1) {
      window.__tclaudeRetireHoldHits += 1;
      return new Promise(function(){});
    }
    return realFetch.apply(this, arguments);
  };
})();`, retireBusyConv)
}

func retireDialogBusyJS() string {
	return `if (!window.__tclaudeRetireHold) throw new Error('retire-busy: the fetch hold was not installed before page scripts');
` + retireRowOpenJS("retire-busy", retireBusyConv) + `
  document.querySelector('#retire-ok').click();
  var ok = await __retireWait('retire-busy', 'busy submit lock', function(){
    var el = document.querySelector('#retire-ok');
    return el && el.disabled && el.getAttribute('aria-busy') === 'true' ? el : null;
  });
  if (!ok.querySelector('.btn-spinner')) throw new Error('retire-busy: busy primary lost its spinner');
  var busyRegular = ok.querySelector('.theme-copy-regular');
  var busyWizard = ok.querySelector('.theme-copy-wizard');
  if (document.body.classList.contains('wizard')) {
    if (getComputedStyle(busyWizard).display === 'none' || busyWizard.textContent !== 'Banishing…') throw new Error('retire-busy: wizard busy label missing');
  } else if (getComputedStyle(busyRegular).display === 'none' || busyRegular.textContent !== 'Retiring…') {
    throw new Error('retire-busy: regular busy label missing');
  }
  if (!document.querySelector('#retire-cancel').disabled) throw new Error('retire-busy: cancel must be blocked while busy');
  if (!document.querySelector('#retire-shutdown').disabled || !wt.disabled) throw new Error('retire-busy: the in-flight choices must be locked');
  if (window.__tclaudeRetireHoldHits !== 1) throw new Error('retire-busy: expected exactly one held retire request, saw ' + window.__tclaudeRetireHoldHits);
})();`
}

func retireDialogErrorJS() string {
	return fmt.Sprintf(`return (async function(){
  %s
  var controller = await import('/static/js/transaction-dialog-controller.js');
  void controller.openRetireAgentDialog(%q, 'retire-error-target').catch(function(){});
  await __retireWait('retire-error', 'retire dialog', function(){ return document.querySelector('#retire-modal.show'); });
  await __retireWait('retire-error', 'probed removable worktree default', function(){
    var el = document.querySelector('#retire-wt');
    return el && el.checked && !el.disabled ? el : null;
  });
  document.querySelector('#retire-ok').click();
  var error = await __retireWait('retire-error', 'inline request error', function(){
    var el = document.querySelector('#retire-error');
    return el && el.textContent.trim() ? el : null;
  });
  if (error.getAttribute('role') !== 'alert') throw new Error('retire-error: the inline error is not announced as an alert');
  if (error.textContent.indexOf('nothing to retire') === -1) throw new Error('retire-error: unexpected daemon error copy: ' + error.textContent);
  var ok = document.querySelector('#retire-ok');
  if (ok.disabled || ok.textContent.trim() !== 'Retry') throw new Error('retire-error: primary must re-arm as an enabled Retry');
  if (!document.querySelector('#retire-shutdown').disabled || !document.querySelector('#retire-wt').disabled) throw new Error('retire-error: the submitted choices must stay frozen for the retry');
  if (!document.querySelector('#retire-wt').checked) throw new Error('retire-error: the frozen worktree opt-in lost its visible ON state');
  if (document.querySelector('#retire-cancel').disabled) throw new Error('retire-error: cancel must re-enable after the failed attempt');
})();`, retireDialogWaitJS, retireErrorConv)
}

func retireDialogDanglingJS() string {
	return fmt.Sprintf(`return (async function(){
  %s
  var controller = await import('/static/js/transaction-dialog-controller.js');
  void controller.openRetireAgentDialog(%q, 'dangling-entry').catch(function(){});
  await __retireWait('retire-dangling', 'retire dialog', function(){ return document.querySelector('#retire-modal.show'); });
  document.querySelector('#retire-ok').click();
  var confirmOK = await __retireWait('retire-dangling', 'shell confirm hand-off', function(){
    return document.querySelector('#confirm-modal.show #confirm-ok');
  });
  if (document.querySelector('#retire-modal')) throw new Error('retire-dangling: the transaction dialog must unmount before the shell confirm owns the viewport');
  if (document.querySelector('#confirm-title').textContent.indexOf('Remove dangling agent entry?') === -1) throw new Error('retire-dangling: confirm title mismatch');
  if (document.querySelector('#confirm-meta').textContent.indexOf('dangling-entry') === -1) throw new Error('retire-dangling: confirm meta does not carry the agent label');
  if (document.activeElement !== confirmOK) throw new Error('retire-dangling: focus did not hand off to the confirm action');
})();`, retireDialogWaitJS, retireDanglingConv)
}
