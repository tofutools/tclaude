package agentd

import (
	"sync"
	"testing"
	"time"
)

// An expired read serves the last times and probes tmux in the background:
// the snapshot poll never waits for list-sessions after the first read.
func TestShellTimesExpiredReadDoesNotWaitForProbe(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	probes := 0
	release := make(chan struct{})
	c := &shellTimesCache{
		now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		probe: func() map[string]shellTimes {
			mu.Lock()
			probes++
			n := probes
			mu.Unlock()
			if n > 1 {
				<-release
			}
			return map[string]shellTimes{"s": {created: time.Unix(int64(n), 0)}}
		},
	}
	if got := c.get()["s"].created; !got.Equal(time.Unix(1, 0)) {
		t.Fatalf("first read probes inline: got %v", got)
	}
	mu.Lock()
	now = now.Add(2 * shellTimesTTL)
	mu.Unlock()

	done := make(chan map[string]shellTimes, 1)
	go func() { done <- c.get() }()
	select {
	case got := <-done:
		if !got["s"].created.Equal(time.Unix(1, 0)) {
			t.Fatalf("expired read serves the last times: got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired read blocked on the tmux probe")
	}
	c.get() // a read while the probe runs does not start another
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for !c.get()["s"].created.Equal(time.Unix(2, 0)) {
		if time.Now().After(deadline) {
			t.Fatal("background probe's times were never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 2 {
		t.Fatalf("probes = %d, want the inline one plus one background one", probes)
	}
}

func TestParseShellTimes(t *testing.T) {
	got := parseShellTimes("tclaude-groupterm-a\t100\t200\t300\n" +
		"tclaude-term-b\t100\t500\t300\n" +
		"tclaude-term-c\t0\t\n" +
		"old\t100\t200\n" +
		"\n")
	want := map[string]shellTimes{
		"tclaude-groupterm-a": {created: time.Unix(100, 0), activity: time.Unix(300, 0)}, // output after input
		"tclaude-term-b":      {created: time.Unix(100, 0), activity: time.Unix(500, 0)}, // input after output
		"tclaude-term-c":      {},                                                        // unrendered: blank
		"old":                 {created: time.Unix(100, 0), activity: time.Unix(200, 0)}, // no window field
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d sessions, want %d: %v", len(got), len(want), got)
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok || !g.created.Equal(w.created) || !g.activity.Equal(w.activity) {
			t.Errorf("%s: got %+v, want %+v", name, g, w)
		}
	}
}
