package tmuxcmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// privateRealTmux is a real tmux server on a socket of its own, so a test can
// check what tmux itself does with a command without going near tclaude's
// server.
type privateRealTmux struct{ socket string }

func (t privateRealTmux) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-u", "-S", t.socket, "-f", "/dev/null"}, args...)...)
}

func (t privateRealTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func withPrivateRealTmux(t *testing.T) privateRealTmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A socket path is limited to about 100 bytes, less than a test's own
	// temp directory may take.
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("tcv-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	if len(socket) > 100 {
		t.Skipf("temp directory too long for a tmux socket path: %s", socket)
	}
	tmux := privateRealTmux{socket: socket}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() {
		_ = tmux.Command("kill-server").Run()
		_ = os.Remove(socket)
		clcommon.Default = previous
	})
	return tmux
}

// Text handed to SendLiteral and PasteText reaches the pane as written, even
// where tmux would read it as its own syntax: a leading '-' is a flag to
// tmux's argument parser and a trailing ';' ends a command.
func TestRealTmuxDeliversLiteralTextVerbatim(t *testing.T) {
	tmux := withPrivateRealTmux(t)
	c := Local()
	out := filepath.Join(t.TempDir(), "typed")
	// The pane copies what is typed into it to a file, one line per Enter.
	require.NoError(t, tmux.Command("new-session", "-d", "-s", "verbatim", "-x", "80", "-y", "10",
		"sh", "-c", "stty -echo; cat > "+out).Run())
	pane := Session("verbatim").FirstPane()

	texts := []string{"-dash first", "ends with;", ";", "- both;", `back\slash\;`, "Enter"}
	for _, text := range texts {
		require.NoError(t, c.SendLiteral(pane, text), text)
		require.NoError(t, c.SendKeys(pane, "Enter"))
	}
	pasted := []string{"- pasted item;", "--", "plain"}
	for i, text := range pasted {
		require.NoError(t, c.PasteText(pane, fmt.Sprintf("buf%d", i), text, nil), text)
		require.NoError(t, c.SendKeys(pane, "Enter"))
	}

	want := strings.Join(append(texts, pasted...), "\n") + "\n"
	deadline := time.Now().Add(20 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(out)
		if got = string(raw); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane received %q, want %q", got, want)
}
