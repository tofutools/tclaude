package agentd

import (
	"strings"
	"sync"
	"testing"
)

// pickTrayIcon's policy: pending=0 → green + base tooltip;
// pending>0 → yellow + count-aware tooltip.
func TestPickTrayIcon_GreenWhenIdle(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	icon, tooltip := pickTrayIcon(green, yellow, 0)
	if string(icon) != "green" {
		t.Errorf("pending=0: icon = %q, want green", icon)
	}
	if tooltip != "tclaude agentd" {
		t.Errorf("pending=0: tooltip = %q, want %q", tooltip, "tclaude agentd")
	}
}

func TestPickTrayIcon_YellowWhenPending(t *testing.T) {
	green := []byte("green")
	yellow := []byte("yellow")
	icon, tooltip := pickTrayIcon(green, yellow, 1)
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
	_, tooltip := pickTrayIcon(green, yellow, 7)
	if !strings.Contains(tooltip, "7 pending") {
		t.Errorf("pending=7 tooltip should mention 7, got %q", tooltip)
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
