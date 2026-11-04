package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestTerminalStatusMatchesDashboardRow pins the detached terminal's status
// projection to the dashboard row's: for each live-row case, the status, the
// status detail and the three background counts must agree, since both views
// show the same agent next to each other.
func TestTerminalStatusMatchesDashboardRow(t *testing.T) {
	setupTestDB(t)
	liveLedger := db.SubagentSet{
		"sub-1": {Type: "explore", Seen: time.Now()},
		"sub-2": {Type: "explore", Seen: time.Now()},
	}.Encode()
	staleLedger := db.SubagentSet{
		"sub-old": {Type: "explore", Seen: time.Now().Add(-2 * db.SubagentTTL)},
	}.Encode()

	cases := []struct {
		name       string
		status     string
		detail     string
		subagents  string
		count      int
		wantStatus string
		wantDetail string
		wantSubs   int
	}{
		{
			name:   "idle with live sub-agents holds up to main_agent_idle",
			status: session.StatusIdle, subagents: liveLedger,
			wantStatus: session.StatusMainAgentIdle,
			wantDetail: session.BackgroundActivityDetail(2, 0, 0), wantSubs: 2,
		},
		{
			name:   "main_agent_idle with an expired ledger settles down to idle",
			status: session.StatusMainAgentIdle, detail: "1 subagent running", subagents: staleLedger,
			wantStatus: session.StatusIdle, wantDetail: "",
		},
		{
			name:   "main_agent_idle re-renders its detail from the reconciled counts",
			status: session.StatusMainAgentIdle, detail: "5 subagents running", subagents: liveLedger,
			wantStatus: session.StatusMainAgentIdle,
			wantDetail: session.BackgroundActivityDetail(2, 0, 0), wantSubs: 2,
		},
		{
			name:   "pre-ledger row surfaces its raw count",
			status: session.StatusIdle, count: 3,
			wantStatus: session.StatusMainAgentIdle,
			wantDetail: session.BackgroundActivityDetail(3, 0, 0), wantSubs: 3,
		},
		{
			name:   "a human prompt keeps its own detail",
			status: session.StatusAwaitingInput, detail: "needs a decision", subagents: liveLedger,
			wantStatus: session.StatusAwaitingInput, wantDetail: "needs a decision", wantSubs: 2,
		},
		{
			// The drift this test was written for: the terminal copy shipped
			// the raw provenance string, so a detached window read
			// "awaiting input — app-server snapshot".
			name:   "implementation provenance is not shown as a detail",
			status: session.StatusAwaitingInput, detail: "app-server snapshot",
			wantStatus: session.StatusAwaitingInput, wantDetail: "",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &db.SessionRow{
				ID: "s-term-" + string(rune('a'+i)), ConvID: "conv-term-" + string(rune('a'+i)),
				TmuxSession: "pane-term-" + string(rune('a'+i)),
				Status:      tc.status, StatusDetail: tc.detail,
				SubagentsJSON: tc.subagents, SubagentCount: tc.count,
				CreatedAt: time.Now(), LastHook: time.Now(),
			}
			require.NoError(t, db.SaveSession(row))
			stored, err := db.LoadSession(row.ID)
			require.NoError(t, err)
			alive := map[string]struct{}{row.TmuxSession: {}}

			term, online := terminalStatusForSessions([]*db.SessionRow{stored}, alive)
			require.True(t, online)
			assert.Equal(t, tc.wantStatus, term.Status, "terminal status")
			assert.Equal(t, tc.wantDetail, term.StatusDetail, "terminal status detail")
			assert.Equal(t, tc.wantSubs, term.SubagentCount, "terminal sub-agent count")

			full := stateForConvInSessions([]*db.SessionRow{stored}, alive)
			assert.Equal(t, full.Status, term.Status, "terminal and dashboard status")
			assert.Equal(t, full.StatusDetail, term.StatusDetail, "terminal and dashboard detail")
			assert.Equal(t, full.SubagentCount, term.SubagentCount)
			assert.Equal(t, full.BgShellCount, term.BgShellCount)
			assert.Equal(t, full.MonitorCount, term.MonitorCount)
		})
	}
}
