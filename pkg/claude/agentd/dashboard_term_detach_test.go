package agentd

import (
	"os/exec"
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// detachRecTmux records the tmux argv it is asked to run and returns a harmless
// no-op command, so a unit test can assert which tmux subcommand a code path
// issues without standing up a real tmux server.
type detachRecTmux struct{ calls [][]string }

func (d *detachRecTmux) Command(args ...string) *exec.Cmd {
	d.calls = append(d.calls, append([]string(nil), args...))
	return exec.Command("true")
}

func (d *detachRecTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// TestDetachTmuxSession_IssuesDetachClient pins the reliable detach path: on
// teardown the modal asks the tmux server to detach the session's clients by
// name (`detach-client -s =<session>`, '='-pinned so a dead name can't
// prefix-resolve onto a live namesake and detach the wrong session's
// clients — see clcommon.ExactTarget). This is what actually drops the web
// window's tmux client when the modal closes.
func TestDetachTmuxSession_IssuesDetachClient(t *testing.T) {
	rec := &detachRecTmux{}
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })

	codex, ok := harness.Get(harness.CodexName)
	if !ok {
		t.Fatal("codex harness not registered")
	}
	detachTmuxSession("spwn-32d8e2", codex)

	if len(rec.calls) != 2 {
		t.Fatalf("want detach and copy-mode cancel calls, got %d: %v", len(rec.calls), rec.calls)
	}
	wantDetach := []string{"detach-client", "-s", "=spwn-32d8e2"}
	wantCancel := []string{"send-keys", "-X", "-t", "=spwn-32d8e2:0.0", "cancel"}
	if !slices.Equal(rec.calls[0], wantDetach) || !slices.Equal(rec.calls[1], wantCancel) {
		t.Errorf("detach cleanup calls = %v, want %v then %v", rec.calls, wantDetach, wantCancel)
	}
}

// TestDetachTmuxSession_EmptyIsNoop guards that a blank session name issues no
// tmux command at all — a `detach-client -s ""` could otherwise match the
// current/most-recent client and detach the wrong thing.
func TestDetachTmuxSession_EmptyIsNoop(t *testing.T) {
	rec := &detachRecTmux{}
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })

	detachTmuxSession("", harness.Default())

	if len(rec.calls) != 0 {
		t.Errorf("empty session must issue no tmux command, got %v", rec.calls)
	}
}

func TestDetachTmuxSession_ClaudeDoesNotCancelMode(t *testing.T) {
	rec := &detachRecTmux{}
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })

	detachTmuxSession("spwn-claude", harness.Default())

	want := []string{"detach-client", "-s", "=spwn-claude"}
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
		t.Fatalf("claude detach must not inject copy-mode cancel: got %v, want %v", rec.calls, want)
	}
}
