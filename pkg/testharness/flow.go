//go:build rewire

package testharness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// Mocks bundles the default replacement functions for the boundaries
// Phase 1/2 needs. Tests install them via `rewire.Func(t, target, m.X)`
// — rewire's scanner only walks `_test.go` files for those calls, so
// the harness can't install them itself.
type Mocks struct {
	// SpawnNew synthesises the side effect of `tclaude session new -d`:
	// writes a SessionRow with the spawned label + a fresh conv-id +
	// tmux session, marks tmux alive. Causes handleGroupSpawn /
	// reincarnate / clone-no-copy poll loops to succeed on the first
	// iteration.
	SpawnNew func(label, cwd string) error

	// SpawnResume is the resume counterpart. handleAgentResume's
	// success path doesn't poll for a row, so this just succeeds.
	// Tests that need to count invocations can wrap this.
	SpawnResume func(convID, cwd string) error

	// TmuxCmd is FakeTmux.Command — returns a no-op cmd, records
	// send-keys, dispatches has-session against the alive table.
	TmuxCmd func(args ...string) *exec.Cmd
}

// DefaultMocks builds the typical Phase-1/2 mock set against this
// World. Tests pass each field into rewire.Func from their own
// _test.go.
func (w *World) DefaultMocks(t *testing.T) Mocks {
	t.Helper()
	return Mocks{
		SpawnNew: func(label, cwd string) error {
			w.CC.MaterializeSpawn(t, label, cwd)
			return nil
		},
		SpawnResume: func(_, _ string) error { return nil },
		TmuxCmd:     w.Tmux.Command,
	}
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

// HaveAliveSession marks a tmux session alive in FakeTmux + writes a
// SessionRow so isConvOnline / pickAliveSession find it. Used to
// precede Reincarnate / Delete / Clone tests that require a live
// pane on the target.
func (f *Flow) HaveAliveSession(convID, label, tmuxSession, cwd string) {
	f.T.Helper()
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: tmuxSession,
		ConvID:      convID,
		Cwd:         cwd,
		Status:      "running",
	}); err != nil {
		f.T.Fatalf("HaveAliveSession: %v", err)
	}
	f.World.Tmux.MarkAlive(tmuxSession)
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
// must already be installed (DefaultMocks wired into rewire.Func).
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

// Reincarnate drives POST /v1/agent/{target}/reincarnate. Empty
// followUp == no handoff message body.
func (f *Flow) Reincarnate(target, followUp string) ReincarnateResp {
	f.T.Helper()
	body := map[string]any{}
	if followUp != "" {
		body["follow_up"] = followUp
	}
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
