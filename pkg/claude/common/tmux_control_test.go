package common

import (
	"errors"
	"testing"
)

func TestParseSessionList(t *testing.T) {
	entries, err := parseSessionList([]string{"$1 0 agent one", "$12 1 corpse", "", "$4 0 __tclaude-control"})
	if err != nil {
		t.Fatal(err)
	}
	alive := aliveSessions(entries)
	if len(alive) != 1 {
		t.Fatalf("alive = %v", alive)
	}
	if _, ok := alive["agent one"]; !ok {
		t.Fatalf("alive = %v", alive)
	}
	// An empty pane_dead (no active pane) keeps the session; the hidden
	// control session never counts.
	entries, err = parseSessionList([]string{"$3  odd"})
	if err != nil || len(aliveSessions(entries)) != 1 {
		t.Fatalf("empty pane_dead: %v %v", entries, err)
	}
	if !hasForeignSession([]sessionListEntry{{name: "x"}}) || hasForeignSession([]sessionListEntry{{name: "__tclaude-control"}}) {
		t.Fatal("hasForeignSession")
	}
}

func TestParseSessionListFailsLoudly(t *testing.T) {
	for _, bad := range []string{
		"agent\t0",  // the old TAB format
		"agent_0",   // a sanitised TAB
		"$1 0",      // no name
		"$x 0 name", // bad id
		"$1 2 name", // bad pane_dead
		"1 0 name",  // missing $
	} {
		if _, err := parseSessionList([]string{"$1 0 good", bad}); !errors.Is(err, errSessionListFormat) {
			t.Errorf("%q: err = %v, want errSessionListFormat", bad, err)
		}
	}
}

func TestControlModeDisabledByEnv(t *testing.T) {
	for v, want := range map[string][2]bool{"0": {true, true}, "off": {true, true}, "1": {false, true}, "": {false, false}, "junk": {false, false}} {
		t.Setenv(ControlModeEnvVar, v)
		d, ok := ControlModeDisabledByEnv()
		if d != want[0] || ok != want[1] {
			t.Errorf("%q: (%v,%v), want %v", v, d, ok, want)
		}
	}
}
