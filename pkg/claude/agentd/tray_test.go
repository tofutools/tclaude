package agentd

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pickTrayIcon's policy: priority yellow (pending) > orange (sudo) >
// green (idle). Tooltip carries the count of whichever colour fired
// (and the soonest-expiry hint for the orange path).
func TestPickTrayIcon_GreenWhenIdle(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 0, 0, "")
	assert.Equal(t, "green", string(icon), "idle: icon")
	assert.Equal(t, "tclaude agentd", tooltip, "idle: tooltip")
}

func TestPickTrayIcon_YellowWhenPending(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 1, 0, "")
	assert.Equal(t, "yellow", string(icon), "pending=1: icon")
	assert.Contains(t, tooltip, "1 pending", "pending=1: tooltip should mention count")
}

func TestPickTrayIcon_YellowCountUpdatesWithMultiple(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	_, tooltip := pickTrayIcon(green, yellow, orange, 7, 0, "")
	assert.Contains(t, tooltip, "7 pending", "pending=7 tooltip should mention 7")
}

// pending=0, sudoActive>0 → orange + count + expiry hint. Pins the
// new state from v2 slice 4.
func TestPickTrayIcon_OrangeWhenSudoActive(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 0, 2, "soonest expires in 4m12s")
	assert.Equal(t, "orange", string(icon), "sudoActive=2: icon")
	assert.Contains(t, tooltip, "2 active sudo", "sudoActive=2: tooltip should mention count")
	assert.Contains(t, tooltip, "soonest expires in 4m12s", "sudoActive=2: tooltip should include expiry hint")
}

// Yellow takes priority over orange when BOTH are non-zero — the
// human's blocked-on-popup state is more time-critical than the
// passive elevation reminder. Pins the precedence rule.
func TestPickTrayIcon_YellowBeatsOrange(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 1, 3, "soonest expires in 1m")
	assert.Equal(t, "yellow", string(icon), "pending=1 sudoActive=3: icon (blocking > passive)")
	assert.Contains(t, tooltip, "1 pending", "pending=1 sudoActive=3: tooltip should mention pending")
	assert.NotContains(t, tooltip, "sudo", "yellow tooltip should NOT mention sudo (different state)")
}

// Orange tooltip without an expiry hint — the SELECT-then-format
// race in snapshotSudoTrayState collapses to "" hint. Pins the
// fallback path so the icon still flips orange but the tooltip
// stays clean.
func TestPickTrayIcon_OrangeWithoutExpiryHint(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 0, 1, "")
	assert.Equal(t, "orange", string(icon), "sudoActive=1, no hint: icon")
	assert.Contains(t, tooltip, "1 active sudo", "tooltip should mention count")
	assert.NotContains(t, tooltip, "expires in", "tooltip should NOT have expiry hint when none provided")
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
		ConvTitle: "alice",
		ConvID:    "aaaa-bbbb",
		CreatedAt: time.Now().Add(-90 * time.Second),
	}
	label := formatApprovalSlotLabel(row)
	assert.Contains(t, label, "groups.spawn", "label should mention perm")
	assert.Contains(t, label, "alice", "label should mention conv title")
	assert.Contains(t, label, "ago", "label should mention age")
}

func TestFormatApprovalSlotLabel_FallsBackToShortIDWhenNoTitle(t *testing.T) {
	row := pendingApprovalSummary{
		ID:        "id-001",
		Perm:      "agent.clone",
		ConvTitle: "",
		ConvID:    "12345678abcdef",
		CreatedAt: time.Now(),
	}
	label := formatApprovalSlotLabel(row)
	assert.Contains(t, label, "12345678", "label should fall back to 8-char conv prefix when no title")
	assert.NotContains(t, label, "abcdef", "label should truncate at 8 chars; full conv-id leaked")
}

// sliceEq is the change-detector the poller uses to decide whether to
// rebind slots. Pin its zero / equal / unequal behaviour.
func TestSliceEq(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []string
		wantEq  bool
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
