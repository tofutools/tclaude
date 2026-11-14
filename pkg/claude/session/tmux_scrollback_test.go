package session

import (
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
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

func TestCancelTmuxScrollback_CodexTargetsExactPane(t *testing.T) {
	rec := withRecordingTmux(t)
	codex, ok := harness.Get(harness.CodexName)
	if !ok {
		t.Fatal("codex harness not registered")
	}

	CancelTmuxScrollback("sess-codex", codex)

	want := [][]string{{"send-keys", "-X", "-t", "=sess-codex:0.0", "cancel"}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("codex detach cleanup = %v, want %v", rec.calls, want)
	}
}

func TestCancelTmuxScrollback_NonNativeAndEmptyNoop(t *testing.T) {
	rec := withRecordingTmux(t)
	codex, ok := harness.Get(harness.CodexName)
	if !ok {
		t.Fatal("codex harness not registered")
	}

	CancelTmuxScrollback("sess-claude", harness.Default())
	CancelTmuxScrollback("", codex)
	CancelTmuxScrollback("sess-nil", nil)

	if len(rec.calls) != 0 {
		t.Fatalf("non-native or empty detach cleanup must be a no-op, got %v", rec.calls)
	}
}

// copyModeTmux is a clcommon.Tmux that plays one pane's copy-mode: copy-mode
// enters it, scroll-up and scroll-down move the view (copy-mode -e leaves at
// the bottom), cancel leaves, and display-message reads the state back.
type copyModeTmux struct {
	copy, otherMode bool
	position        int
	calls           [][]string
}

func (c *copyModeTmux) Command(args ...string) *exec.Cmd {
	c.calls = append(c.calls, append([]string(nil), args...))
	switch args[0] {
	case "display-message":
		format := args[len(args)-1]
		out := ""
		switch {
		case strings.Contains(format, "scroll_position"):
			out = fmt.Sprintf("%s %d", onOff(c.copy), c.position)
		default:
			out = fmt.Sprintf("%s %s", onOff(c.copy), onOff(c.copy || c.otherMode))
		}
		return exec.Command("printf", "%s", out)
	case "copy-mode":
		c.copy = true
	case "send-keys":
		n := 1
		for i, a := range args {
			if a == "-N" {
				n, _ = strconv.Atoi(args[i+1])
			}
		}
		switch args[len(args)-1] {
		case "scroll-up":
			c.position += n
		case "scroll-down":
			c.position = max(0, c.position-n)
			if c.position == 0 {
				c.copy = false
			}
		case "cancel":
			c.copy, c.position = false, 0
		}
	}
	return exec.Command("true")
}

func (c *copyModeTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func onOff(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

func (c *copyModeTmux) actions() [][]string {
	var out [][]string
	for _, call := range c.calls {
		if call[0] != "display-message" {
			out = append(out, call)
		}
	}
	return out
}

// TestScrollTmuxPane pins how a browser swipe scrolls a pane whose tmux mouse
// mode is off (Claude Code): up enters copy-mode -e and scrolls five lines a
// notch, down scrolls back and leaves copy-mode at the bottom, live cancels,
// and each reports where the pane is.
func TestScrollTmuxPane(t *testing.T) {
	fake := &copyModeTmux{}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })

	if got := ScrollTmuxPane("sess", ScrollUp, 1); got != (PaneScroll{CopyMode: true, Position: 5}) {
		t.Fatalf("up = %+v", got)
	}
	want := [][]string{
		{"copy-mode", "-e", "-t", "=sess:0.0"},
		{"send-keys", "-t", "=sess:0.0", "-X", "-N", "5", "scroll-up"},
	}
	if !reflect.DeepEqual(fake.actions(), want) {
		t.Fatalf("up ran %v, want %v", fake.actions(), want)
	}
	if got := ScrollTmuxPane("sess", ScrollUp, 2); got.Position != 15 {
		t.Fatalf("a second up = %+v, want position 15", got)
	}
	if n := len(fake.actions()); n != 3 {
		t.Fatalf("a pane already in copy-mode is not entered again: %v", fake.actions())
	}
	if got := ScrollTmuxPane("sess", ScrollDown, 3); got != (PaneScroll{}) {
		t.Fatalf("down to the bottom = %+v, want live", got)
	}
	before := len(fake.actions())
	ScrollTmuxPane("sess", ScrollDown, 1)
	ScrollTmuxPane("sess", ScrollLive, 1)
	if len(fake.actions()) != before {
		t.Fatalf("down or live at live ran %v", fake.actions()[before:])
	}

	ScrollTmuxPane("sess", ScrollUp, 1)
	ScrollTmuxPane("sess", ScrollLive, 1)
	if last := fake.actions()[len(fake.actions())-1]; !reflect.DeepEqual(last, []string{"send-keys", "-t", "=sess:0.0", "-X", "cancel"}) {
		t.Fatalf("live ran %v", last)
	}

	// A pane in another mode (choose-tree) is left alone.
	fake.otherMode = true
	before = len(fake.actions())
	if got := ScrollTmuxPane("sess", ScrollUp, 1); got.CopyMode {
		t.Fatalf("up over another mode = %+v", got)
	}
	if len(fake.actions()) != before {
		t.Fatalf("up over another mode ran %v", fake.actions()[before:])
	}
}

// TestLeaveTmuxCopyMode pins that a browser terminal leaving copy-mode behind
// cancels it, and leaves a live pane alone.
func TestLeaveTmuxCopyMode(t *testing.T) {
	fake := &copyModeTmux{}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
	LeaveTmuxCopyMode("sess")
	if len(fake.actions()) != 0 {
		t.Fatalf("live pane: ran %v", fake.actions())
	}
	fake.copy = true
	LeaveTmuxCopyMode("sess")
	if !reflect.DeepEqual(fake.actions(), [][]string{{"send-keys", "-t", "=sess:0.0", "-X", "cancel"}}) {
		t.Fatalf("ran %v", fake.actions())
	}
}
