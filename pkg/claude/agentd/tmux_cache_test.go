package agentd

import (
	"errors"
	"testing"
	"time"
)

// A fake clock the cache reads via its now func; tests advance it by hand so
// TTL boundaries are exact rather than wall-clock-dependent.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time  { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// TestTmuxSessionCache_CoalescesWithinTTL pins the three cache invariants the
// dashboard poll relies on: within the TTL repeated get()s reuse one probe
// (the tick's parallel handlers coalesce), once the TTL elapses the next get()
// re-probes, and the cached map is the one the probe returned.
func TestTmuxSessionCache_CoalescesWithinTTL(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	probes := 0
	want := map[string]struct{}{"tmux-a": {}, "tmux-b": {}}
	c := newTmuxSessionCache(500*time.Millisecond, clk.now, func() (map[string]struct{}, error) {
		probes++
		return want, nil
	})

	// First get is a cold miss → one probe.
	got, err := c.get()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probes != 1 {
		t.Fatalf("cold get: probes = %d, want 1", probes)
	}
	if len(got) != 2 {
		t.Fatalf("cold get: alive size = %d, want 2", len(got))
	}

	// Two more gets inside the TTL are hits → still one probe total. This is
	// the coalescing the tick's parallel handlers depend on.
	clk.add(100 * time.Millisecond)
	_, _ = c.get()
	clk.add(100 * time.Millisecond)
	_, _ = c.get()
	if probes != 1 {
		t.Fatalf("within-TTL gets: probes = %d, want 1 (cache must coalesce)", probes)
	}

	// Cross the TTL boundary: the next get re-probes.
	clk.add(500 * time.Millisecond)
	_, _ = c.get()
	if probes != 2 {
		t.Fatalf("post-expiry get: probes = %d, want 2 (TTL must expire)", probes)
	}
}

// TestTmuxSessionCache_ErrorCachedThenRecovers pins the error semantics: a
// probe error is cached for the TTL (so a wedged tmux server yields one failed
// fork per TTL, not a fork-storm), and the failure self-heals — after the TTL
// the next get re-probes and can succeed, so it never sticks permanently.
func TestTmuxSessionCache_ErrorCachedThenRecovers(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	probes := 0
	failNext := true
	c := newTmuxSessionCache(500*time.Millisecond, clk.now, func() (map[string]struct{}, error) {
		probes++
		if failNext {
			return nil, errors.New("tmux down")
		}
		return map[string]struct{}{"tmux-a": {}}, nil
	})

	// Cold miss hits the failing probe; the error (and nil map) is returned.
	got, err := c.get()
	if err == nil {
		t.Fatalf("expected error on first probe")
	}
	if got != nil {
		t.Fatalf("errored get: alive = %v, want nil", got)
	}

	// Within the TTL the error is served from cache — no second fork while
	// tmux is down.
	clk.add(100 * time.Millisecond)
	if _, err := c.get(); err == nil {
		t.Fatalf("expected cached error within TTL")
	}
	if probes != 1 {
		t.Fatalf("errored within-TTL: probes = %d, want 1 (error must be cached)", probes)
	}

	// tmux recovers; after the TTL the next get re-probes and succeeds — the
	// failure did not stick.
	failNext = false
	clk.add(500 * time.Millisecond)
	got, err = c.get()
	if err != nil {
		t.Fatalf("post-recovery get: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("post-recovery get: alive size = %d, want 1", len(got))
	}
	if probes != 2 {
		t.Fatalf("post-recovery: probes = %d, want 2", probes)
	}
}

// TestTmuxSessionCache_ZeroTTLTransparent pins the flag newFlow relies on: a
// TTL of 0 disables caching so every get re-probes, keeping production
// freshness semantics under the flow-test harness.
func TestTmuxSessionCache_ZeroTTLTransparent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	probes := 0
	c := newTmuxSessionCache(0, clk.now, func() (map[string]struct{}, error) {
		probes++
		return map[string]struct{}{}, nil
	})
	_, _ = c.get()
	_, _ = c.get()
	_, _ = c.get()
	if probes != 3 {
		t.Fatalf("zero-TTL: probes = %d, want 3 (cache must be transparent)", probes)
	}
}
