package session

import (
	"os/exec"
	"reflect"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

type detachSizeTmux struct {
	calls [][]string
}

func (t *detachSizeTmux) Command(args ...string) *exec.Cmd {
	t.calls = append(t.calls, append([]string(nil), args...))
	return exec.Command("true")
}

func (t *detachSizeTmux) ListSessions() (map[string]struct{}, error) {
	return nil, nil
}

func withDetachSizeTmux(t *testing.T) *detachSizeTmux {
	t.Helper()
	rec := &detachSizeTmux{}
	previous := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = previous })
	return rec
}

func TestNormalizeTmuxPaneAfterDetachUsesAtomicServerCondition(t *testing.T) {
	rec := withDetachSizeTmux(t)

	NormalizeTmuxPaneAfterDetach("spwn-detached")

	want := [][]string{{
		"if-shell", "-F", "-t", "=spwn-detached:",
		"#{==:#{session_attached},0}",
		"resize-window -t =spwn-detached: -x 200 -y 50 ; set-option -w -t =spwn-detached: window-size latest",
	}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("detach normalization calls = %v, want %v", rec.calls, want)
	}
}

func TestConfigureTmuxDetachNormalizationOptsInAndEnsuresHook(t *testing.T) {
	rec := withDetachSizeTmux(t)

	ConfigureTmuxDetachNormalization("spwn-managed")

	want := [][]string{
		{"set-option", "-t", "=spwn-managed:", tmuxDetachNormalizeOption, "on"},
		{"set-hook", "-g", "client-detached[9001136]",
			"if-shell -F '#{&&:#{==:#{@tclaude_detach_normalize},on},#{==:#{session_attached},0}}' 'run-shell -C \"resize-window -t =#{session_name}: -x 200 -y 50 ; set-option -w -t =#{session_name}: window-size latest\"'"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("detach hook configuration = %v, want %v", rec.calls, want)
	}
}

func TestDetachSizeHelpersEmptySessionNoop(t *testing.T) {
	rec := withDetachSizeTmux(t)

	ConfigureTmuxDetachNormalization("")
	NormalizeTmuxPaneAfterDetach("")

	if len(rec.calls) != 0 {
		t.Fatalf("empty session must be a no-op, got %v", rec.calls)
	}
}
