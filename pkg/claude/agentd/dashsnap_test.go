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
// rod. Run it explicitly:
//
//	TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ -run TestDashSnap -v -count=1 -timeout 300s
//
// Output: dashsnap-out/<timestamp>/ (gitignored) with one PNG per state + an
// index.html contact sheet. See pkg/claude/agentd/dashsnap/dashsnap.go for the
// runtime prerequisites (system Chrome, --no-sandbox, the harmless stderr noise).

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
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
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	// Millisecond granularity so two runs in the same second don't overwrite.
	outDir := filepath.Join(dashSnapOutRoot(t), time.Now().Format("20060102-150405.000"))
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
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States:  states,
	})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}

	sheet := filepath.Join(outDir, "index.html")
	var failed []string
	for _, s := range shots {
		status := "ok"
		if s.Err != "" {
			status = "FAIL: " + s.Err
			failed = append(failed, s.State.Key)
		}
		t.Logf("  [%-18s] %s", s.State.Key, status)
	}
	t.Logf("dashsnap: %d states, %d failed", len(shots), len(failed))
	t.Logf("contact sheet: file://%s", sheet)

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
	}
	infraMembers := []dashMemberSpec{
		{convID: "c2000000-0000-4000-8000-000000000001", label: "lbl-in-lead", tmux: "tmux-in-lead",
			title: "infra-lead", role: "lead", status: "running", online: true, owner: true,
			tags: []string{"backend"}, taskURL: otherTaskURL, taskLabel: "PR #386"},
		{convID: "c2000000-0000-4000-8000-000000000002", label: "lbl-in-dev", tmux: "tmux-in-dev",
			title: "infra-dev-db", role: "dev", online: false,
			tags: []string{"backend", "sqlite"}},
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

	seedPalette(t, f)
	seedProcessDashSnap(t, f)
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
    var b = det.querySelector('.force-fold-btn[data-act="toggle-force-fold"]');
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
			Key:      "bounded-jobs-normal",
			Title:    "Bounded Preact — Jobs normal",
			Caption:  "Jobs island completed its initial request and rendered its normal fixture state.",
			JS:       boundedTabJS("jobs", "#cron-create-open"),
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
			Key:      "bounded-costs-normal",
			Title:    "Bounded Preact — Costs normal",
			Caption:  "Costs island completed its request and rendered controls for the fixture's empty-cost span.",
			JS:       boundedTabJS("costs", "#costs-factor"),
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
			Key:      "processes-runs",
			Title:    "Processes — runs",
			Caption:  "Processes Runs sub-view with a populated live run row, status, current activity, and viewer action.",
			JS:       processTabJS("runs", `[data-process-run="dashsnap-release-42"]`),
			SettleMS: 900,
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
		{
			Key:     "process-editor-commands",
			Title:   "Process editor — contextual commands",
			Caption: "TCL-435: the shared dashboard command palette contributes selection-aware graph operations, searchable plain copy, disabled reasons, and the documented editor launcher.",
			JS: processEditorStateJS(`ed.setSelection({type: 'node', id: 'begin'});
  ed.commandsButton.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input = document.querySelector('#palette-input');
  if (!input || !document.querySelector('#command-palette-modal.show')) throw new Error('process command palette did not open');
  input.value = 'selection'; input.dispatchEvent(new InputEvent('input', {bubbles:true}));
  if (!document.querySelector('.palette-item[aria-disabled="false"]')) throw new Error('enabled selection command missing');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-edge-drop-chooser",
			Title:   "Process editor — connector-drop chooser",
			Caption: "TCL-433: releasing a connector on empty canvas anchors the searchable, keyboard-accessible complete node vocabulary at the intended graph coordinate.",
			JS: processEditorStateJS(`var p={x:360,y:260},r=ed.graph.svg.getBoundingClientRect();
  ed.openConnectedNodeChooser({nodeId:'begin',port:'out'},p,{clientX:r.left+ed.graph.view.x+p.x*ed.graph.view.k,clientY:r.top+ed.graph.view.y+p.y*ed.graph.view.k});
  await Promise.resolve();
  var chooser=document.querySelector('.process-node-chooser');
  if(!chooser||document.activeElement!==chooser.querySelector('.process-node-chooser-input')) throw new Error('edge-drop chooser did not open or focus');
  if(chooser.querySelectorAll('[role="option"]').length!==5) throw new Error('edge-drop chooser vocabulary incomplete');`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-commands-wizard",
			Title:   "Process editor — contextual commands (wizard)",
			Caption: "The same TCL-435 command registry under the wizard skin: arcane labels remain searchable by ordinary vocabulary and disabled context stays visibly explained.",
			Wizard:  true,
			JS: processEditorStateJS(`ed.setSelection(null);
  ed.commandsButton.click();
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
			Caption: "Real Chrome input: an inspector edit blurs on the first node click, the clicked node remains selected after pointer capture retargeting, and Delete confirms/removes that node.",
			JS: processEditorStateJS(`ed.setSelection({type:'node',id:'begin'}); window.__browserEd=ed;
  var input=ed.inspector.querySelector('.process-inspector-input'); input.value='';`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "click", Selector: ".process-inspector-input"},
				{Kind: "input", Selector: ".process-inspector-input", Text: "Renamed begin"},
				{Kind: "click", Selector: `.process-node[data-node-id="ship"] .process-node-shape`},
				{Kind: "eval", JS: `var ed=window.__browserEd;
          if(ed.selection?.type!=='node'||ed.selection.id!=='ship') throw new Error('trusted node click did not select ship');
          if(!ed.graph.nodeLayer.querySelector('[data-node-id="ship"]').classList.contains('is-selected')) throw new Error('ship highlight missing');
          if(ed.model.node('begin').name!=='Renamed begin') throw new Error('inspector blur change did not commit');`},
				{Kind: "key", Key: "Delete"},
				{Kind: "eval", JS: `if(!document.querySelector('.process-editor-modal')) throw new Error('Delete did not open node confirmation');`},
				{Kind: "click", Selector: ".process-editor-modal .confirm-danger"},
				{Kind: "eval", JS: `if(window.__browserEd.model.node('ship')) throw new Error('confirmed node Delete did not remove ship');`},
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
          if(ed.selection?.type!=='edge') throw new Error('trusted edge click did not select an edge');
          if(!ed.inspector.querySelector('.process-inspector-kind')?.textContent.includes('edge')) throw new Error('edge inspector missing');
          window.__edgeKey=[ed.selection.from,ed.selection.outcome];`},
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
				{Kind: "key-down", Key: "Control"},
				{Kind: "click", Selector: ".process-edge .process-edge-hit"},
				{Kind: "key-up", Key: "Control"},
				{Kind: "eval", JS: `var ed=window.__browserEd,items=ed.selection?.items||[];
          if(ed.selection?.type!=='multi'||items.length!==2) throw new Error('trusted modifier click did not create two-item selection');
          if(ed.graph.root.querySelectorAll('.is-selected').length!==2) throw new Error('multi-selection highlights out of sync');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-drag-live",
			Title:   "Process editor — live multi-drag connectors",
			Caption: "Real Chrome drag held mid-frame: both selected nodes move, their internal connector/label/arrow geometry follows, and the model remains unmodified until release.",
			JS: processEditorStateJS(`window.__browserEd=ed; var items=ed.graph.layout.nodes.map(function(n){return {type:'node',id:n.id};}); ed.setSelection({type:'multi',items:items});
  window.__dragBefore={path:ed.graph.edgeLayer.querySelector('.process-edge-path').getAttribute('d'),rev:ed.model.rev,undo:ed.model.undoStack.length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 120, DY: 65, Steps: 6},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
          if(!ed.graph.transientLayout) throw new Error('drag did not create transient edge layout');
          if(ed.graph.edgeLayer.querySelector('.process-edge-path').getAttribute('d')===b.path) throw new Error('connector stayed frozen mid-drag');
          if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('drag frame mutated model/undo');`},
			},
			SettleMS: 100,
		},
		{
			Key:     "process-editor-browser-drag-commit",
			Title:   "Process editor — atomic drag commit",
			Caption: "Real Chrome drag release commits the selected nodes once, clears transient routing, and consumes exactly one undo slot.",
			JS: processEditorStateJS(`window.__browserEd=ed; var items=ed.graph.layout.nodes.map(function(n){return {type:'node',id:n.id};}); ed.setSelection({type:'multi',items:items});
  window.__dragBefore={rev:ed.model.rev,undo:ed.model.undoStack.length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 100, DY: 55, Steps: 5},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('pre-release model mutation');`},
				{Kind: "mouse-up"},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
          if(ed.model.undoStack.length!==b.undo+1) throw new Error('drag release was not one atomic undo step');
          if(ed.graph.transientLayout) throw new Error('transient routing survived release');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-browser-drag-cancel",
			Title:   "Process editor — cancelled drag restore",
			Caption: "A browser drag followed by pointercancel restores node and connector geometry completely and leaves model revision/undo untouched.",
			JS: processEditorStateJS(`window.__browserEd=ed; window.__dragBefore={
    path:ed.graph.edgeLayer.querySelector('.process-edge-path').getAttribute('d'),
    transform:ed.graph.nodeLayer.querySelector('[data-node-id="begin"]').getAttribute('transform'),
    rev:ed.model.rev,undo:ed.model.undoStack.length};`),
			Actions: []dashsnap.BrowserAction{
				{Kind: "mouse-down", Selector: `.process-node[data-node-id="begin"] .process-node-shape`},
				{Kind: "move-by", DX: 90, DY: 50, Steps: 5},
				{Kind: "eval", JS: `var ed=window.__browserEd,id=ed.graph.pointer?.id;if(id==null) throw new Error('drag pointer missing before cancel');
          ed.graph.svg.dispatchEvent(new PointerEvent('pointercancel',{pointerId:id,bubbles:true}));`},
				{Kind: "eval", JS: `var ed=window.__browserEd,b=window.__dragBefore;
          if(ed.graph.edgeLayer.querySelector('.process-edge-path').getAttribute('d')!==b.path) throw new Error('cancel did not restore connector');
          if(ed.graph.nodeLayer.querySelector('[data-node-id="begin"]').getAttribute('transform')!==b.transform) throw new Error('cancel did not restore node');
          if(ed.model.rev!==b.rev||ed.model.undoStack.length!==b.undo) throw new Error('cancel mutated model/undo');`},
			},
			SettleMS: 300,
		},
		{
			Key:     "process-editor-marquee-multi",
			Title:   "Process editor — marquee multi-selection",
			Caption: "A left-drag marquee selects several nodes at once; every selected node has an accent outline and the inspector summarizes the current set.",
			JS: processEditorStateJS(`var items = ed.graph.layout.nodes.map(function(node){ return {type:'node',id:node.id}; }); ed.setSelection({type:'multi',items:items});
  var box = document.createElementNS('http://www.w3.org/2000/svg','rect'); box.setAttribute('class','process-marquee'); box.setAttribute('x','40'); box.setAttribute('y','20'); box.setAttribute('width','430'); box.setAttribute('height','360'); ed.graph.viewport.append(box);`),
			SettleMS: 1100,
		},
		{
			Key:     "process-editor-template-settings",
			Title:   "Process editor — template name mid-edit",
			Caption: "Template metadata editor mid-rename: immutable id plus a focused, changed-but-uncommitted display name alongside description and documentation.",
			JS: processEditorStateJS(`ed.setSelection({type: 'template'});
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
  ed.externalReviewPanel.hidden = false; ed.renderExternalChange();
  if (ed.externalChange.kind !== 'clean' || !ed.externalChange.review) throw new Error('clean external review state missing');`),
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
  ed.externalReviewPanel.hidden = false; ed.renderExternalChange();
  if (ed.externalChange.kind !== 'dirty' || ed.externalKeepButton.hidden || !ed.externalChange.review) throw new Error('dirty external review actions missing');`),
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
			JS:       nodeDialogStateJS(`ed.openNodeSettings('implement');` + nodeDialogSelfCheck("agent")),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-resized",
			Title:   "Process node dialog — resized workspace",
			Caption: "TCL-419: the standard persisted resize affordance expands the compound task editor into a two-column workspace; the scroll body and fixed action row remain usable in both skins.",
			JS: nodeDialogStateJS(`ed.openNodeSettings('implement');` + nodeDialogSelfCheck("agent") + `
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
  ed.openNodeSettings('implement');` + nodeDialogSelfCheck("human") + nodeDialogScrollToWork),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-task-program",
			Title:   "Process node dialog — task, program work performer",
			Caption: "The shared performer editor keyed to program: command + per-line arguments and the explicit ⚠ command-execution security note (§10) — scrolled to the work section.",
			JS: nodeDialogStateJS(`ed.model.updateNode('implement', function(n){
    n.performer = {kind: 'program', profile: 'ci', run: 'go', args: ['test', './...']};
  });
  ed.openNodeSettings('implement');` + nodeDialogSelfCheck("program") + `
  if (!document.querySelector('.process-node-security-note')) throw new Error('program security note missing');` + nodeDialogScrollToWork),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-dialog-decision",
			Title:   "Process node dialog — decision node",
			Caption: "Decision node dialog: the decider performer (human, with choices) and the read-only choices → edges mapping pointing at the canvas for topology edits.",
			JS: nodeDialogStateJS(`ed.openNodeSettings('escalate');` + `
  var choiceHead = Array.from(document.querySelectorAll('.process-node-section-title')).find(function(el){ return el.textContent === 'choices → edges'; });
  if (!choiceHead) throw new Error('decision dialog missing the choices → edges section');`),
			SettleMS: 1200,
		},
		{
			Key:     "process-node-card-readonly",
			Title:   "Process node detail card — read-only mode",
			Caption: "The exact same component in view mode (the viewer's node detail card): read-only badge, every control disabled, zero duplicated markup — the §9 unlock later flips this flag back to edit.",
			JS: nodeDialogStateJS(`ed.model.config.nodeEditable = function(){ return false; };
  ed.openNodeSettings('implement');
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
			Title:   "Groups tab — legacy inline editor boundary",
			Caption: "TCL-357 (self-checked): the description editor remains styled in both skins while its exact Preact-managed chip stays connected and hidden behind the transient input.",
			JS: showGroups + expandGroups + `document.body.classList.add('dock-open');` + `return (async function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  var chip = det && det.querySelector(':scope > summary .group-descr');
  if (!chip) throw new Error('groups-inline-editor: description chip missing');
  chip.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var input = det.querySelector(':scope > summary .group-descr-input');
  if (!input) throw new Error('groups-inline-editor: input did not open');
  if (!chip.isConnected || !chip.hidden) throw new Error('groups-inline-editor: managed chip was detached instead of hidden');
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
  var btn = det.querySelector('.force-fold-btn[data-act="toggle-force-fold"]');
  if (!btn) throw new Error('force-folded: no 🎯 toggle button in the action row');
  btn.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var det2 = document.querySelector('details[data-group-key="frontend-squad"]');
  if (det2.querySelector(':scope > .subtable > .group-force-block')) throw new Error('force-folded: card still present after folding');
  var btn2 = det2.querySelector('.force-fold-btn.folded[data-act="toggle-force-fold"]');
  if (!btn2) throw new Error('force-folded: toggle did not enter its .folded state');
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
  while (!document.querySelector('.group-default-profile-select') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 60); });
  }
  if (!document.querySelector('.group-default-profile-select')) throw new Error('chip-keyboard: Enter did not open the profile picker');
})();`},
				{Kind: "key", Key: "Escape"},
				{Kind: "eval", JS: `var ae = document.activeElement;
  if (!ae || !ae.classList.contains('group-default-model')) throw new Error('chip-keyboard: Escape did not hand focus back to the chip');`},
			},
			SettleMS: 400,
		},
		{
			Key:   "dock-open",
			Title: "Palette dock open",
			// JOH-390 items 4/5/7: the dock head hosts the re-homed groups-toolbar
			// globals ("+ new group" + ⚙ cog on row 1, the 🧠 default-profile chip
			// on row 2); the profiles heading is spelled out ("Agent profiles" /
			// "Familiar patterns"); the top-bar Palette toggle is gone.
			Caption: "Palette dock expanded (groups collapsed): re-homed + new group / ⚙ cog / 🧠 default-profile in the head, full 'Agent profiles' heading, no top-bar toggle.",
			JS:      showGroups + collapseGroups + `document.body.classList.add('dock-open');`,
		},
		{
			Key:   "dock-collapsed",
			Title: "Palette dock collapsed",
			// JOH-390 item 4: collapsed, the re-homed controls render back in the
			// toolbar exactly as before (the only reopen affordance is the edge tab).
			Caption: "Palette dock collapsed, members expanded — main list reclaims the width; + new group / ⚙ cog / 🧠 default-profile are back in the toolbar.",
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
	}
	return append(states, processGraphStates()...)
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

// processEditorStateJS opens the seeded release-train template in the graph
// editor (Processes tab → Templates → open) and waits for the lazily imported
// editor to mount its canvas, then runs extraJS with `ed` bound to the editor
// instance (its dashsnap/test handle) to drive selection/dirty states.
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
  %s
})();`, extraJS)
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

func boundedTabJS(tab, readySelector string) string {
	return fmt.Sprintf(`return Promise.all([
  import('/static/js/snapshot-store.js'),
  import('/static/js/feature-state-registry.js')
]).then(async function(modules) {
var store = modules[0], registry = modules[1];
var __tab = document.querySelector('nav [data-tab=%q]');
if (!__tab) throw new Error('missing bounded tab: %s');
if (%q === 'plugins' || %q === 'costs') {
  store.dashboardState.snapshot.value = Object.assign({}, store.dashboardState.snapshot.value, {
    plugins_tab_visible: true, cost_tab_visible: true
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
});`, tab, tab, tab, tab, readySelector, tab, tab, tab, tab, tab)
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
