package agentd

import (
	"testing"
	"time"
)

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
