package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// ctxInfo mirrors the daemon's single-agent context wire shape
// (/v1/whoami/context and /v1/agent/{sel}/context).
type ctxInfo struct {
	ConvID            string  `json:"conv_id"`
	SessionID         string  `json:"session_id"`
	ContextPct        float64 `json:"context_pct"`
	TokensInput       int64   `json:"tokens_input"`
	TokensOutput      int64   `json:"tokens_output"`
	ContextWindowSize int64   `json:"context_window_size"`
	CallerConv        string  `json:"caller_conv"`
}

// ctxGroupEntry mirrors the daemon's groupContextEntry wire shape
// (/v1/groups/{name}/context).
type ctxGroupEntry struct {
	ConvID            string  `json:"conv_id"`
	Title             string  `json:"title"`
	Role              string  `json:"role"`
	Online            bool    `json:"online"`
	HasSnapshot       bool    `json:"has_snapshot"`
	ContextPct        float64 `json:"context_pct"`
	TokensInput       int64   `json:"tokens_input"`
	TokensOutput      int64   `json:"tokens_output"`
	ContextWindowSize int64   `json:"context_window_size"`
	Model             string  `json:"model"`
}

func findCtxEntry(entries []ctxGroupEntry, convID string) *ctxGroupEntry {
	for i := range entries {
		if entries[i].ConvID == convID {
			return &entries[i]
		}
	}
	return nil
}

// Scenario: a lead that OWNS the group reads a worker's context window
// via the manager-pattern --target route — the core use case (watch a
// worker approach its limit). No agent.context-info slug needed; group
// ownership is the structural bypass.
//
// Also asserts the self route (/v1/whoami/context) still serves the
// worker its own snapshot after the writeContextInfo refactor, and that
// caller_conv is echoed only on the cross-agent read.
func TestContextInfo_OwnerReadsWorkerTarget(t *testing.T) {
	f := newFlow(t)

	const lead = "ctxl-aaaa-bbbb-cccc-dddd"
	const worker = "ctxw-aaaa-bbbb-cccc-dddd"
	const label = "lbl-ctxw"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, label, "tmux-ctxw", "/tmp/ctxw")
	f.HaveMember("squad", worker)
	// Lead is an owner of the group — the bypass that lets it read member
	// context without the slug.
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")

	// The statusline hook wrote the worker's context snapshot.
	require.NoError(t,
		db.UpdateContextSnapshot(label, 72.0, 130000, 14000, 200000),
		"seed worker snapshot")

	// Self route: the worker reads its own state — no caller_conv.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/context", nil), worker))
	require.Equal(t, http.StatusOK, rec.Code, "whoami/context: body=%s", rec.Body.String())
	var self ctxInfo
	testharness.DecodeJSON(t, rec, &self)
	assert.Equal(t, worker, self.ConvID, "self conv_id")
	assert.Equal(t, 72.0, self.ContextPct, "self context_pct")
	assert.Equal(t, int64(200000), self.ContextWindowSize, "self window size")
	assert.Empty(t, self.CallerConv, "self read carries no caller_conv")

	// Cross-agent route: the owning lead reads the worker's state.
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil), lead))
	require.Equal(t, http.StatusOK, rec.Code, "agent/context (owner): body=%s", rec.Body.String())
	var other ctxInfo
	testharness.DecodeJSON(t, rec, &other)
	assert.Equal(t, worker, other.ConvID, "target conv_id")
	assert.Equal(t, 72.0, other.ContextPct, "target context_pct")
	assert.Equal(t, int64(130000), other.TokensInput, "target tokens_input")
	assert.Equal(t, int64(14000), other.TokensOutput, "target tokens_output")
	assert.Equal(t, lead, other.CallerConv, "cross-agent read echoes caller_conv")
}

// Scenario: a group owner has an explicit DENY override on
// agent.context-info. Deny is always authoritative — it suppresses the
// owner bypass on both the per-target and the group read, the same
// universal precedence every cross-agent verb follows. The owner bypass
// only fills the "undecided" gap (no explicit grant or deny); it never
// beats an explicit deny.
func TestContextInfo_DenyOverrideBeatsOwnerBypass(t *testing.T) {
	f := newFlow(t)

	const lead = "ctxd-aaaa-bbbb-cccc-dddd"
	const worker = "ctxe-aaaa-bbbb-cccc-dddd"
	const label = "lbl-ctxe"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, label, "tmux-ctxe", "/tmp/ctxe")
	f.HaveMember("squad", worker)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")
	// An explicit deny override locks the owner out — deny always wins.
	require.NoError(t,
		db.SetAgentPermissionOverride(lead, agentd.PermAgentContextInfo, db.PermEffectDeny, "test"),
		"seed deny override")
	require.NoError(t,
		db.UpdateContextSnapshot(label, 64.0, 120000, 8000, 200000),
		"seed worker snapshot")

	// Per-target read: owner is denied despite ownership.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil), lead))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"deny override must beat the owner bypass on --target; body=%s", rec.Body.String())

	// Group listing: owner is denied despite ownership.
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups/squad/context", nil), lead))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"deny override must beat the owner bypass on --group; body=%s", rec.Body.String())
}

// Scenario: a plain co-member (not an owner, no slug) tries to read a
// peer's context via --target. Sharing a group is not enough — context
// reads require the slug or group ownership.
//
// Expected: 403. The human (no agent identity) always passes.
func TestContextInfo_TargetDeniedWithoutSlugOrOwnership(t *testing.T) {
	f := newFlow(t)

	const peer = "ctxp-aaaa-bbbb-cccc-dddd"
	const worker = "ctxv-aaaa-bbbb-cccc-dddd"
	const label = "lbl-ctxv"

	f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, label, "tmux-ctxv", "/tmp/ctxv")
	f.HaveMember("squad", worker)
	f.HaveMember("squad", peer) // co-member, but not an owner
	require.NoError(t,
		db.UpdateContextSnapshot(label, 40.0, 70000, 10000, 200000),
		"seed worker snapshot")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil), peer))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"co-member without slug/ownership should be 403; body=%s", rec.Body.String())

	// The human passes — no agent identity to gate.
	rec = testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil)))
	assert.Equal(t, http.StatusOK, rec.Code,
		"human cross-agent read should be allowed; body=%s", rec.Body.String())
}

// Scenario: a caller that shares NO group with the target but has been
// granted the agent.context-info slug reads its context via --target.
// The slug is the explicit cross-boundary capability (the owner bypass
// only covers a manager's own groups).
func TestContextInfo_TargetAllowedWithSlug(t *testing.T) {
	f := newFlow(t)

	const caller = "ctxc-aaaa-bbbb-cccc-dddd"
	const worker = "ctxt-aaaa-bbbb-cccc-dddd"
	const label = "lbl-ctxt"

	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, label, "tmux-ctxt", "/tmp/ctxt")
	f.HaveEnrolledAgent(worker)
	require.NoError(t,
		db.UpdateContextSnapshot(label, 88.0, 170000, 6000, 200000),
		"seed worker snapshot")

	// No shared group, no ownership — denied until the slug is granted.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil), caller))
	require.Equal(t, http.StatusForbidden, rec.Code,
		"pre-grant should be 403; body=%s", rec.Body.String())

	require.NoError(t,
		db.GrantAgentPermission(caller, agentd.PermAgentContextInfo, "test"),
		"grant agent.context-info")

	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+worker+"/context", nil), caller))
	require.Equal(t, http.StatusOK, rec.Code,
		"post-grant should be 200; body=%s", rec.Body.String())
	var info ctxInfo
	testharness.DecodeJSON(t, rec, &info)
	assert.Equal(t, 88.0, info.ContextPct, "target context_pct")
	assert.Equal(t, caller, info.CallerConv, "caller_conv echoed")
}

// Scenario: a lead reads its whole team's context at a glance via
// --group. One worker has reported a snapshot; another was just spawned
// and its statusline hook hasn't fired. The owner bypass authorises the
// read.
//
// Expected: an entry per member, with has_snapshot distinguishing the
// reported worker (true, real pct) from the fresh one (false, 0%) so the
// CLI renders "—" rather than a misleading 0%.
func TestGroupContext_OwnerSeesEveryMember(t *testing.T) {
	f := newFlow(t)

	const lead = "gcl0-aaaa-bbbb-cccc-dddd"
	const hot = "gch0-aaaa-bbbb-cccc-dddd"
	const fresh = "gcf0-aaaa-bbbb-cccc-dddd"
	const hotLabel = "lbl-gch0"
	const freshLabel = "lbl-gcf0"

	g := f.HaveGroup("team")
	f.HaveConvWithTitle(hot, "hot-worker")
	f.HaveAliveSession(hot, hotLabel, "tmux-gch0", "/tmp/gch0")
	f.HaveMember("team", hot)
	f.HaveConvWithTitle(fresh, "fresh-worker")
	f.HaveAliveSession(fresh, freshLabel, "tmux-gcf0", "/tmp/gcf0")
	f.HaveMember("team", fresh)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")

	// Only the hot worker has reported a snapshot.
	require.NoError(t,
		db.UpdateContextSnapshot(hotLabel, 91.0, 180000, 9000, 200000),
		"seed hot snapshot")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups/team/context", nil), lead))
	require.Equal(t, http.StatusOK, rec.Code, "group context (owner): body=%s", rec.Body.String())
	var entries []ctxGroupEntry
	testharness.DecodeJSON(t, rec, &entries)
	require.Len(t, entries, 2, "two members expected")

	hotEntry := findCtxEntry(entries, hot)
	require.NotNil(t, hotEntry, "hot worker missing from group context")
	assert.True(t, hotEntry.HasSnapshot, "hot worker has a snapshot")
	assert.Equal(t, 91.0, hotEntry.ContextPct, "hot worker context_pct")
	assert.Equal(t, int64(200000), hotEntry.ContextWindowSize, "hot worker window size")

	freshEntry := findCtxEntry(entries, fresh)
	require.NotNil(t, freshEntry, "fresh worker missing from group context")
	assert.False(t, freshEntry.HasSnapshot, "fresh worker has no snapshot yet")
	assert.Zero(t, freshEntry.ContextPct, "fresh worker reports 0% (unknown)")
}

// Scenario: a plain member (no ownership, no slug) asks for the group's
// context listing.
//
// Expected: 403 — the bulk context view is gated the same as the
// per-target read. The human always passes.
func TestGroupContext_DeniedForNonOwnerNonSlug(t *testing.T) {
	f := newFlow(t)

	const member = "gcm1-aaaa-bbbb-cccc-dddd"
	const worker = "gcw1-aaaa-bbbb-cccc-dddd"
	const label = "lbl-gcw1"

	f.HaveGroup("team")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, label, "tmux-gcw1", "/tmp/gcw1")
	f.HaveMember("team", worker)
	f.HaveMember("team", member)
	require.NoError(t,
		db.UpdateContextSnapshot(label, 55.0, 100000, 10000, 200000),
		"seed worker snapshot")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups/team/context", nil), member))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"plain member should be 403; body=%s", rec.Body.String())

	// The human sees the whole team.
	rec = testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups/team/context", nil)))
	require.Equal(t, http.StatusOK, rec.Code,
		"human group context should be allowed; body=%s", rec.Body.String())
	var entries []ctxGroupEntry
	testharness.DecodeJSON(t, rec, &entries)
	hotEntry := findCtxEntry(entries, worker)
	require.NotNil(t, hotEntry, "worker missing from human group context")
	assert.True(t, hotEntry.HasSnapshot, "worker snapshot surfaced to human")
	assert.Equal(t, 55.0, hotEntry.ContextPct, "worker context_pct")
}
