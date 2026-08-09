package session

import (
	"os/exec"
	"reflect"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// recordingTmux is a clcommon.Tmux that records every Command invocation's
// args and runs nothing. Its returned *exec.Cmd has an empty Path, so the
// caller's .Run() fails fast without spawning a process — ConfigureTmuxScrollback
// discards that error, so the test stays hermetic (no tmux, no PATH lookup).
type recordingTmux struct {
	calls [][]string
}

func (r *recordingTmux) Command(args ...string) *exec.Cmd {
	r.calls = append(r.calls, append([]string(nil), args...))
	return exec.Command("")
}

func (r *recordingTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

// withRecordingTmux swaps clcommon.Default for a recorder for the duration
// of the test (these tests don't t.Parallel, matching the package's other
// global-swapping tests).
func withRecordingTmux(t *testing.T) *recordingTmux {
	t.Helper()
	rec := &recordingTmux{}
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })
	return rec
}

// TestConfigureTmuxScrollback_Codex pins that a Codex spawn enables tmux
// mouse mode for THAT session only — `set-option -t =<session>: mouse on`,
// never a global (-g) toggle (JOH-213). The target is the
// ExactTarget(name)+":" form: set-option's -t is pane-typed, so a
// colon-less '='-pin would land in the pane slot and never resolve (see
// clcommon.ExactTarget).
func TestConfigureTmuxScrollback_Codex(t *testing.T) {
	rec := withRecordingTmux(t)

	codex, ok := harness.Get(harness.CodexName)
	if !ok {
		t.Fatalf("codex harness not registered")
	}
	ConfigureTmuxScrollback("sess-codex", codex)

	want := [][]string{{"set-option", "-t", "=sess-codex:", "mouse", "on"}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("codex scrollback config = %v, want %v", rec.calls, want)
	}
}

// TestConfigureTmuxScrollback_ClaudeNoop pins that Claude Code — which owns
// its scrollback — never has mouse mode toggled, so it keeps fighting tmux
// over the wheel for no one. The helper must emit zero tmux commands.
func TestConfigureTmuxScrollback_ClaudeNoop(t *testing.T) {
	rec := withRecordingTmux(t)

	ConfigureTmuxScrollback("sess-claude", harness.Default())

	if len(rec.calls) != 0 {
		t.Fatalf("claude must not toggle tmux mouse mode, got %v", rec.calls)
	}
}

// TestConfigureTmuxScrollback_NilHarness guards the defensive nil path:
// a nil descriptor folds to a no-op rather than panicking.
func TestConfigureTmuxScrollback_NilHarness(t *testing.T) {
	rec := withRecordingTmux(t)

	ConfigureTmuxScrollback("sess-nil", nil)

	if len(rec.calls) != 0 {
		t.Fatalf("nil harness must be a no-op, got %v", rec.calls)
	}
}
