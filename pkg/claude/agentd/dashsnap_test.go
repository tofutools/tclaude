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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDashSnap is the manually-run visual smoke harness. It is skipped unless
// TCLAUDE_DASHSNAP is set, so `go test ./...` (CI) compiles it but never drives a
// browser.
func TestDashSnap(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("visual smoke harness — set TCLAUDE_DASHSNAP=1 to run (needs a Linux headless Chrome)")
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
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States:  dashSnapStates(),
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
	const showGroups = `document.querySelector('nav button[data-tab="groups"]').click();`
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
	const ensureForceOpen = `(function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  if (det && det.open && !det.querySelector(':scope > .subtable > .group-force-block')) {
    var b = det.querySelector('.force-fold-btn[data-act="toggle-force-fold"]');
    if (b) b.click();
  }
})();`
	return []dashsnap.State{
		{
			Key:     "groups",
			Title:   "Groups tab",
			Caption: "Groups tab, members expanded: the task-force info card (mission/roles) atop frontend-squad, its 🎯 hide-info toggle in the action row, tf: chips, owner ★, online + offline, task links.",
			JS:      showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open');`,
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
			JS: showGroups + expandGroups + ensureForceOpen + `document.body.classList.add('dock-open');` + `(function(){
  var det = document.querySelector('details[data-group-key="frontend-squad"]');
  if (!det) throw new Error('force-folded: frontend-squad not found');
  if (!det.querySelector(':scope > .subtable > .group-force-block')) throw new Error('force-folded: expected an open force card before folding');
  var btn = det.querySelector('.force-fold-btn[data-act="toggle-force-fold"]');
  if (!btn) throw new Error('force-folded: no 🎯 toggle button in the action row');
  btn.click();
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
			// fold is set on the live <details>, and morph preserves it across the
			// 2s tick (open is live-owned), matching the persisted-prefs path.
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
	return `document.querySelector('nav button[data-tab="groups"]').click();
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
	return `document.querySelector('nav button[data-tab="jobs"]').click();
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
	return `document.querySelector('nav button[data-tab="groups"]').click();
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
//   (a) Up-flip clip guard — the reason toggleCardMenu measures #dock-body and
//       NOT the viewport. Force the dock body to overflow (short max-height),
//       scroll the LAST card to its fold, open that card's menu, and assert it
//       stays within #dock-body's bottom. A downward drop there spills under the
//       body's overflow:auto fold, so it only clears if the menu flipped up —
//       which the OLD viewport-measuring code would miss (the body bottom sits a
//       footer-height ABOVE window.innerHeight, so the spill stayed "on screen").
//   (b) Horizontal clip guard + the SCREENSHOT — restore the body, open the
//       FIRST profile card's menu, assert it stays within #agent-dock's width (a
//       narrow, right-anchored menu must fit), and leave it open for the capture.
func cardMenuJS() string {
	return `document.querySelector('nav button[data-tab="groups"]').click();
document.body.classList.add('dock-open');
return new Promise(function(resolve, reject){
  setTimeout(function(){
    var body = document.querySelector('#dock-body');
    var dock = document.querySelector('#agent-dock');
    if (!body || !dock) { reject(new Error('card-menu FAIL: dock shell missing')); return; }
    // (a) Force overflow, drive the LAST card to the fold, open its menu up-flipped.
    var prevMax = body.style.maxHeight;
    body.style.maxHeight = '200px';
    var cards = body.querySelectorAll('.dock-card');
    var last = cards[cards.length - 1];
    if (!last) { reject(new Error('card-menu FAIL: no cards')); return; }
    last.scrollIntoView({block: 'end'});
    var lastCog = last.querySelector('.dock-card-manage[data-dock-act="card-menu"]');
    lastCog.click();
    var lastMenu = last.querySelector('.dock-card-menu.open');
    if (!lastMenu) { reject(new Error('card-menu FAIL: bottom card menu did not open')); return; }
    var lm = lastMenu.getBoundingClientRect();
    var bb = body.getBoundingClientRect();
    var flipped = lastMenu.classList.contains('opens-up');
    var vFit = lm.bottom <= bb.bottom + 2;
    lastCog.click(); // close
    body.style.maxHeight = prevMax;
    body.scrollTop = 0;
    if (!vFit) { reject(new Error('card-menu FAIL: bottom card menu bottom=' + lm.bottom.toFixed(0) + ' clips past #dock-body bottom=' + bb.bottom.toFixed(0) + ' (opens-up=' + flipped + ')')); return; }
    // (b) Screenshot + horizontal guard on the first profile card.
    var card = body.querySelector('.dock-card[data-dock-kind="profiles"]');
    if (!card) { reject(new Error('card-menu FAIL: no profile card')); return; }
    card.querySelector('.dock-card-manage[data-dock-act="card-menu"]').click();
    var menu = card.querySelector('.dock-card-menu.open');
    if (!menu) { reject(new Error('card-menu FAIL: menu did not open')); return; }
    var m = menu.getBoundingClientRect();
    var d = dock.getBoundingClientRect();
    var hFit = m.left >= d.left - 2 && m.right <= d.right + 2;
    var o = document.createElement('div');
    o.style.cssText = 'position:fixed;left:8px;bottom:36px;z-index:999;background:#000;color:#0f0;font:13px monospace;padding:6px;';
    o.textContent = 'card-menu H-INSIDE=' + (hFit ? 'YES' : 'NO') + ' bottom-card V-FIT=' + (vFit ? 'YES' : 'NO') + ' (flip=' + (flipped ? 'up' : 'down') + ')';
    document.body.appendChild(o);
    if (hFit) resolve();
    else reject(new Error('card-menu FAIL: menu (' + m.left.toFixed(0) + '..' + m.right.toFixed(0) + ') clips past the dock (' + d.left.toFixed(0) + '..' + d.right.toFixed(0) + ')'));
  }, 200);
});
`
}

// cardCloneJS builds a self-checking state for the clone dialog: open a profile
// card's ⚙ menu, click its Clone item, and assert the generic new-name dialog
// (#clone-modal) opens with the name pre-filled. Rejects on a miss so a broken
// clone wiring fails the run. The captured PNG also lets a human eyeball the
// per-#id wizard chrome (which string pins can't see).
func cardCloneJS() string {
	return `document.querySelector('nav button[data-tab="groups"]').click();
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
    var modal = document.querySelector('#clone-modal.show');
    if (!modal) { reject(new Error('card-clone FAIL: clone dialog did not open')); return; }
    var name = document.querySelector('#clone-modal-name');
    if (!name || !name.value) { reject(new Error('card-clone FAIL: name not pre-filled')); return; }
    resolve();
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
document.querySelector('nav button[data-tab="groups"]').click();
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
if (!document.querySelector('#template-deploy-modal.show')) {
  throw new Error('summon dialog did not open (#template-deploy-modal.show absent)');
}
`, tfTemplate, tfTemplate, dropSel, dropSel)
	if copyMode {
		js += `
var __copy = document.querySelector('#template-deploy-modal input[name=template-deploy-mode][value=copy]');
if (!__copy) throw new Error('copy radio not found');
__copy.click();
`
	}
	return js
}
