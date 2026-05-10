package agentd

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// pickTrayIcon's policy: priority yellow (pending) > orange (sudo) >
// green (idle). Tooltip carries the count of whichever colour fired
// (and the soonest-expiry hint for the orange path).
func TestPickTrayIcon_GreenWhenIdle(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 0, 0, "")
	if string(icon) != "green" {
		t.Errorf("idle: icon = %q, want green", icon)
	}
	if tooltip != "tclaude agentd" {
		t.Errorf("idle: tooltip = %q, want %q", tooltip, "tclaude agentd")
	}
}

func TestPickTrayIcon_YellowWhenPending(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 1, 0, "")
	if string(icon) != "yellow" {
		t.Errorf("pending=1: icon = %q, want yellow", icon)
	}
	if !strings.Contains(tooltip, "1 pending") {
		t.Errorf("pending=1: tooltip should mention count, got %q", tooltip)
	}
}

func TestPickTrayIcon_YellowCountUpdatesWithMultiple(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	_, tooltip := pickTrayIcon(green, yellow, orange, 7, 0, "")
	if !strings.Contains(tooltip, "7 pending") {
		t.Errorf("pending=7 tooltip should mention 7, got %q", tooltip)
	}
}

// pending=0, sudoActive>0 → orange + count + expiry hint. Pins the
// new state from v2 slice 4.
func TestPickTrayIcon_OrangeWhenSudoActive(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 0, 2, "soonest expires in 4m12s")
	if string(icon) != "orange" {
		t.Errorf("sudoActive=2: icon = %q, want orange", icon)
	}
	if !strings.Contains(tooltip, "2 active sudo") {
		t.Errorf("sudoActive=2: tooltip should mention count, got %q", tooltip)
	}
	if !strings.Contains(tooltip, "soonest expires in 4m12s") {
		t.Errorf("sudoActive=2: tooltip should include expiry hint, got %q", tooltip)
	}
}

// Yellow takes priority over orange when BOTH are non-zero — the
// human's blocked-on-popup state is more time-critical than the
// passive elevation reminder. Pins the precedence rule.
func TestPickTrayIcon_YellowBeatsOrange(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	orange := []byte("orange")
	icon, tooltip := pickTrayIcon(green, yellow, orange, 1, 3, "soonest expires in 1m")
	if string(icon) != "yellow" {
		t.Errorf("pending=1 sudoActive=3: icon = %q, want yellow (blocking > passive)", icon)
	}
	if !strings.Contains(tooltip, "1 pending") {
		t.Errorf("pending=1 sudoActive=3: tooltip should mention pending, got %q", tooltip)
	}
	if strings.Contains(tooltip, "sudo") {
		t.Errorf("yellow tooltip should NOT mention sudo (different state); got %q", tooltip)
	}
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
	if string(icon) != "orange" {
		t.Errorf("sudoActive=1, no hint: icon = %q, want orange", icon)
	}
	if !strings.Contains(tooltip, "1 active sudo") {
		t.Errorf("tooltip should mention count, got %q", tooltip)
	}
	if strings.Contains(tooltip, "expires in") {
		t.Errorf("tooltip should NOT have expiry hint when none provided, got %q", tooltip)
	}
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
	if len(got) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(got))
	}
	want := []string{"old", "mid", "new"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: got %q, want %q (full: %+v)", i, got[i].ID, w, got)
		}
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
	if !strings.Contains(label, "groups.spawn") {
		t.Errorf("label %q should mention perm", label)
	}
	if !strings.Contains(label, "alice") {
		t.Errorf("label %q should mention conv title", label)
	}
	if !strings.Contains(label, "ago") {
		t.Errorf("label %q should mention age", label)
	}
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
	if !strings.Contains(label, "12345678") {
		t.Errorf("label %q should fall back to 8-char conv prefix when no title", label)
	}
	if strings.Contains(label, "abcdef") {
		t.Errorf("label %q should truncate at 8 chars; full conv-id leaked", label)
	}
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
			if got := sliceEq(c.a, c.b); got != c.wantEq {
				t.Errorf("sliceEq(%v, %v) = %v, want %v", c.a, c.b, got, c.wantEq)
			}
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
	if got := r.pendingCount(); got != 0 {
		t.Errorf("after balanced add/delete, count = %d, want 0", got)
	}
}
