package agentd_test

// learnshots_test.go renders the dashboard screenshots embedded in the Learn
// tutorials (docs/learn/). Like TestDocShots it reuses the dashsnap capture
// driver against an isolated fake installation, but seeds the small team the
// tutorials build — a "hello" group led by alice, with workers bob and carol —
// so each image matches the page it sits on. No real names, paths, hosts or
// costs appear. Gated so CI never launches a browser:
//
//	TCLAUDE_DOCSHOTS=1 go test ./pkg/claude/agentd/ -run TestLearnShots -v -count=1 -timeout 600s
//
// Output lands in dashsnap-out/learn-images/ (gitignored); run
// scripts/docs/learn-images.sh to regenerate and install every Learn image.

import (
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
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	learnProject = docHome + "/code/my-project"
	learnRepo    = "https://github.com/you/my-project"
)

type learnAgent struct {
	conv, title, model, status, detail string
	branch                             string // "" means main in learnProject
	age, last                          time.Duration
}

func TestLearnShots(t *testing.T) {
	if os.Getenv("TCLAUDE_DOCSHOTS") == "" {
		t.Skip("docs screenshot generator — set TCLAUDE_DOCSHOTS=1 to run (needs a local headless Chrome)")
	}

	// The workers' pull requests, as the git-info resolver reports them.
	prs := map[string]docPR{
		"readme-typos": {12, "open", docChecks(6, 0)},
		"more-tests":   {13, "open", docChecks(4, 2)},
	}
	t.Cleanup(agentd.SetGitInfoResolverWithChecksForTest(
		func(_, branch string) (string, string, int, string, string, string, bool) {
			pr, ok := prs[branch]
			if !ok {
				return learnRepo, "main", 0, "", "", "", true
			}
			return learnRepo, "main", pr.number, learnRepo + "/pull/" + itoa(pr.number), pr.state, pr.checks, true
		}))

	f := newFlow(t)
	seedLearnTeam(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	restorePopup := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restorePopup)
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
	_ = fetchDashSnapshot(t, mux)
	restorePopup()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// wait polls for a selector-and-text match; the generator drives the real
	// UI by visible text, as a reader would.
	const helpers = `async function waitFor(sel, text){
  var deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    var hit = Array.from(document.querySelectorAll(sel)).find(function(e){ return !text || e.textContent.includes(text); });
    if (hit) return hit;
    await new Promise(function(r){ setTimeout(r, 30); });
  }
  throw new Error('missing ' + sel + ' ' + (text || ''));
}
function hideDragHint(){
  Array.from(document.querySelectorAll('body *')).forEach(function(e){
    if (e.children.length < 12 && /^\s*drag & drop:/.test(e.textContent)) e.style.display = 'none';
  });
}
`
	const showGroups = helpers + `document.querySelector('nav [data-tab="groups"]').click();
document.querySelectorAll('details').forEach(function(d){ d.open = true; });
document.body.classList.remove('dock-open');
hideDragHint();`
	states := []dashsnap.State{{
		Key:     "learn-dashboard-groups",
		Title:   "Learn — the hello team in Groups",
		Caption: "first-agent step 3.",
		// first-agent has one agent so far: show alice alone, without the
		// ownership, mail and notification marks later tutorials add.
		JS: showGroups + `return (async function(){
  for (const name of ['bob', 'carol']) (await waitFor('tr', name)).style.display = 'none';
  document.querySelectorAll('.owner-badge, .human-notification-attention, .human-notification-attention-glyph, .human-notification-hint, #messages-badge').forEach(function(b){ b.style.display = 'none'; });
  Array.from(document.querySelectorAll('button, span')).forEach(function(e){
    if (e.children.length === 0 && /^👥/.test(e.textContent.trim())) e.style.visibility = 'hidden';
  });
})();`,
		Width: 1400, Height: 590,
		SettleMS: 400,
	}, {
		Key:     "learn-dashboard-spawn",
		Title:   "Learn — the spawn dialog",
		Caption: "first-agent step 2.",
		JS: showGroups + `return (async function(){
  var head = await waitFor('summary, .group-head, details', 'hello');
  var btn = (head.closest('details') || document).querySelector('[data-act="spawn-agent"]');
  if (!btn) throw new Error('no spawn button on hello');
  // first-agent starts from an empty group: hide the seeded rows behind the modal.
  document.querySelectorAll('tbody tr').forEach(function(tr){ tr.style.visibility = 'hidden'; });
  btn.click();
  function fill(id, v){
    var el = document.getElementById(id);
    if (!el) throw new Error('missing #' + id);
    el.value = v;
    el.dispatchEvent(new Event('input', {bubbles: true}));
  }
  await waitFor('#agent-spawn-name');
  fill('agent-spawn-name', 'alice');
  fill('agent-spawn-init-msg', 'Read this project and tell me in three sentences what it does.');
})();`,
		// Crop below Harness and Model, the fields the tutorial mentions.
		Width: 1400, Height: 660,
		SettleMS: 500,
	}, {
		Key:     "learn-dashboard-agent-menu",
		Title:   "Learn — an agent's ⚙ menu",
		Caption: "team-lead step 7.",
		JS: showGroups + `return (async function(){
  var row = await waitFor('tr', 'alice');
  var gear = row.querySelector('.cog-glyph');
  if (!gear) throw new Error('no ⚙ on alice');
  gear.closest('button').click();
  await waitFor('[role="menuitem"], button', 'web window');
  // The menu is longer than the page; crop below its everyday items.
  document.querySelector('footer').style.display = 'none';
})();`,
		Width: 1400, Height: 688,
		SettleMS: 400,
	}, {
		Key:     "learn-dashboard-resume",
		Title:   "Learn — Retired and Conversations",
		Caption: "find-and-resume steps 2 and 3.",
		JS: showGroups + `return (async function(){
  var view = await waitFor('button', '▾ view');
  view.click();
  for (const label of ['show retired', 'show conversations']) {
    var opt = await waitFor('#filter-groups-view-menu label, #filter-groups-view-menu [role="menuitemcheckbox"]', label);
    var box = opt.querySelector('input') || opt;
    if (!box.checked) box.click();
  }
  view.click();
  var retired = await waitFor('.group-name', 'Retired');
  await waitFor('.group-name', 'Conversations');
  document.querySelectorAll('details').forEach(function(d){ d.open = true; });
  await waitFor('button', 'reinstate');
  await waitFor('button', 'promote');
  // Show only the two virtual groups: the live hello team (with its own bob)
  // and Ungrouped belong to the earlier tutorials.
  Array.from(document.querySelectorAll('.group-name')).forEach(function(n){
    var name = n.textContent.trim();
    if (name === 'hello' || name.indexOf('Ungrouped') === 0) {
      var block = n.closest('details') || n.parentElement;
      block.style.display = 'none';
    }
  });
  void retired;
})();`,
		Width: 1400, Height: 420,
		SettleMS: 600,
	}, {
		Key:     "learn-dashboard-messages",
		Title:   "Learn — the hello group's mail",
		Caption: "team-lead step 4.",
		JS: helpers + `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  (await waitFor('#mail-sidebar .mailbox', 'hello')).click();
  (await waitFor('#mail-list .mail-row', 'Fix the README typos')).click();
  await waitFor('#mail-reader .mail-reader-body');
})();`,
		Width: 1400, Height: 560,
		SettleMS: 500,
	}, {
		Key:     "learn-dashboard-group-settings",
		Title:   "Learn — the hello group's settings",
		Caption: "team-setup step 1.",
		JS: showGroups + `return (async function(){
  var head = await waitFor('summary, .group-head, details', 'hello');
  var cog = (head.closest('details') || document).querySelector('.group-header-cog button');
  if (!cog) throw new Error('no ⚙ on hello');
  cog.click();
  (await waitFor('button', '⚙ settings…')).click();
  await waitFor('#group-settings-modal');
})();`,
		Width: 1400, Height: 760,
		SettleMS: 500,
	}, {
		Key:     "learn-dashboard-profile",
		Title:   "Learn — saving a spawn profile",
		Caption: "team-setup step 2.",
		JS: showGroups + `return (async function(){
  var head = await waitFor('summary, .group-head, details', 'hello');
  var btn = (head.closest('details') || document).querySelector('[data-act="spawn-agent"]');
  if (!btn) throw new Error('no spawn button on hello');
  btn.click();
  (await waitFor('#agent-spawn-save-profile')).click();
  var name = await waitFor('#profile-editor-name');
  name.value = 'worker';
  name.dispatchEvent(new Event('input', {bubbles: true}));
  // Scroll to the spawn-dialog defaults the tutorial explains.
  (await waitFor('#profile-editor-owner')).scrollIntoView({block: 'center'});
})();`,
		Width: 1400, Height: 820,
		SettleMS: 500,
	}, {
		Key:     "learn-dashboard-lead-team",
		Title:   "Learn — a lead and its workers",
		Caption: "team-lead steps 3 and 5.",
		JS:      showGroups,
		Width:   1400, Height: 480,
		SettleMS: 400,
	}, {
		Key:     "learn-dashboard-notify",
		Title:   "Learn — a notification from the lead",
		Caption: "team-lead step 6.",
		JS: helpers + `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  (await waitFor('#mail-sidebar .mailbox[data-id="human"]')).click();
  (await waitFor('#mail-list .mail-row', 'Both PRs are up')).click();
  await waitFor('#mail-reader .mail-reader-body');
})();`,
		Width: 1400, Height: 640,
		SettleMS: 800,
	}}

	outDir := filepath.Join(dashSnapOutRoot(t), "learn-images")
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
	if err := db.AddAgentGroupOwner(g.ID, alice, "learnshots"); err != nil {
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

	// find-and-resume: bob after "Your first agent team" retired him (a
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
