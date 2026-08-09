package agentd

import (
	"os/exec"
	"reflect"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type paneReconcileTmux struct {
	listing string
	calls   [][]string
}

func (t *paneReconcileTmux) Command(args ...string) *exec.Cmd {
	t.calls = append(t.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "list-sessions" {
		return exec.Command("printf", "%s", t.listing)
	}
	return exec.Command("true")
}

func (t *paneReconcileTmux) ListSessions() (map[string]struct{}, error) {
	return nil, nil
}

func TestNormalizeUnattachedPaneSizesCancelsOnlyNativeScrollbackPanes(t *testing.T) {
	rec := &paneReconcileTmux{listing: "codex-detached\t0\t200\t50\tlatest\n" +
		"opencode-detached\t0\t200\t50\tlatest\n" +
		"claude-detached\t0\t200\t50\tlatest\n" +
		"codex-attached\t1\t200\t50\tlatest\n"}
	previous := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = previous })

	normalizeUnattachedPaneSizes([]*session.SessionState{
		{ID: "c1", TmuxSession: "codex-detached", Harness: harness.CodexName},
		{ID: "o1", TmuxSession: "opencode-detached", Harness: harness.OpenCodeName},
		{ID: "cl1", TmuxSession: "claude-detached", Harness: harness.DefaultName},
		{ID: "c2", TmuxSession: "codex-attached", Harness: harness.CodexName},
	})

	want := [][]string{
		{"list-sessions", "-F", "#{session_name}\t#{session_attached}\t#{window_width}\t#{window_height}\t#{window-size}"},
		{"send-keys", "-X", "-t", "=codex-detached:0.0", "cancel"},
		{"send-keys", "-X", "-t", "=opencode-detached:0.0", "cancel"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("tmux calls = %v, want %v", rec.calls, want)
	}
}
