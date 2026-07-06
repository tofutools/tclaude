package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dashSnapshot mirrors the relevant fields of agentd.snapshotPayload
// without importing the unexported type. Adding fields here is cheap
// when more assertions need them.
type dashSnapshot struct {
	Version              string             `json:"version"`
	Groups               []dashGroup        `json:"groups"`
	Agents               []dashAgent        `json:"agents"`
	Ungrouped            []dashAgent        `json:"ungrouped"`
	Conversations        []dashConversation `json:"conversations"`
	Retired              []dashRetired      `json:"retired"`
	Replaced             []dashReplaced     `json:"replaced"`
	Pending              []dashPending      `json:"pending"`
	Usage                dashUsage          `json:"usage"`
	Harnesses            []dashHarness      `json:"harnesses"`
	NotificationsEnabled bool               `json:"notifications_enabled"`
	CostTabVisible       bool               `json:"cost_tab_visible"`
	CostTabWhatIf        bool               `json:"cost_tab_whatif"`
	PluginsTabVisible    bool               `json:"plugins_tab_visible"`
	RemoteAccess         dashRemoteAccess   `json:"remote_access"`
}

// dashRemoteAccess mirrors agentd.dashboardRemoteAccess — the Config tab's
// remote-access runtime state (material generated? listener live?).
type dashRemoteAccess struct {
	MaterialExists bool   `json:"material_exists"`
	Running        bool   `json:"running"`
	RunningBind    string `json:"running_bind"`
}

// dashConversation mirrors agentd.dashboardConversation.
type dashConversation struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Online bool   `json:"online"`
}

// dashRetired mirrors agentd.dashboardRetiredAgent.
type dashRetired struct {
	ConvID       string `json:"conv_id"`
	Title        string `json:"title"`
	Online       bool   `json:"online"`
	RetiredBy    string `json:"retired_by,omitempty"`
	RetireReason string `json:"retire_reason,omitempty"`
}

// dashReplaced mirrors agentd.dashboardReplacedGen — one superseded
// predecessor generation on the snapshot's replaced[] list.
type dashReplaced struct {
	ConvID       string `json:"conv_id"`
	Title        string `json:"title"`
	Reason       string `json:"reason,omitempty"`
	ReplacedAt   string `json:"replaced_at,omitempty"`
	Online       bool   `json:"online"`
	ActorConvID  string `json:"actor_conv_id"`
	ActorTitle   string `json:"actor_title"`
	ActorRetired bool   `json:"actor_retired,omitempty"`
}

// dashPending mirrors agentd.dashboardPending — a not-yet-enrolled
// dashboard spawn surfaced on the snapshot's pending[] list (JOH-205).
type dashPending struct {
	Label     string `json:"label"`
	Group     string `json:"group,omitempty"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	Descr     string `json:"descr,omitempty"`
	Online    bool   `json:"online"`
	Cwd       string `json:"cwd,omitempty"`
	Harness   string `json:"harness,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type dashGroup struct {
	Name          string       `json:"name"`
	Descr         string       `json:"descr"`
	MaxMembers    int          `json:"max_members"`
	NotifyEnabled bool         `json:"notify_enabled"`
	Scribe        bool         `json:"scribe,omitempty"`
	Members       []dashMember `json:"members"`
}

type dashMember struct {
	AgentID         string    `json:"agent_id,omitempty"`
	ConvID          string    `json:"conv_id"`
	Title           string    `json:"title"`
	CreatedAt       string    `json:"created_at,omitempty"`
	Branch          string    `json:"branch,omitempty"`
	StartupDir      string    `json:"startup_dir,omitempty"`
	StartupBranch   string    `json:"startup_branch,omitempty"`
	CurrentDir      string    `json:"current_dir,omitempty"`
	BranchURL       string    `json:"branch_url,omitempty"`
	BranchPRNum     int       `json:"branch_pr_number,omitempty"`
	BranchPRURL     string    `json:"branch_pr_url,omitempty"`
	BranchPRState   string    `json:"branch_pr_state,omitempty"`
	TaskURL         string    `json:"task_ref_url,omitempty"`
	TaskLabel       string    `json:"task_ref_label,omitempty"`
	Tags            []string  `json:"tags,omitempty"`
	Online          bool      `json:"online"`
	Notify          string    `json:"notify,omitempty"`
	NotifyEffective bool      `json:"notify_effective"`
	State           dashState `json:"state"`
}

type dashAgent struct {
	ConvID          string    `json:"conv_id"`
	Title           string    `json:"title"`
	Branch          string    `json:"branch,omitempty"`
	StartupDir      string    `json:"startup_dir,omitempty"`
	StartupBranch   string    `json:"startup_branch,omitempty"`
	CurrentDir      string    `json:"current_dir,omitempty"`
	BranchURL       string    `json:"branch_url,omitempty"`
	BranchPRNum     int       `json:"branch_pr_number,omitempty"`
	BranchPRURL     string    `json:"branch_pr_url,omitempty"`
	BranchPRState   string    `json:"branch_pr_state,omitempty"`
	Online          bool      `json:"online"`
	Groups          []string  `json:"groups"`
	Notify          string    `json:"notify,omitempty"`
	NotifyEffective bool      `json:"notify_effective"`
	State           dashState `json:"state"`
}

// dashHarness mirrors the relevant fields of agentd.dashboardHarness.
type dashHarness struct {
	Name             string            `json:"name"`
	DisplayName      string            `json:"display_name"`
	Models           []string          `json:"models"`
	EffortLevels     []string          `json:"effort_levels"`
	SandboxModes     []string          `json:"sandbox_modes"`
	DefaultSandbox   string            `json:"default_sandbox"`
	SandboxModeHelp  map[string]string `json:"sandbox_mode_help"`
	ApprovalModes    []string          `json:"approval_modes"`
	DefaultApproval  string            `json:"default_approval"`
	ApprovalModeHelp map[string]string `json:"approval_mode_help"`
	CanRename        bool              `json:"can_rename"`
	CanCompact       bool              `json:"can_compact"`
	CanSandbox       bool              `json:"can_sandbox"`
	CanApproval      bool              `json:"can_approval"`
	CanRemoteControl bool              `json:"can_remote_control"`
}

// dashState mirrors the relevant fields of agentd.agentState.
type dashState struct {
	Status            string  `json:"status,omitempty"`
	StatusDetail      string  `json:"status_detail,omitempty"`
	SubagentCount     int     `json:"subagent_count,omitempty"`
	LastHook          string  `json:"last_hook,omitempty"`
	ContextPct        float64 `json:"context_pct,omitempty"`
	TokensInput       int64   `json:"tokens_input,omitempty"`
	TokensOutput      int64   `json:"tokens_output,omitempty"`
	ContextWindowSize int64   `json:"context_window_size,omitempty"`
	Model             string  `json:"model,omitempty"`
	EffortLevel       string  `json:"effort_level,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
	VirtualCostUSD    float64 `json:"virtual_cost_usd,omitempty"`
	ExitReason        string  `json:"exit_reason,omitempty"`
	Harness           string  `json:"harness,omitempty"`
	SandboxMode       string  `json:"sandbox_mode,omitempty"`
	RemoteControl     bool    `json:"remote_control,omitempty"`
}

// fetchSnapshotOnly fetches ONLY /api/snapshot (a single request). Use it when
// a test asserts on the snapshot's own fields or its request-level cost (e.g.
// the one-tmux-list invariant); fetchDashSnapshot bundles three extra requests
// for the moved lists and would inflate such counts.
func fetchSnapshotOnly(t *testing.T, mux http.Handler) dashSnapshot {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "/api/snapshot body=%s", rec.Body.String())
	var snap dashSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")
	return snap
}

func fetchDashSnapshot(t *testing.T, mux http.Handler) dashSnapshot {
	t.Helper()
	snap := fetchSnapshotOnly(t, mux)
	// The retired / conversations / replaced lists no longer ride on the 2s
	// snapshot — each moved to its own paginated endpoint (GET /api/retired,
	// /api/conversations, /api/replaced) so the poll stops shipping the full
	// lists. Re-assemble them here from those endpoints (unbounded, limit=0) so
	// the many existing assertions over snap.Retired/.Conversations/.Replaced
	// keep verifying the same data at its new real surface.
	snap.Conversations = fetchListRows[dashConversation](t, mux, "/api/conversations?limit=0")
	snap.Retired = fetchListRows[dashRetired](t, mux, "/api/retired?limit=0")
	snap.Replaced = fetchListRows[dashReplaced](t, mux, "/api/replaced?limit=0")
	return snap
}

func TestDashboardSnapshot_VersionSurfaced(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(buildversion.SetStampedVersion("v9.8.7-test"))

	newFlow(t)

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())
	assert.Equal(t, "v9.8.7-test", snap.Version)
}

// Scenario: an ENROLLED agent has a live tmux session but is NOT a
// member of any group. The dashboard's `/api/snapshot` must surface it
// under the `ungrouped[]` array so the `+ add member` overlay can pull
// it as a candidate without a second fetch.
//
// Pins the bug class where an ungrouped agent is invisible to the
// dashboard until the human manually adds it to a group.
func TestDashboardSnapshot_UngroupedSurfacesLooseConvs(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	// Scenario set-up:
	//   - "loose" — alive, enrolled, not in any group → expect in Ungrouped.
	//   - "joined" — alive, member of group "alpha" (HaveMember enrolls
	//     it) → NOT in Ungrouped.
	const looseConv = "loos-1111-2222-3333-4444"
	const joinedConv = "join-1111-2222-3333-4444"
	f.HaveConvWithTitle(looseConv, "loose-worker")
	f.HaveConvWithTitle(joinedConv, "joined-worker")
	f.HaveAliveSession(looseConv, "spwn-loose", "tmux-loose", "/tmp/loose")
	f.HaveAliveSession(joinedConv, "spwn-join", "tmux-join", "/tmp/join")
	f.HaveEnrolledAgent(looseConv)
	g := f.HaveGroup("alpha")
	_ = g
	f.HaveMember("alpha", joinedConv)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Find by conv-id.
	inUngrouped := func(conv string) bool {
		for _, a := range snap.Ungrouped {
			if a.ConvID == conv {
				return true
			}
		}
		return false
	}
	inAgents := func(conv string) bool {
		for _, a := range snap.Agents {
			if a.ConvID == conv {
				return true
			}
		}
		return false
	}

	if !assert.True(t, inUngrouped(looseConv),
		"loose conv %s should be in Ungrouped; got %d ungrouped rows", looseConv, len(snap.Ungrouped)) {
		for _, a := range snap.Ungrouped {
			t.Logf("  ungrouped row: %+v", a)
		}
	}
	assert.False(t, inUngrouped(joinedConv),
		"joined conv %s should NOT be in Ungrouped (it's in group alpha)", joinedConv)
	// Both should appear in the broader Agents list.
	assert.True(t, inAgents(looseConv), "loose conv %s should be in Agents (broader list)", looseConv)
	assert.True(t, inAgents(joinedConv), "joined conv %s should be in Agents (member of alpha)", joinedConv)
}

// Scenario: ungrouped[] carries offline agents too. The dashboard's
// virtual "Ungrouped" group is a membership-management surface — an
// agent that is enrolled, in no group, and offline must still be
// visible there so the human can drag it into a group. (The `+ add
// member` overlay applies its own online filter, so this does not
// leak offline rows into that live-roster picker.)
func TestDashboardSnapshot_UngroupedIncludesOfflineAgents(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const onlineConv = "onln-1111-2222-3333-4444"
	const offlineConv = "offl-1111-2222-3333-4444"
	f.HaveConvWithTitle(onlineConv, "online")
	f.HaveConvWithTitle(offlineConv, "offline")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	// Both are enrolled agents; one's tmux has since died.
	f.HaveEnrolledAgent(onlineConv)
	f.HaveEnrolledAgent(offlineConv)
	f.MarkOffline("tmux-offl")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	online := false
	offline := false
	for _, a := range snap.Ungrouped {
		if a.ConvID == onlineConv {
			online = true
		}
		if a.ConvID == offlineConv {
			offline = true
		}
	}
	assert.True(t, online, "online ungrouped agent should appear in ungrouped[]; got %d rows", len(snap.Ungrouped))
	assert.True(t, offline, "offline ungrouped agent %s should also appear in ungrouped[]", offlineConv)

	// Both still appear in the broader agents[] list.
	offlineInAgents := false
	for _, a := range snap.Agents {
		if a.ConvID == offlineConv {
			offlineInAgents = true
		}
	}
	assert.True(t, offlineInAgents, "offline enrolled agent %s should still be in agents[]", offlineConv)
}
