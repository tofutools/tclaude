package agentd

import (
	"strings"
	"sync"
	"testing"
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
