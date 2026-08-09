package session

import (
	"os/exec"
	"reflect"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

type detachSizeTmux struct {
	status string
	calls  [][]string
}

func (t *detachSizeTmux) Command(args ...string) *exec.Cmd {
	t.calls = append(t.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		return exec.Command("printf", "%s", t.status)
	}
	return exec.Command("true")
}

func (t *detachSizeTmux) ListSessions() (map[string]struct{}, error) {
	return nil, nil
}

func withDetachSizeTmux(t *testing.T, status string) *detachSizeTmux {
	t.Helper()
	rec := &detachSizeTmux{status: status}
	previous := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = previous })
	return rec
}

func TestNormalizeTmuxPaneAfterDetachResizesLastClientPane(t *testing.T) {
	rec := withDetachSizeTmux(t, "0\t155\t39\tlatest")

	NormalizeTmuxPaneAfterDetach("spwn-detached")

	want := [][]string{
		{"display-message", "-p", "-t", "=spwn-detached:", "#{session_attached}\t#{window_width}\t#{window_height}\t#{window-size}"},
		{"resize-window", "-t", "=spwn-detached:", "-x", "200", "-y", "50"},
		{"set-option", "-w", "-t", "=spwn-detached:", "window-size", "latest"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("detach normalization calls = %v, want %v", rec.calls, want)
	}
}

func TestNormalizeTmuxPaneAfterDetachLeavesAttachedPaneAlone(t *testing.T) {
	rec := withDetachSizeTmux(t, "1\t155\t39\tlatest")

	NormalizeTmuxPaneAfterDetach("spwn-viewed")

	want := [][]string{{"display-message", "-p", "-t", "=spwn-viewed:", "#{session_attached}\t#{window_width}\t#{window_height}\t#{window-size}"}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("attached pane calls = %v, want %v", rec.calls, want)
	}
}

func TestNormalizeTmuxPaneAfterDetachRepairsManualSizing(t *testing.T) {
	rec := withDetachSizeTmux(t, "0\t200\t50\tmanual")

	NormalizeTmuxPaneAfterDetach("spwn-manual")

	want := [][]string{
		{"display-message", "-p", "-t", "=spwn-manual:", "#{session_attached}\t#{window_width}\t#{window_height}\t#{window-size}"},
		{"set-option", "-w", "-t", "=spwn-manual:", "window-size", "latest"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("manual-size repair calls = %v, want %v", rec.calls, want)
	}
}

func TestNormalizeTmuxPaneAfterDetachEmptySessionNoop(t *testing.T) {
	rec := withDetachSizeTmux(t, "0\t155\t39\tlatest")

	NormalizeTmuxPaneAfterDetach("")

	if len(rec.calls) != 0 {
		t.Fatalf("empty session must be a no-op, got %v", rec.calls)
	}
}
