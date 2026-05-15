package testharness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// Flow wraps a World with a small Given/When/Then DSL so flow tests
// read as a sequence of intent-named calls rather than raw HTTP +
// JSON plumbing. A test reads top-to-bottom:
//
//	f := newFlow(t)              // setup
//	f.HaveGroup("alpha")         // Given
//	sp := f.AsHuman().Spawn(…)   // When
//	f.AssertSentContains(…)      // Then
//
// testharness can't import agentd (would cycle), so the test owns
// the mux + peer wrappers and passes them in via NewFlow.
type Flow struct {
	T     *testing.T
	World *World
	Mux   http.Handler

	humanWrap func(*http.Request) *http.Request
	agentWrap func(*http.Request, string) *http.Request
	currPeer  func(*http.Request) *http.Request
}

// NewFlow wires a World + http.Handler + the agentd peer wrappers.
// `human` and `agent` are normally `agentd.AsHumanPeer` and
// `agentd.AsAgentPeer`. The default scope is human; per-call
// AsHuman / AsAgent override it.
func NewFlow(
	t *testing.T,
	w *World,
	mux http.Handler,
	human func(*http.Request) *http.Request,
	agent func(*http.Request, string) *http.Request,
) *Flow {
	t.Helper()
	return &Flow{
		T:         t,
		World:     w,
		Mux:       mux,
		humanWrap: human,
		agentWrap: agent,
		currPeer:  human,
	}
}

// SpawnerLike mirrors the agentd.Spawner interface here to avoid an
// import cycle (agentd_test imports testharness, so testharness
// can't import agentd directly). Go interfaces are structural; any
// concrete type satisfying agentd.Spawner satisfies this too, so a
// flow_setup_test.go can do `agentd.Spawn = mocks.Spawner` directly.
type SpawnerLike interface {
	SpawnNew(label, cwd string) error
	SpawnResume(convID, cwd string) error
}

// Mocks bundles the default boundary impls for the v2 simulators.
// Tests assign these to the production package vars
// (clcommon.Default, agentd.Spawn) at setup, with t.Cleanup
// restoring the originals.
type Mocks struct {
	// Tmux is a TmuxSim — answers has-session against an alive table,
	// routes send-keys to the attached CCSim, models kill-session.
	// Drop-in for clcommon.Default.
	Tmux clcommon.Tmux

	// Spawner builds CCSims on SpawnNew and re-attaches them on
	// SpawnResume. Drop-in for agentd.Spawn. Production poll loops
	// see SessionRow + alive flag the moment SpawnNew returns.
	Spawner SpawnerLike
}

// DefaultMocks builds the canonical mock set against this World.
// flow_setup_test.go does the package-var swap with t.Cleanup.
func (w *World) DefaultMocks(t *testing.T) Mocks {
	t.Helper()
	return Mocks{
		Tmux:    w.Tmux,
		Spawner: &simSpawner{t: t, w: w},
	}
}

// simSpawner is the SpawnerLike implementation backed by CCSim +
// TmuxSim. SpawnNew creates a fresh CCSim, writes the SessionRow the
// hook callback would have written in prod, and registers in
// TmuxSim. SpawnResume locates an existing CCSim or hydrates from
// disk and re-attaches under a fresh resume label.
type simSpawner struct {
	t *testing.T
	w *World
}

func (s *simSpawner) SpawnNew(label, cwd string) error {
	cc := NewCCSim(s.t, s.w.HomeDir, cwd)
	if err := cc.Start(); err != nil {
		return err
	}
	// Use cc.Cwd (post-default-substitution) so the SessionRow agrees
	// with the .jsonl's actual on-disk location. Otherwise an empty
	// body.Cwd leaves the row with cwd="" and downstream cwd lookups
	// can't derive the project dir.
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		ConvID:      cc.ConvID,
		Cwd:         cc.Cwd,
		Status:      "running",
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cc.Cwd, cc)
	s.w.CCs.Set(label, cc)
	return nil
}

func (s *simSpawner) SpawnResume(convID, cwd string) error {
	cc := s.w.CCs.GetByConvID(convID)
	if cc == nil {
		cc = HydrateCCSim(s.t, s.w.HomeDir, convID, cwd)
		s.w.CCs.SetByConvID(cc)
	}
	if err := cc.Start(); err != nil {
		return err
	}
	label := generateResumeLabel()
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		ConvID:      convID,
		Cwd:         cc.Cwd,
		Status:      "running",
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cc.Cwd, cc)
	s.w.CCs.Set(label, cc)
	return nil
}

// AsHuman returns a Flow scoped to the human peer (no claude
// ancestor). All requirePermission gates pass.
func (f *Flow) AsHuman() *Flow {
	cp := *f
	cp.currPeer = f.humanWrap
	return &cp
}

// AsAgent returns a Flow scoped to a specific agent conv-id.
// requirePermission resolves through the agent's grants.
func (f *Flow) AsAgent(convID string) *Flow {
	cp := *f
	cp.currPeer = func(r *http.Request) *http.Request {
		return f.agentWrap(r, convID)
	}
	return &cp
}

// -- Given (state setup) --

// HaveGroup creates an active agent group and returns its row.
func (f *Flow) HaveGroup(name string) *db.AgentGroup {
	f.T.Helper()
	if _, err := db.CreateAgentGroup(name, ""); err != nil {
		f.T.Fatalf("HaveGroup(%q): %v", name, err)
	}
	g, err := db.GetAgentGroupByName(name)
	if err != nil || g == nil {
		f.T.Fatalf("HaveGroup(%q) re-fetch: %v row=%v", name, err, g)
	}
	return g
}

// HaveMember inserts a (group, conv) row. Group must exist first.
func (f *Flow) HaveMember(group, convID, alias string) {
	f.T.Helper()
	g, err := db.GetAgentGroupByName(group)
	if err != nil || g == nil {
		f.T.Fatalf("HaveMember: group %q not found", group)
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID, ConvID: convID, Alias: alias,
	}); err != nil {
		f.T.Fatalf("HaveMember: %v", err)
	}
}

// HaveConvWithTitle drops a conv_index row carrying customTitle so
// downstream lookups (uniqueReincarnateTitle, FreshConvRow) resolve.
func (f *Flow) HaveConvWithTitle(convID, customTitle string) {
	f.T.Helper()
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		CustomTitle: customTitle,
		IndexedAt:   time.Now(),
	}); err != nil {
		f.T.Fatalf("HaveConvWithTitle: %v", err)
	}
}

// HaveAliveSession sets up a live agent at convID: builds a real
// CCSim (so its .jsonl exists and accepts /exit / /rename), writes
// the SessionRow so isConvOnline / pickAliveSession find it, and
// registers in TmuxSim. Used to precede Reincarnate / Delete / Clone
// tests that require a live pane on the target.
func (f *Flow) HaveAliveSession(convID, label, tmuxSession, cwd string) {
	f.T.Helper()
	cc := NewCCSimWithID(f.T, f.World.HomeDir, convID, cwd)
	if err := cc.Start(); err != nil {
		f.T.Fatalf("HaveAliveSession: cc.Start: %v", err)
	}
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Cwd:         cwd,
		Status:      "running",
	}); err != nil {
		f.T.Fatalf("HaveAliveSession: %v", err)
	}
	f.World.Tmux.Register(tmuxSession, cwd, cc)
	f.World.CCs.Set(label, cc)
}

// MarkOffline flips a tmux session off (handler side believes it's
// down). Useful between an action that left the conv online and an
// action that requires it offline (resume).
func (f *Flow) MarkOffline(tmuxSession string) {
	f.World.Tmux.MarkOffline(tmuxSession)
}

// -- When (actions) --

// SpawnResp parses POST /v1/groups/{name}/spawn.
type SpawnResp struct {
	Group       string `json:"group"`
	ConvID      string `json:"conv_id"`
	Label       string `json:"label"`
	TmuxSession string `json:"tmux_session"`
	Code        int    `json:"-"`
	Raw         []byte `json:"-"`
}

// TmuxTarget is the pane address used by injectTextAndSubmit.
func (s SpawnResp) TmuxTarget() string { return s.TmuxSession + ":0.0" }

// Spawn drives POST /v1/groups/{group}/spawn with the alias. Mocks
// must already be installed (DefaultMocks assigned to clcommon.Default
// and agentd.Spawn — see flow_setup_test.go).
func (f *Flow) Spawn(group, alias string) SpawnResp {
	f.T.Helper()
	rec := f.do(http.MethodPost,
		"/v1/groups/"+group+"/spawn",
		map[string]any{"alias": alias})
	var resp SpawnResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Spawn(%q,%q): status=%d body=%s", group, alias, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Spawn decode: %v body=%s", err, rec.Body.String())
	}
	if resp.ConvID == "" || resp.TmuxSession == "" {
		f.T.Fatalf("Spawn missing conv_id/tmux_session: %s", rec.Body.String())
	}
	return resp
}

// ResumeResp parses POST /v1/agent/{conv}/resume.
type ResumeResp struct {
	ConvID string `json:"conv_id"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	Code   int    `json:"-"`
	Raw    []byte `json:"-"`
}

// Resume drives POST /v1/agent/{conv}/resume.
func (f *Flow) Resume(convID string) ResumeResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/agent/"+convID+"/resume", nil)
	var resp ResumeResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Resume(%q): status=%d body=%s", convID, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Resume decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// ReincarnateResp parses POST /v1/agent/{conv}/reincarnate.
type ReincarnateResp struct {
	OldConv     string `json:"old_conv"`
	NewConv     string `json:"new_conv"`
	NewTitle    string `json:"new_title"`
	Label       string `json:"label"`
	TmuxSession string `json:"tmux_session"`
	Code        int    `json:"-"`
	Raw         []byte `json:"-"`
}

// TmuxTarget mirrors SpawnResp.TmuxTarget.
func (r ReincarnateResp) TmuxTarget() string { return r.TmuxSession + ":0.0" }

// Reincarnate drives POST /v1/agent/{target}/reincarnate. followUp
// is required by the daemon; pass a non-empty string (e.g. "fresh
// start") even when the test doesn't care about the handoff content.
func (f *Flow) Reincarnate(target, followUp string) ReincarnateResp {
	f.T.Helper()
	body := map[string]any{"follow_up": followUp}
	rec := f.do(http.MethodPost, "/v1/agent/"+target+"/reincarnate", body)
	var resp ReincarnateResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Reincarnate(%q): status=%d body=%s", target, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Reincarnate decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// CloneResp parses POST /v1/agent/{target}/clone. Note: clone has no
// `new_title` field — the per-group alias is computed and stored in
// agent_group_members; tests assert via AssertCloneAliasInGroup.
type CloneResp struct {
	OldConv     string   `json:"old_conv"`
	NewConv     string   `json:"new_conv"`
	Label       string   `json:"label"`
	TmuxSession string   `json:"tmux_session"`
	Copied      []string `json:"copied"`
	CopyConv    bool     `json:"copy_conv"`
	Code        int      `json:"-"`
	Raw         []byte   `json:"-"`
}

// TmuxTarget mirrors SpawnResp.TmuxTarget.
func (r CloneResp) TmuxTarget() string { return r.TmuxSession + ":0.0" }

// CloneFresh drives POST /v1/agent/{target}/clone with
// `no_copy_conv: true`, which skips the jsonl copy and spawns a
// brand-new CC instance. Identity migrates regardless. Used in
// tests to avoid wiring a fake convops.CopyConversationToPath.
func (f *Flow) CloneFresh(target, alias string) CloneResp {
	f.T.Helper()
	body := map[string]any{"no_copy_conv": true}
	if alias != "" {
		body["alias"] = alias
	}
	rec := f.do(http.MethodPost, "/v1/agent/"+target+"/clone", body)
	var resp CloneResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("CloneFresh(%q,%q): status=%d body=%s", target, alias, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("CloneFresh decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// DeleteResp parses DELETE /v1/agent/{conv}.
type DeleteResp struct {
	ConvID    string         `json:"conv_id"`
	Action    string         `json:"action"`
	DBCounts  map[string]int `json:"db_counts"`
	JSONLGone bool           `json:"jsonl_removed"`
	Code      int            `json:"-"`
	Raw       []byte         `json:"-"`
}

// Delete drives DELETE /v1/agent/{conv}/delete. force=true passes
// ?force=1 so live tmux sessions get killed before the row purge
// (production refuses-by-default to avoid racing teardown with the
// live agent's writes).
func (f *Flow) Delete(convID string, force bool) DeleteResp {
	f.T.Helper()
	path := "/v1/agent/" + convID + "/delete"
	if force {
		path += "?force=1"
	}
	rec := f.do(http.MethodDelete, path, nil)
	var resp DeleteResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Delete(%q,force=%v): status=%d body=%s", convID, force, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Delete decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// -- Then (assertions) --

// AssertSentContains waits up to timeout for a send-keys hit on
// `target` whose text contains `contains`. Fatals on miss with the
// captured log so root cause is visible at a glance.
func (f *Flow) AssertSentContains(target, contains string, timeout time.Duration) {
	f.T.Helper()
	if !f.World.Tmux.WaitForSendKeys(target, contains, timeout) {
		f.T.Fatalf("expected send-keys to %s containing %q within %s; got %+v",
			target, contains, timeout, f.World.Tmux.Sent())
	}
}

// AssertResumeSpawned asserts the resume call exercised the
// `tclaude session new -r` path (not the already-online short-circuit).
func (f *Flow) AssertResumeSpawned(r ResumeResp) {
	f.T.Helper()
	if r.Action != "resumed" {
		f.T.Errorf("Resume action = %q, want %q (raw=%s)", r.Action, "resumed", r.Raw)
	}
}

// AssertReincarnateTitle pins the new instance's auto-titled name —
// the bug class this guards is `r-1`-on-`-r-N` ancestors.
func (f *Flow) AssertReincarnateTitle(r ReincarnateResp, wantTitle string) {
	f.T.Helper()
	if r.NewTitle != wantTitle {
		f.T.Errorf("NewTitle = %q, want %q (raw=%s)", r.NewTitle, wantTitle, r.Raw)
	}
}

// AssertCloneAliasInGroup asserts the clone's member row in `group`
// has the expected alias. Clone-of-empty-alias is the canonical bug
// here: the daemon should fall back to the original conv's display
// title when the original member row had no alias, producing
// `<title>-c-N` rather than bare `c-N`.
func (f *Flow) AssertCloneAliasInGroup(c CloneResp, groupName, wantAlias string) {
	f.T.Helper()
	g, err := db.GetAgentGroupByName(groupName)
	if err != nil || g == nil {
		f.T.Fatalf("AssertCloneAliasInGroup: group %q not found", groupName)
	}
	m, err := db.FindMemberInGroup(g.ID, c.NewConv)
	if err != nil || m == nil {
		f.T.Fatalf("AssertCloneAliasInGroup: clone %s not found in %s", c.NewConv, groupName)
	}
	if m.Alias != wantAlias {
		f.T.Errorf("clone alias in %s = %q, want %q", groupName, m.Alias, wantAlias)
	}
}

// MemberView is the parsed shape of one row in
// GET /v1/groups/{name}/members — what `tclaude agent groups members`
// would render. The handler refreshes each member's Title from the
// underlying .jsonl (agent.FreshTitle) before responding, so a renamed
// or freshly-spawned member surfaces its real name here without a
// prior `conv ls` indexing pass.
type MemberView struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	Online bool   `json:"online"`
	Owner  bool   `json:"owner,omitempty"`
}

// PeerView is the parsed shape of one row in GET /v1/peers — what
// `tclaude agent ls` renders. Like MemberView, Title is refreshed from
// the .jsonl by the handler (agent.FreshTitle); Alias is the per-group
// handle and is empty for ungrouped online agents.
type PeerView struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
	Alias  string   `json:"alias,omitempty"`
	Role   string   `json:"role,omitempty"`
	Descr  string   `json:"descr,omitempty"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

// ListGroupMembers calls GET /v1/groups/{name}/members and returns
// the parsed list — the same shape `tclaude agent groups members`
// renders. Use AsHuman/AsAgent to scope the caller. Fatals on
// non-200.
func (f *Flow) ListGroupMembers(group string) []MemberView {
	f.T.Helper()
	rec := f.do(http.MethodGet, "/v1/groups/"+group+"/members", nil)
	if rec.Code != http.StatusOK {
		f.T.Fatalf("ListGroupMembers(%q): status=%d body=%s", group, rec.Code, rec.Body.String())
	}
	var out []MemberView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.T.Fatalf("ListGroupMembers decode: %v body=%s", err, rec.Body.String())
	}
	return out
}

// ListPeers calls GET /v1/peers and returns the parsed list — the same
// shape `tclaude agent ls` renders. Use AsHuman/AsAgent to scope the
// caller. Fatals on non-200.
func (f *Flow) ListPeers() []PeerView {
	f.T.Helper()
	rec := f.do(http.MethodGet, "/v1/peers", nil)
	if rec.Code != http.StatusOK {
		f.T.Fatalf("ListPeers: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out []PeerView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.T.Fatalf("ListPeers decode: %v body=%s", err, rec.Body.String())
	}
	return out
}

// FindPeer returns the PeerView for convID from ListPeers, or nil if
// the caller can't see it. Convenience for `agent ls` assertions.
func (f *Flow) FindPeer(convID string) *PeerView {
	f.T.Helper()
	for _, p := range f.ListPeers() {
		if p.ConvID == convID {
			return &p
		}
	}
	return nil
}

// AssertGroupMember asserts that `tclaude agent groups members <group>`
// shows convID with the expected alias and title. Polls because the
// .jsonl write that follows /rename is async; each poll re-hits the
// members handler, which refreshes the title from the .jsonl
// (agent.FreshTitle) before responding — so the loop converges on its
// own once the rename lands, with no test-side index priming.
func (f *Flow) AssertGroupMember(group, convID, wantAlias, wantTitle string, timeout time.Duration) {
	f.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		members := f.ListGroupMembers(group)
		for _, m := range members {
			if m.ConvID != convID {
				continue
			}
			if m.Alias == wantAlias && m.Title == wantTitle {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	last := f.ListGroupMembers(group)
	f.T.Fatalf("AssertGroupMember(%q, %s, alias=%q, title=%q): not found within %s; got %+v",
		group, convID, wantAlias, wantTitle, timeout, last)
}

// AssertNotGroupMember asserts that convID is NOT in
// `tclaude agent groups members <group>`. Used for reincarnate (old
// conv migrates off) and delete (target purged).
func (f *Flow) AssertNotGroupMember(group, convID string) {
	f.T.Helper()
	for _, m := range f.ListGroupMembers(group) {
		if m.ConvID == convID {
			f.T.Errorf("conv %s should not be in group %q; got member %+v",
				convID, group, m)
		}
	}
}

// AssertConvNotListed asserts that convID does not appear in the
// disk-scan output of `conv.ListSessions(projectDir)` — the same scan
// `tclaude conv ls` runs. Used post-delete to surface the orphan-
// .jsonl bug class: if removeJSONLBestEffort walks the wrong project
// dir and the .jsonl lingers, the next conv ls re-indexes the orphan
// and it shows up here.
//
// cwd is the cwd the conv was last running in (recorded on its
// SessionRow before delete; tests should capture it pre-delete).
func (f *Flow) AssertConvNotListed(convID, cwd string) {
	f.T.Helper()
	projectDir := convops.GetClaudeProjectPath(cwd)
	entries, err := conv.ListSessions(projectDir)
	if err != nil {
		// Project dir gone entirely is a stronger guarantee than no
		// matching entry; treat as success.
		return
	}
	for _, e := range entries {
		if e.SessionID == convID {
			f.T.Errorf("conv %s should not be listed by conv ls in %s after delete; got entry %+v",
				convID, projectDir, e)
		}
	}
}

// AssertDeleted does a post-delete sweep across the agent tables to
// confirm the row purge actually landed. Used after Flow.Delete.
func (f *Flow) AssertDeleted(convID string) {
	f.T.Helper()
	if perms, _ := db.ListAgentPermissionsForConv(convID); len(perms) != 0 {
		f.T.Errorf("agent_permissions still has rows for %s: %v", convID, perms)
	}
	if rows, _ := db.FindSessionsByConvID(convID); len(rows) != 0 {
		f.T.Errorf("sessions still has rows for %s: %d rows", convID, len(rows))
	}
	groups, _ := db.ListGroupsForConv(convID)
	if len(groups) != 0 {
		var names []string
		for _, g := range groups {
			names = append(names, g.Name)
		}
		f.T.Errorf("agent_group_members still has rows for %s in groups: %s",
			convID, strings.Join(names, ","))
	}
}

// -- Internals --

func (f *Flow) do(method, path string, body any) *httptest.ResponseRecorder {
	f.T.Helper()
	r := JSONRequest(f.T, method, path, body)
	if f.currPeer != nil {
		r = f.currPeer(r)
	}
	return Serve(f.Mux, r)
}
