package agentd_test

// learnshots_test.go renders the dashboard screenshots embedded in the Learn
// tutorials (docs/learn/). Like TestDocShots it reuses the dashsnap capture
// driver against an isolated fake installation, but seeds the small team the
// tutorials build — a "hello" group with alice and bob in ~/code/my-project —
// so each image matches the page it sits on. No real names, paths, hosts or
// costs appear. Gated so CI never launches a browser:
//
//	TCLAUDE_DOCSHOTS=1 go test ./pkg/claude/agentd/ -run TestLearnShots -v -count=1 -timeout 600s
//
// Output lands in dashsnap-out/learn-images/ (gitignored); run
// scripts/docs/learn-images.sh to regenerate and install every Learn image.

import (
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
	"github.com/tofutools/tclaude/pkg/testharness"
)

const learnProject = docHome + "/code/my-project"

type learnAgent struct {
	conv, title, model, status, detail string
	age, last                          time.Duration
}

func TestLearnShots(t *testing.T) {
	if os.Getenv("TCLAUDE_DOCSHOTS") == "" {
		t.Skip("docs screenshot generator — set TCLAUDE_DOCSHOTS=1 to run (needs a local headless Chrome)")
	}

	f := newFlow(t)
	seedLearnTeam(t, f)

	mux := agentd.BuildDashboardHandlerForTest()
	restorePopup := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restorePopup)
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
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
		JS:      showGroups,
		Width:   1400, Height: 590,
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
		Caption: "first-team step 4.",
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
		Caption: "first-team step 3.",
		JS: helpers + `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  (await waitFor('#mail-sidebar .mailbox', 'hello')).click();
  (await waitFor('#mail-list .mail-row', 'entry point')).click();
  await waitFor('#mail-reader .mail-reader-body');
})();`,
		Width: 1400, Height: 560,
		SettleMS: 500,
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

// seedLearnTeam builds the end state of "Your first agent team": a group
// "hello" with alice (asking) and bob (answering), their mail, and one plain
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
	const alice = "1ea40000-0000-4000-8000-00000000a11c"
	const bob = "1ea40000-0000-4000-8000-000000000b0b"
	for _, a := range []learnAgent{
		{conv: alice, title: "alice", model: "Sonnet 5.5", status: "working", detail: "Read", age: 6 * time.Minute, last: 2 * time.Second},
		{conv: bob, title: "bob", model: "Sonnet 5.5", status: "idle", age: 7 * time.Minute, last: 40 * time.Second},
	} {
		seedLearnSession(t, f, a)
		f.HaveMemberWithRole("hello", a.conv, "")
		backdateAgent(t, a.conv, a.age)
	}

	for _, m := range []db.AgentMessage{
		{GroupID: g.ID, FromConv: alice, ToConv: bob, Subject: "entry point?",
			Body: "Hi bob! What is this project's main entry point?"},
		{GroupID: g.ID, FromConv: bob, ToConv: alice, Subject: "Re: entry point?",
			Body: "It's main.go at the repo root: it builds the CLI and hands off to cmd.Execute()."},
	} {
		if _, err := db.InsertAgentMessage(&m); err != nil {
			t.Fatalf("message: %v", err)
		}
	}

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
	f.HaveAliveSessionOnBranch(a.conv, label, "tmux-"+a.title, learnProject, "main")
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
