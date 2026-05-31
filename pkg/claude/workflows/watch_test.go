package workflows

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
)

func TestIsTerminalStatus(t *testing.T) {
	cases := map[ccworkflows.RunStatus]bool{
		ccworkflows.RunCompleted: true,
		ccworkflows.RunFailed:    true,
		ccworkflows.RunRunning:   false,
		ccworkflows.RunUnknown:   false,
	}
	for s, want := range cases {
		if got := isTerminalStatus(s); got != want {
			t.Errorf("isTerminalStatus(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestWatchInterval(t *testing.T) {
	if got := watchInterval(0); got != defaultWatchInterval {
		t.Errorf("watchInterval(0) = %v, want default %v", got, defaultWatchInterval)
	}
	if got := watchInterval(0.1); got != minWatchInterval {
		t.Errorf("watchInterval(0.1) = %v, want floor %v", got, minWatchInterval)
	}
	if got := watchInterval(3); got != 3*time.Second {
		t.Errorf("watchInterval(3) = %v, want 3s", got)
	}
}

func runState(id string, status ccworkflows.RunStatus) *ccworkflows.RunState {
	return &ccworkflows.RunState{
		RunID:  id,
		Status: status,
		Phases: []ccworkflows.Phase{{Index: 1, Title: "Run", Status: ccworkflows.AgentRunning}},
		Agents: []ccworkflows.Agent{{ID: "a1", Label: "a:one", PhaseIndex: 1, State: ccworkflows.AgentRunning}},
	}
}

func TestRunShowWatch_TerminalStopsImmediately(t *testing.T) {
	var out, errb bytes.Buffer
	waitCalls := 0
	code := runShowWatch("wf_x", &out, &errb, watchDeps{
		load: func(string) (*ccworkflows.RunState, *ccworkflows.RunRef, error) {
			return runState("wf_x", ccworkflows.RunCompleted), &ccworkflows.RunRef{}, nil
		},
		waitNext: func() bool { waitCalls++; return true },
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if waitCalls != 0 {
		t.Errorf("waitNext called %d times for a terminal run, want 0", waitCalls)
	}
	if !strings.Contains(out.String(), "no longer in flight") {
		t.Errorf("missing terminal banner:\n%s", out.String())
	}
}

func TestRunShowWatch_RunningThenInterrupted(t *testing.T) {
	var out, errb bytes.Buffer
	code := runShowWatch("wf_x", &out, &errb, watchDeps{
		load: func(string) (*ccworkflows.RunState, *ccworkflows.RunRef, error) {
			return runState("wf_x", ccworkflows.RunRunning), &ccworkflows.RunRef{}, nil
		},
		waitNext: func() bool { return false }, // interrupted before next frame
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "stopped watching") {
		t.Errorf("missing interrupted banner:\n%s", out.String())
	}
}

func TestRunShowWatch_RunningThenCompletes(t *testing.T) {
	var out, errb bytes.Buffer
	frames := 0
	code := runShowWatch("wf_x", &out, &errb, watchDeps{
		load: func(string) (*ccworkflows.RunState, *ccworkflows.RunRef, error) {
			frames++
			if frames == 1 {
				return runState("wf_x", ccworkflows.RunRunning), &ccworkflows.RunRef{}, nil
			}
			return runState("wf_x", ccworkflows.RunCompleted), &ccworkflows.RunRef{}, nil
		},
		waitNext: func() bool { return true }, // allow exactly the transition
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if frames != 2 {
		t.Errorf("rendered %d frames, want 2 (running → completed)", frames)
	}
	if !strings.Contains(out.String(), "no longer in flight") {
		t.Errorf("did not reach terminal:\n%s", out.String())
	}
}

func TestRunShowWatch_RecoversFromTransientError(t *testing.T) {
	// A run finishing mid-watch can briefly yield a half-written completed JSON:
	// the load tick that hits it errors, but the watch must keep polling and
	// succeed on the next frame, not abort.
	var out, errb bytes.Buffer
	calls := 0
	code := runShowWatch("wf_x", &out, &errb, watchDeps{
		load: func(string) (*ccworkflows.RunState, *ccworkflows.RunRef, error) {
			calls++
			if calls < 3 { // first two ticks fail transiently
				return nil, nil, errFake
			}
			return runState("wf_x", ccworkflows.RunCompleted), &ccworkflows.RunRef{}, nil
		},
		waitNext: func() bool { return true },
	})
	if code != 0 {
		t.Fatalf("code = %d, want 0 (recovered)", code)
	}
	if !strings.Contains(out.String(), "temporarily unreadable") {
		t.Errorf("expected a transient-retry note:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no longer in flight") {
		t.Errorf("expected to reach terminal after recovery:\n%s", out.String())
	}
}

func TestRunShowWatch_LoadError(t *testing.T) {
	var out, errb bytes.Buffer
	code := runShowWatch("wf_x", &out, &errb, watchDeps{
		load: func(string) (*ccworkflows.RunState, *ccworkflows.RunRef, error) {
			return nil, nil, errFake
		},
		waitNext: func() bool { return true },
	})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "boom") {
		t.Errorf("stderr = %q", errb.String())
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "boom" }

var errFake = fakeErr{}
