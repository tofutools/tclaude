package testharness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	SpawnNew(label, cwd, effort, model, harness string) error
	SpawnResume(convID, cwd, effort, model, harness string) error
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

// SpawnNew builds the harness-appropriate pane sim, writes the SessionRow
// the production hook callback would have written, and registers in
// TmuxSim. harness=="codex" routes to a CodexSim + a harness="codex" row;
// everything else (""/"claude") keeps the CCSim path byte-for-byte as
// before the seam, so the production Spawner signature is satisfied with no
// behaviour change for Claude Code.
func (s *simSpawner) SpawnNew(label, cwd, effort, model, harness string) error {
	if harness == codexHarnessName {
		return s.spawnNewCodex(label, cwd, effort, model)
	}
	cc := NewCCSim(s.t, s.w.HomeDir, cwd)
	// The session row's ID is the agent's TCLAUDE_SESSION_ID — the
	// stable key the hook callback tracks conv-id rotations against.
	cc.SessionID = label
	if err := cc.Start(); err != nil {
		return err
	}
	// Capture the effort and model the spawn path threaded through,
	// keyed by the new conv-id, so a flow test can assert them — the
	// same way the cwd is observable via the SessionRow written below.
	s.w.RecordSpawnEffort(cc.ConvID, effort)
	s.w.RecordSpawnModel(cc.ConvID, model)
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

// SpawnResume re-attaches the matching sim by harness. A Codex conv
// relaunches its CodexSim (located by conv-id, or hydrated from the
// on-disk rollout); everything else re-attaches a CCSim exactly as before.
func (s *simSpawner) SpawnResume(convID, cwd, effort, model, harness string) error {
	if harness == codexHarnessName {
		return s.spawnResumeCodex(convID, cwd, effort, model)
	}
	cc := s.w.CCs.GetByConvID(convID)
	if cc == nil {
		cc = HydrateCCSim(s.t, s.w.HomeDir, convID, cwd)
		s.w.CCs.SetByConvID(cc)
	}
	if err := cc.Start(); err != nil {
		return err
	}
	// Same observability as SpawnNew: capture the effort and model the
	// resume path threaded through, keyed by the conv-id, so flow tests
	// can assert model inheritance on resume / clone-copy paths.
	s.w.RecordSpawnEffort(convID, effort)
	s.w.RecordSpawnModel(convID, model)
	label := generateResumeLabel()
	// Resume mints a fresh session row / TCLAUDE_SESSION_ID; track it.
	cc.SessionID = label
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

// codexHarnessName is the harness tag the production spawn path threads
// through for a Codex agent (matches harness.CodexName). Kept as a local
// literal so testharness stays free of a harness-package import, the same
// decoupling the package keeps from agentd.
const codexHarnessName = "codex"

// spawnNewCodex is SpawnNew's `--harness codex` branch: it builds a
// CodexSim (owns a date-indexed rollout .jsonl, implements PaneSim), writes
// the harness="codex" SessionRow the production hook callback would have
// written, registers in TmuxSim, and stashes the sim in World.Codexes.
func (s *simSpawner) spawnNewCodex(label, cwd, effort, model string) error {
	cx := NewCodexSim(s.t, s.w.HomeDir, cwd)
	if err := cx.Start(); err != nil {
		return err
	}
	// Mirror the CCSim path's observability: capture the effort/model the
	// spawn threaded, keyed by the new conv-id.
	s.w.RecordSpawnEffort(cx.ConvID, effort)
	s.w.RecordSpawnModel(cx.ConvID, model)
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		ConvID:      cx.ConvID,
		Cwd:         cx.Cwd,
		Status:      "running",
		// The tag the whole soft-stop / resume / identity path keys on:
		// harnessForConv resolves this to the Codex harness so a stop
		// injects `/quit`, and resume relaunches `--harness codex`.
		Harness: codexHarnessName,
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cx.Cwd, cx)
	s.w.Codexes.Set(label, cx)
	return nil
}

// spawnResumeCodex is SpawnResume's `--harness codex` branch: it re-attaches
// the existing CodexSim (or hydrates one from the on-disk rollout) under a
// fresh resume label, mirroring `codex resume <id>` reopening the rollout.
func (s *simSpawner) spawnResumeCodex(convID, cwd, effort, model string) error {
	cx := s.w.Codexes.GetByConvID(convID)
	if cx == nil {
		cx = HydrateCodexSim(s.t, s.w.HomeDir, convID, cwd)
		s.w.Codexes.SetByConvID(cx)
	}
	if err := cx.Start(); err != nil {
		return err
	}
	s.w.RecordSpawnEffort(convID, effort)
	s.w.RecordSpawnModel(convID, model)
	label := generateResumeLabel()
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		ConvID:      convID,
		Cwd:         cx.Cwd,
		Status:      "running",
		Harness:     codexHarnessName,
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cx.Cwd, cx)
	s.w.Codexes.Set(label, cx)
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
// A member has no per-group name — an agent's single name is its
// conversation title.
func (f *Flow) HaveMember(group, convID string) {
	f.T.Helper()
	f.HaveMemberWithRole(group, convID, "")
}

// HaveMemberWithRole inserts a (group, conv) row carrying a role —
// used by tests that exercise role-filtered multicast.
func (f *Flow) HaveMemberWithRole(group, convID, role string) {
	f.T.Helper()
	g, err := db.GetAgentGroupByName(group)
	if err != nil || g == nil {
		f.T.Fatalf("HaveMemberWithRole: group %q not found", group)
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID, ConvID: convID, Role: role,
	}); err != nil {
		f.T.Fatalf("HaveMemberWithRole: %v", err)
	}
}

// HaveEnrolledAgent marks convID as an active agent in
// agent_enrollment — the explicit "this conv is an agent" record. Use
// it for an UNGROUPED agent in a test; a grouped conv auto-enrolls via
// HaveMember (AddAgentGroupMember fires EnrollAgent).
func (f *Flow) HaveEnrolledAgent(convID string) {
	f.T.Helper()
	if err := db.EnrollAgent(convID, "test"); err != nil {
		f.T.Fatalf("HaveEnrolledAgent(%q): %v", convID, err)
	}
}

// HaveRetiredAgent enrolls convID and then retires it, leaving a
// retired enrollment row — the demoted-agent state.
func (f *Flow) HaveRetiredAgent(convID string) {
	f.T.Helper()
	if err := db.EnrollAgent(convID, "test"); err != nil {
		f.T.Fatalf("HaveRetiredAgent enroll(%q): %v", convID, err)
	}
	if _, err := db.RetireAgent(convID, "test", "test retire"); err != nil {
		f.T.Fatalf("HaveRetiredAgent retire(%q): %v", convID, err)
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

// HaveConvWithPrompt drops a conv_index row carrying only a first
// prompt — a "plain conversation" that was never /rename'd and has no
// summary. The firstPrompt is stored verbatim, exactly as Claude Code's
// .jsonl scan records it (inline system tags, newlines and all), so a
// listing surface's title-cleaning path is genuinely exercised.
func (f *Flow) HaveConvWithPrompt(convID, firstPrompt string) {
	f.T.Helper()
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		FirstPrompt: firstPrompt,
		IndexedAt:   time.Now(),
	}); err != nil {
		f.T.Fatalf("HaveConvWithPrompt: %v", err)
	}
}

// HaveAliveSession sets up a live agent at convID: builds a real
// CCSim (so its .jsonl exists and accepts /exit / /rename), writes
// the SessionRow so isConvOnline / pickAliveSession find it, and
// registers in TmuxSim. Used to precede Reincarnate / Delete / Clone
// tests that require a live pane on the target.
func (f *Flow) HaveAliveSession(convID, label, tmuxSession, cwd string) {
	f.T.Helper()
	f.HaveAliveSessionOnBranch(convID, label, tmuxSession, cwd, "")
}

// HaveAliveSessionOnBranch is HaveAliveSession plus a git branch: the
// CCSim stamps `branch` into a user turn's gitBranch field (exactly as
// real Claude Code stamps every turn), so a conv_index scan resolves
// the agent's worktree/branch the way production does. An empty branch
// behaves identically to HaveAliveSession (no extra turn written).
// Used by surfaces that render an agent's branch — `agent ls`,
// `agent groups members`, and the dashboard.
func (f *Flow) HaveAliveSessionOnBranch(convID, label, tmuxSession, cwd, branch string) {
	f.T.Helper()
	cc := NewCCSimWithID(f.T, f.World.HomeDir, convID, cwd)
	cc.GitBranch = branch
	// The session row's ID is the agent's TCLAUDE_SESSION_ID — the
	// stable key the hook callback tracks conv-id rotations against.
	cc.SessionID = label
	if err := cc.Start(); err != nil {
		f.T.Fatalf("HaveAliveSessionOnBranch: cc.Start: %v", err)
	}
	if branch != "" {
		// The initial summary turn carries no gitBranch; write one
		// branch-bearing user turn so a conv_index scan has something
		// to read the branch off of.
		if err := cc.WriteUserTurn("working on " + branch); err != nil {
			f.T.Fatalf("HaveAliveSessionOnBranch: WriteUserTurn: %v", err)
		}
	}
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Cwd:         cwd,
		Status:      "running",
	}); err != nil {
		f.T.Fatalf("HaveAliveSessionOnBranch: %v", err)
	}
	f.World.Tmux.Register(tmuxSession, cwd, cc)
	f.World.CCs.Set(label, cc)
}

// HaveAliveCodexSession is the Codex analog of HaveAliveSession: it stands
// up a live Codex-tagged session DETERMINISTICALLY — a CodexSim with a real
// rollout, an alive tmux registration, and a harness="codex" SessionRow —
// WITHOUT the async spawn post-init (no background /rename to race). Use it
// when a lifecycle test needs a Codex agent whose state is fully settled
// before the test acts (rename, reincarnate, compact). Returns the sim so
// the test can seed its threads state row (WriteThreadRow) or read back a
// rename (ThreadTitle).
func (f *Flow) HaveAliveCodexSession(convID, label, tmuxSession, cwd string) *CodexSim {
	f.T.Helper()
	cx := NewCodexSimWithID(f.T, f.World.HomeDir, convID, cwd)
	if err := cx.Start(); err != nil {
		f.T.Fatalf("HaveAliveCodexSession: cx.Start: %v", err)
	}
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Cwd:         cwd,
		Status:      "running",
		Harness:     codexHarnessName,
	}); err != nil {
		f.T.Fatalf("HaveAliveCodexSession: %v", err)
	}
	f.World.Tmux.Register(tmuxSession, cwd, cx)
	f.World.Codexes.Set(label, cx)
	return cx
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

// Spawn drives POST /v1/groups/{group}/spawn with the agent name (the
// title injected via /rename on the new pane). Mocks must already be
// installed (DefaultMocks assigned to clcommon.Default and
// agentd.Spawn — see flow_setup_test.go).
func (f *Flow) Spawn(group, name string) SpawnResp {
	f.T.Helper()
	rec := f.do(http.MethodPost,
		"/v1/groups/"+group+"/spawn",
		map[string]any{"name": name})
	var resp SpawnResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Spawn(%q,%q): status=%d body=%s", group, name, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Spawn decode: %v body=%s", err, rec.Body.String())
	}
	if resp.ConvID == "" || resp.TmuxSession == "" {
		f.T.Fatalf("Spawn missing conv_id/tmux_session: %s", rec.Body.String())
	}
	return resp
}

// SpawnHarness drives POST /v1/groups/{group}/spawn for a specific
// harness (e.g. "codex"). It is Spawn plus a `harness` field in the body,
// so the daemon resolves the spawn against that harness's registry and the
// simSpawner builds the matching pane sim. Like Spawn it fatals on a
// non-200 so a spawn-path regression surfaces at the call site.
func (f *Flow) SpawnHarness(group, name, harness string) SpawnResp {
	f.T.Helper()
	rec := f.do(http.MethodPost,
		"/v1/groups/"+group+"/spawn",
		map[string]any{"name": name, "harness": harness})
	var resp SpawnResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("SpawnHarness(%q,%q,%q): status=%d body=%s", group, name, harness, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("SpawnHarness decode: %v body=%s", err, rec.Body.String())
	}
	if resp.ConvID == "" || resp.TmuxSession == "" {
		f.T.Fatalf("SpawnHarness missing conv_id/tmux_session: %s", rec.Body.String())
	}
	return resp
}

// SpawnWith drives POST /v1/groups/{group}/spawn with an arbitrary
// JSON body and returns the parsed outcome WITHOUT fatal-on-error — so
// tests can exercise the failure paths (bad cwd, missing group, …).
// On 2xx the ConvID / TmuxSession fields are populated; on error the
// Code + Raw fields carry the daemon's response for assertion.
func (f *Flow) SpawnWith(group string, body map[string]any) SpawnResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/groups/"+group+"/spawn", body)
	var resp SpawnResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
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

// StopResp parses POST /v1/agent/{conv}/stop. Action distinguishes the
// graceful soft-stop ("soft_stopped") from the harness-has-no-soft-exit
// hard kill ("killed_no_soft_exit"), a force kill ("killed"), and the
// already-offline no-op ("skipped:already_offline").
type StopResp struct {
	ConvID  string `json:"conv_id"`
	Action  string `json:"action"`
	TmuxSes string `json:"tmux_session"`
	Detail  string `json:"detail,omitempty"`
	Code    int    `json:"-"`
	Raw     []byte `json:"-"`
}

// Stop drives POST /v1/agent/{conv}/stop. force=true passes ?force=1 for
// a hard kill-session; force=false is the soft stop (inject the harness's
// SoftExitCommand — CC's /exit, Codex's /quit). Fatals on a non-200.
func (f *Flow) Stop(convID string, force bool) StopResp {
	f.T.Helper()
	path := "/v1/agent/" + convID + "/stop"
	if force {
		path += "?force=1"
	}
	rec := f.do(http.MethodPost, path, nil)
	var resp StopResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("Stop(%q,force=%v): status=%d body=%s", convID, force, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("Stop decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// AssertSoftStopped asserts a Stop took the graceful soft-exit path
// (action "soft_stopped"), NOT the harness-has-no-soft-exit hard-kill
// fallback ("killed_no_soft_exit") or a force kill. This is the
// acceptance bar for a Codex graceful stop: the daemon must have injected
// `/quit`, not fallen back to kill-session.
func (f *Flow) AssertSoftStopped(r StopResp) {
	f.T.Helper()
	if r.Action != "soft_stopped" {
		f.T.Errorf("Stop action = %q, want %q (raw=%s)", r.Action, "soft_stopped", r.Raw)
	}
}

// ClearResp carries the conv-ids either side of a simulated /clear.
type ClearResp struct {
	OldConv string
	NewConv string
}

// Clear simulates Claude Code's /clear on the agent running under the
// given session label. It drives `/clear` into the agent's pane; the
// CCSim turns that into a conv-id rotation plus the real
// SessionEnd(reason=clear) / SessionStart(source=clear) hook sequence
// (see CCSim.clear), so the production hook callback's identity
// migration runs exactly as it would in a live session. Returns the
// old and new conv-ids. The CCSim is re-registered under the new
// conv-id so a later Resume can still locate it.
func (f *Flow) Clear(label string) ClearResp {
	f.T.Helper()
	cc := f.World.CCs.GetByLabel(label)
	if cc == nil {
		f.T.Fatalf("Clear: no CCSim registered under label %q", label)
		return ClearResp{} // unreachable: Fatalf exits the goroutine
	}
	oldConv := cc.ConvID
	// Type /clear into the pane exactly as a user (or the agent) would;
	// the buffered Enter flushes it through the CCSim's /clear handler.
	cc.Receive("/clear")
	cc.Receive("Enter")
	newConv := cc.ConvID
	if newConv == oldConv {
		f.T.Fatalf("Clear(%q): conv-id did not rotate (still %s)", label, oldConv)
	}
	f.World.CCs.SetByConvID(cc)
	return ClearResp{OldConv: oldConv, NewConv: newConv}
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

// ReincarnateWith drives POST /v1/agent/{target}/reincarnate with an
// arbitrary JSON body and returns the outcome WITHOUT fatal-on-error,
// so tests can exercise rejection paths (oversized or solo-pane-invalid
// follow-up). Mirrors CloneWith. On a non-200 the Code + Raw fields
// carry the daemon's response.
func (f *Flow) ReincarnateWith(target string, body map[string]any) ReincarnateResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/agent/"+target+"/reincarnate", body)
	var resp ReincarnateResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// RenameResp parses POST /v1/agent/{conv}/rename. Returns WITHOUT
// fatal-on-error so tests can assert both the success (200) and the
// no-store / rejected-title failure paths.
type RenameResp struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Code   int    `json:"-"`
	Raw    []byte `json:"-"`
}

// Rename drives POST /v1/agent/{conv}/rename with an explicit title.
func (f *Flow) Rename(convID, title string) RenameResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/agent/"+convID+"/rename", map[string]any{"title": title})
	var resp RenameResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// CompactResp parses POST /v1/agent/{conv}/compact. Code/Raw carry the
// daemon's response so a test can assert the harness-unsupported 400 (a
// harness with no CompactCommand) as readily as the CC success path.
type CompactResp struct {
	Code int    `json:"-"`
	Raw  []byte `json:"-"`
}

// Compact drives POST /v1/agent/{conv}/compact WITHOUT fatal-on-error.
func (f *Flow) Compact(convID string) CompactResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/agent/"+convID+"/compact", nil)
	return CompactResp{Code: rec.Code, Raw: rec.Body.Bytes()}
}

// CloneResp parses POST /v1/agent/{target}/clone. Note: clone has no
// `new_title` field — the clone's title is computed (`<base>-c-<N>`)
// and injected via /rename asynchronously; tests assert it via
// AssertCloneTitle once the rename lands.
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
//
// The clone's title (`<base>-c-<N>`) is derived from the original by
// the daemon — there is no name to pass in.
func (f *Flow) CloneFresh(target string) CloneResp {
	f.T.Helper()
	body := map[string]any{"no_copy_conv": true}
	rec := f.do(http.MethodPost, "/v1/agent/"+target+"/clone", body)
	var resp CloneResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		f.T.Fatalf("CloneFresh(%q): status=%d body=%s", target, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.T.Fatalf("CloneFresh decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// CloneWith drives POST /v1/agent/{target}/clone with an arbitrary
// JSON body and returns the outcome WITHOUT fatal-on-error — so tests
// can exercise the failure paths (bad cwd override, …) and inspect a
// successful clone's fields. On error the Code + Raw fields carry the
// daemon's response.
func (f *Flow) CloneWith(target string, body map[string]any) CloneResp {
	f.T.Helper()
	rec := f.do(http.MethodPost, "/v1/agent/"+target+"/clone", body)
	var resp CloneResp
	resp.Code = rec.Code
	resp.Raw = rec.Body.Bytes()
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
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

// AssertCloneTitle asserts the clone surfaces the expected title on
// the `tclaude agent groups members <group>` surface. The clone's
// title is derived as `<base>-c-<N>` from the original's title and
// injected via /rename asynchronously, so this polls — each poll
// re-hits the members handler, which refreshes the title from the
// .jsonl (agent.FreshTitle). Clone-of-untitled-original is the
// canonical edge: it should yield a bare `c-<N>`.
func (f *Flow) AssertCloneTitle(c CloneResp, groupName, wantTitle string, timeout time.Duration) {
	f.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range f.ListGroupMembers(groupName) {
			if m.ConvID == c.NewConv && m.Title == wantTitle {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	last := f.ListGroupMembers(groupName)
	f.T.Fatalf("AssertCloneTitle(%s, group=%q, title=%q): not found within %s; got %+v",
		c.NewConv, groupName, wantTitle, timeout, last)
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
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	// Branch is the agent's *current* git branch; StartupBranch is the
	// launch dir's branch and StartupDir / CurrentDir are the launch
	// vs. live-worktree directories — see agentd.agentLocationView.
	Branch        string `json:"branch,omitempty"`
	StartupDir    string `json:"startup_dir,omitempty"`
	StartupBranch string `json:"startup_branch,omitempty"`
	CurrentDir    string `json:"current_dir,omitempty"`
	Online        bool   `json:"online"`
	Owner         bool   `json:"owner,omitempty"`
}

// PeerView is the parsed shape of one row in GET /v1/peers — what
// `tclaude agent ls` renders. Like MemberView, Title is refreshed from
// the .jsonl by the handler (agent.FreshTitle).
type PeerView struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
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
// shows convID with the expected title. Polls because the .jsonl write
// that follows /rename is async; each poll re-hits the members handler,
// which refreshes the title from the .jsonl (agent.FreshTitle) before
// responding — so the loop converges on its own once the rename lands,
// with no test-side index priming.
func (f *Flow) AssertGroupMember(group, convID, wantTitle string, timeout time.Duration) {
	f.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		members := f.ListGroupMembers(group)
		for _, m := range members {
			if m.ConvID != convID {
				continue
			}
			if m.Title == wantTitle {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	last := f.ListGroupMembers(group)
	f.T.Fatalf("AssertGroupMember(%q, %s, title=%q): not found within %s; got %+v",
		group, convID, wantTitle, timeout, last)
}

// Lookup calls GET /v1/lookup?selector=<sel> — the surface
// `tclaude agent lookup` renders. Returns the resolved conv-id (empty
// on a miss) and the HTTP status code, so a test can assert both a
// hit and a miss.
func (f *Flow) Lookup(selector string) (convID string, code int) {
	f.T.Helper()
	rec := f.do(http.MethodGet, "/v1/lookup?selector="+url.QueryEscape(selector), nil)
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.ConvID, rec.Code
}

// AssertResolvesByTitle polls GET /v1/lookup until `title` resolves to
// wantConv. The /rename that sets a freshly-spawned agent's title is
// async and resolution-by-title needs the .jsonl scanned into
// conv_index — so this converges once the rename lands, no test-side
// index priming required.
func (f *Flow) AssertResolvesByTitle(title, wantConv string, timeout time.Duration) {
	f.T.Helper()
	deadline := time.Now().Add(timeout)
	var lastConv string
	var lastCode int
	for time.Now().Before(deadline) {
		lastConv, lastCode = f.Lookup(title)
		if lastCode == http.StatusOK && lastConv == wantConv {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.T.Fatalf("AssertResolvesByTitle(%q): want conv %s, got conv=%q code=%d after %s",
		title, wantConv, lastConv, lastCode, timeout)
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
