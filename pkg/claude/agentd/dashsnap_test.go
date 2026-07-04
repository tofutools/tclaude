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
	return []dashsnap.State{
		{
			Key:     "groups",
			Title:   "Groups tab",
			Caption: "Groups tab, members expanded: tf:frontend-squad chips, owner ★, online + offline, task links.",
			JS:      showGroups + expandGroups + `document.body.classList.add('dock-open');`,
		},
		{
			Key:     "dock-open",
			Title:   "Palette dock open",
			Caption: "Palette dock expanded (groups collapsed) — profiles / roles / templates cards.",
			JS:      showGroups + collapseGroups + `document.body.classList.add('dock-open');`,
		},
		{
			Key:     "dock-collapsed",
			Title:   "Palette dock collapsed",
			Caption: "Palette dock collapsed, members expanded — main list reclaims the width.",
			JS:      showGroups + expandGroups + `document.body.classList.remove('dock-open');`,
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
