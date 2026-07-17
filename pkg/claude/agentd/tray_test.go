package agentd

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// pickTrayMode's policy priority: blink (agent blocked on the human:
// awaiting / errored / pending approval) > orange (sudo active) >
// yellow (all online agents idle) > green (working, or nothing online).
// Tooltip carries the count(s) of whichever state fired.

// No agents online, nothing pending: the default green.
func TestPickTrayMode_GreenWhenNothingOnline(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{}, 0, 0, "")
	assert.Equal(t, trayGreen, mode, "nothing online: mode")
	assert.Equal(t, "tclaude agentd", tooltip, "nothing online: tooltip")
}

func TestRunSystrayLoopRecoversNativePanic(t *testing.T) {
	err := runSystrayLoop(func() { panic("native teardown failed") })
	require.EqualError(t, err, "system tray panic: native teardown failed")
}

// At least one agent working → green, even when others are idle.
func TestPickTrayMode_GreenWhenAnyWorking(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 3, busy: 1, idle: 2}, 0, 0, "")
	assert.Equal(t, trayGreen, mode, "1 working: mode")
	assert.Contains(t, tooltip, "1 agent(s) working", "tooltip should mention working count")
}

// Online agents present and ALL idle → yellow (the quiet state).
func TestPickTrayMode_YellowWhenAllIdle(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 2, idle: 2}, 0, 0, "")
	assert.Equal(t, trayYellow, mode, "all idle: mode")
	assert.Contains(t, tooltip, "all 2 agent(s) idle", "tooltip should mention idle count")
}

// "not busy" must not be mistaken for "all idle": a counts where online
// exceeds the recognized idle bucket (a hypothetical status that counts
// toward online without being idle) stays green, not yellow. Pins the
// idle == online guard at the policy layer.
func TestPickTrayMode_NotAllIdleWhenOnlineExceedsIdle(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 2, idle: 1}, 0, 0, "")
	assert.Equal(t, trayGreen, mode, "online=2 idle=1 (one unclassified) → not yellow")
	assert.Equal(t, "tclaude agentd", tooltip, "unclassified-only: plain tooltip")
}

// A pending approval popup blinks (folded into "blocked on human").
func TestPickTrayMode_BlinkWhenPendingApproval(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 1, busy: 1}, 1, 0, "")
	assert.Equal(t, trayBlink, mode, "pending=1: mode")
	assert.Contains(t, tooltip, "1 pending approval(s)", "tooltip should mention pending")
}

// An agent awaiting permission/input blinks.
func TestPickTrayMode_BlinkWhenAwaiting(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 2, busy: 1, awaiting: 1}, 0, 0, "")
	assert.Equal(t, trayBlink, mode, "awaiting=1: mode")
	assert.Contains(t, tooltip, "1 awaiting input", "tooltip should mention awaiting")
}

// An errored agent blinks too (operator chose "needs attention").
func TestPickTrayMode_BlinkWhenErrored(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 1, errored: 1}, 0, 0, "")
	assert.Equal(t, trayBlink, mode, "errored=1: mode")
	assert.Contains(t, tooltip, "1 errored", "tooltip should mention errored")
}

// Blink tooltip composes all three sources in priority order.
func TestPickTrayMode_BlinkTooltipComposesSources(t *testing.T) {
	_, tooltip := pickTrayMode(agentTrayCounts{online: 4, awaiting: 2, errored: 1}, 3, 0, "")
	assert.Contains(t, tooltip, "2 awaiting input", "tooltip awaiting")
	assert.Contains(t, tooltip, "1 errored", "tooltip errored")
	assert.Contains(t, tooltip, "3 pending approval(s)", "tooltip pending")
	// Order: awaiting before errored before pending.
	assert.Less(t, strings.Index(tooltip, "awaiting"), strings.Index(tooltip, "errored"), "awaiting before errored")
	assert.Less(t, strings.Index(tooltip, "errored"), strings.Index(tooltip, "pending"), "errored before pending")
}

// sudoActive>0, nothing blocked → orange + count + expiry hint.
func TestPickTrayMode_OrangeWhenSudoActive(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 1, busy: 1}, 0, 2, "soonest expires in 4m12s")
	assert.Equal(t, trayOrange, mode, "sudoActive=2: mode")
	assert.Contains(t, tooltip, "2 active sudo", "tooltip should mention count")
	assert.Contains(t, tooltip, "soonest expires in 4m12s", "tooltip should include expiry hint")
}

// Blink takes priority over orange — a human blocked on a popup is more
// time-critical than the passive elevation reminder.
func TestPickTrayMode_BlinkBeatsOrange(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{online: 1, awaiting: 1}, 0, 3, "soonest expires in 1m")
	assert.Equal(t, trayBlink, mode, "awaiting + sudo: mode (blocking > passive)")
	assert.Contains(t, tooltip, "awaiting input", "tooltip should mention awaiting")
	assert.NotContains(t, tooltip, "sudo", "blink tooltip should NOT mention sudo (different state)")
}

// Orange takes priority over yellow — an active elevation reminder
// outranks the quiet all-idle state.
func TestPickTrayMode_OrangeBeatsYellow(t *testing.T) {
	mode, _ := pickTrayMode(agentTrayCounts{online: 2, idle: 2}, 0, 1, "")
	assert.Equal(t, trayOrange, mode, "all idle + sudo: orange wins over yellow")
}

// Orange tooltip without an expiry hint — the SELECT-then-format race
// in snapshotSudoTrayState collapses to "" hint. The icon still flips
// orange but the tooltip stays clean.
func TestPickTrayMode_OrangeWithoutExpiryHint(t *testing.T) {
	mode, tooltip := pickTrayMode(agentTrayCounts{}, 0, 1, "")
	assert.Equal(t, trayOrange, mode, "sudoActive=1, no hint: mode")
	assert.Contains(t, tooltip, "1 active sudo", "tooltip should mention count")
	assert.NotContains(t, tooltip, "expires in", "tooltip should NOT have expiry hint when none provided")
}

// renderTrayIcon maps a mode (+ blink phase) to icon bytes. Blink
// alternates green/red; every other mode is static.
func TestRenderTrayIcon(t *testing.T) {
	g, y, o, r := []byte("green"), []byte("yellow"), []byte("orange"), []byte("red")
	assert.Equal(t, "green", string(renderTrayIcon(trayGreen, false, g, y, o, r)), "green mode")
	assert.Equal(t, "yellow", string(renderTrayIcon(trayYellow, false, g, y, o, r)), "yellow mode")
	assert.Equal(t, "orange", string(renderTrayIcon(trayOrange, false, g, y, o, r)), "orange mode")
	assert.Equal(t, "red", string(renderTrayIcon(trayBlink, true, g, y, o, r)), "blink on → red")
	assert.Equal(t, "green", string(renderTrayIcon(trayBlink, false, g, y, o, r)), "blink off → green")
}

// countAgentStates reduces session rows + the live tmux set to per-status
// counts over ONLINE convs. Offline convs (no alive tmux row) are
// skipped; per-conv the most-recently-updated alive row wins.
func TestCountAgentStates(t *testing.T) {
	base := time.Now()
	rows := []*db.SessionRow{
		// online: working
		{ConvID: "a", TmuxSession: "tmux-a", Status: session.StatusWorking, UpdatedAt: base},
		// online: idle
		{ConvID: "b", TmuxSession: "tmux-b", Status: session.StatusIdle, UpdatedAt: base},
		// online: awaiting permission
		{ConvID: "c", TmuxSession: "tmux-c", Status: session.StatusAwaitingPermission, UpdatedAt: base},
		// online: errored
		{ConvID: "d", TmuxSession: "tmux-d", Status: session.StatusError, UpdatedAt: base},
		// online: main_agent_idle counts as busy
		{ConvID: "e", TmuxSession: "tmux-e", Status: session.StatusMainAgentIdle, UpdatedAt: base},
		// OFFLINE (tmux not alive) — skipped even though status is idle
		{ConvID: "f", TmuxSession: "tmux-f", Status: session.StatusIdle, UpdatedAt: base},
		// conv c has a second alive row, older + idle — most-recent (awaiting) must win
		{ConvID: "c", TmuxSession: "tmux-c", Status: session.StatusIdle, UpdatedAt: base.Add(-time.Minute)},
		// row with no tmux session — skipped
		{ConvID: "g", TmuxSession: "", Status: session.StatusWorking, UpdatedAt: base},
		// alive tmux row but status=exited (SessionEnd fired before tmux
		// teardown) — must NOT count toward online (would falsely tip yellow)
		{ConvID: "h", TmuxSession: "tmux-h", Status: session.StatusExited, UpdatedAt: base},
	}
	alive := map[string]struct{}{
		"tmux-a": {}, "tmux-b": {}, "tmux-c": {}, "tmux-d": {}, "tmux-e": {}, "tmux-h": {},
	}
	c := countAgentStates(rows, alive)
	assert.Equal(t, 5, c.online, "online convs (a,b,c,d,e; f offline, g no-tmux, h exited)")
	assert.Equal(t, 2, c.busy, "busy (a working + e main_agent_idle)")
	assert.Equal(t, 1, c.idle, "idle (b)")
	assert.Equal(t, 1, c.awaiting, "awaiting (c — newest row wins over the older idle one)")
	assert.Equal(t, 1, c.errored, "errored (d)")
}

// Per-conv pick across DISTINCT tmux sessions: when a conv's newest row
// is dead and an older row is alive (different tmux session names), the
// alive one is the only candidate and its status is what counts. Pins
// the load-bearing tray/dashboard agreement on which row wins.
func TestCountAgentStates_NewestDeadOlderAlive(t *testing.T) {
	base := time.Now()
	rows := []*db.SessionRow{
		// newest row for conv x — its tmux session is DEAD
		{ConvID: "x", TmuxSession: "tmux-x-new", Status: session.StatusWorking, UpdatedAt: base},
		// older row for conv x — its tmux session is ALIVE, status idle
		{ConvID: "x", TmuxSession: "tmux-x-old", Status: session.StatusIdle, UpdatedAt: base.Add(-time.Minute)},
	}
	alive := map[string]struct{}{"tmux-x-old": {}} // only the older session lives
	c := countAgentStates(rows, alive)
	assert.Equal(t, 1, c.online, "one online conv (via the alive older row)")
	assert.Equal(t, 0, c.busy, "newest (working) row is dead and filtered out")
	assert.Equal(t, 1, c.idle, "the alive older row's idle status is what counts")
}

func TestCountAgentStates_Empty(t *testing.T) {
	c := countAgentStates(nil, map[string]struct{}{})
	assert.Equal(t, agentTrayCounts{}, c, "no rows → zero aggregate")
}

// snapshot must order rows oldest-first so the longest-blocked
// popup lands at the top of the tray menu. Pins the sort key.
func TestApprovalRegistry_SnapshotSortsOldestFirst(t *testing.T) {
	now := time.Now()
	r := &approvalRegistry{pending: map[string]*approvalRequest{
		"new": {id: "new", createdAt: now.Add(-10 * time.Second)},
		"old": {id: "old", createdAt: now.Add(-2 * time.Minute)},
		"mid": {id: "mid", createdAt: now.Add(-1 * time.Minute)},
	}}
	got := r.snapshot()
	require.Len(t, got, 3, "snapshot len")
	want := []string{"old", "mid", "new"}
	for i, w := range want {
		assert.Equal(t, w, got[i].ID, "position %d (full: %+v)", i, got)
	}
}

// formatApprovalSlotLabel must include the perm, the who (title or
// short id), and an age. Pins the exact shape the tray menu surfaces.
func TestFormatApprovalSlotLabel_UsesConvTitleWhenPresent(t *testing.T) {
	row := pendingApprovalSummary{
		ID:        "abcdef0123",
		Perm:      "groups.spawn",
		AgentID:   "agt_1234567890abcdef",
		ConvTitle: "alice",
		ConvID:    "aaaa-bbbb",
		CreatedAt: time.Now().Add(-90 * time.Second),
	}
	label := formatApprovalSlotLabel(row)
	assert.Contains(t, label, "groups.spawn", "label should mention perm")
	assert.Contains(t, label, "agt_12345678", "label should lead caller metadata with the stable id")
	assert.Contains(t, label, "alice", "label should mention conv title")
	assert.Less(t, strings.Index(label, "agt_12345678"), strings.Index(label, "alice"),
		"stable id must appear before mutable title")
	assert.Contains(t, label, "ago", "label should mention age")
}

func TestFormatApprovalSlotLabel_ExplicitlyDegradesMissingIdentityAndTitle(t *testing.T) {
	row := pendingApprovalSummary{
		ID:          "id-001",
		Perm:        "agent.clone",
		ConvTitle:   "",
		ConvID:      "12345678abcdef",
		CallerState: approvalCallerMissing,
		CreatedAt:   time.Now(),
	}
	label := formatApprovalSlotLabel(row)
	assert.Contains(t, label, "12345678", "label should fall back to 8-char conv prefix when no title")
	assert.Contains(t, label, approvalTitleMissing)
	assert.Contains(t, label, "metadata missing")
	assert.NotContains(t, label, "abcdef", "label should truncate at 8 chars; full conv-id leaked")
}

// sliceEq is the change-detector the poller uses to decide whether to
// rebind slots. Pin its zero / equal / unequal behaviour.
func TestSliceEq(t *testing.T) {
	cases := []struct {
		name   string
		a, b   []string
		wantEq bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs empty", nil, []string{}, true},
		{"both empty", []string{}, []string{}, true},
		{"equal", []string{"x", "y"}, []string{"x", "y"}, true},
		{"different length", []string{"x"}, []string{"x", "y"}, false},
		{"different content", []string{"x", "y"}, []string{"x", "z"}, false},
		{"order matters", []string{"x", "y"}, []string{"y", "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.wantEq, sliceEq(c.a, c.b), "sliceEq(%v, %v)", c.a, c.b)
		})
	}
}

// pendingCount must be safe under concurrent add + remove. The tray
// poller reads it on a 200ms tick while popup goroutines are adding
// and clearing entries; a torn read would surface as either a
// flickering icon or — worse — a panic on the underlying map.
func TestApprovalRegistry_PendingCountConcurrent(t *testing.T) {
	r := &approvalRegistry{pending: map[string]*approvalRequest{}}

	var wg sync.WaitGroup
	const writers = 4
	const iters = 200

	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := string(rune('a'+id)) + ":" + string(rune('0'+(i%10)))
				r.mu.Lock()
				r.pending[key] = &approvalRequest{id: key}
				r.mu.Unlock()
				_ = r.pendingCount()
				r.mu.Lock()
				delete(r.pending, key)
				r.mu.Unlock()
			}
		}(w)
	}

	// Reader hammering pendingCount alongside the writers — the test
	// passes if the race detector doesn't fire.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = r.pendingCount()
			}
		}
	}()

	wg.Wait()
	close(done)

	// Final state should be empty since each writer balances add+delete.
	assert.Equal(t, 0, r.pendingCount(), "after balanced add/delete, count")
}
