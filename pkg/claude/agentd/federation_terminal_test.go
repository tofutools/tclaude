package agentd

import (
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"io"
	"os/exec"
	"testing"
)

// Subprocess boundary fixture for persistent window-option recovery.
type indicatorTmux struct {
	clcommon.Tmux
	options map[string]string
}

func (m *indicatorTmux) Command(a ...string) *exec.Cmd {
	switch a[0] {
	case "send-keys":
		return exec.Command("true")
	case "list-windows":
		return exec.Command("printf", "%s", "@1\n")
	case "resize-window":
		m.options["window-size"] = "manual"
		return exec.Command("true")
	case "show-options":
		name := a[len(a)-1]
		v, ok := m.options[name]
		if !ok {
			return exec.Command("printf", "%s", "")
		}
		if a[2] == "-qv" {
			return exec.Command("printf", "%s", v+"\n")
		}
		return exec.Command("printf", "%s", name+" "+v+"\n")
	case "set-option":
		if a[1] == "-wu" {
			delete(m.options, a[4])
		} else {
			m.options[a[4]] = a[5]
		}
		return exec.Command("true")
	}
	return m.Tmux.Command(a...)
}
func TestFederationTerminalIndicatorCrashRecoveryPreservesOperatorEdits(t *testing.T) {
	old := clcommon.Default
	m := &indicatorTmux{Tmux: old, options: map[string]string{"pane-border-format": "original"}}
	clcommon.Default = m
	t.Cleanup(func() { clcommon.Default = old; fedIndicators = map[string]*fedIndicator{} })
	pin := &fedPanePin{window: "@1"}
	_, err := setFedIndicator(pin, "viewer", "peer", false)
	require.NoError(t, err)
	require.Contains(t, m.options["pane-border-format"], "REMOTE INPUT")
	// Recreate the daemon's in-memory state; persisted restore data survives.
	fedIndicators = map[string]*fedIndicator{}
	cleanupFedTerminalIndicators()
	require.Equal(t, "original", m.options["pane-border-format"])
	require.NotContains(t, m.options, "pane-border-status", "originally inherited option must stay inherited")
	require.NotContains(t, m.options, fedIndicatorOption)
	restore, err := setFedIndicator(pin, "viewer", "peer", true)
	require.NoError(t, err)
	m.options["pane-border-format"] = "operator changed this"
	require.False(t, fedIndicatorPresent("@1"))
	restore()
	require.Equal(t, "operator changed this", m.options["pane-border-format"])
	require.NotContains(t, m.options, fedIndicatorOption)
}

type rendererReadOnly struct{ reads int }

func (r *rendererReadOnly) Read(p []byte) (int, error) { r.reads++; return 0, io.EOF }
func (*rendererReadOnly) Close() error                 { return nil }
func TestFederationTerminalRendererHasNoKeyboardWritePath(t *testing.T) {
	old := clcommon.Default
	m := &indicatorTmux{Tmux: old, options: map[string]string{}}
	clcommon.Default = m
	t.Cleanup(func() { clcommon.Default = old })
	// This renderer implements only Read and Close, so connecting input to its
	// PTY would fail to compile. Special keys and text use tmux subprocesses.
	renderer := &rendererReadOnly{}
	pane := &fedPaneTerminal{f: renderer, pin: &fedPanePin{pane: "%1"}, resize: func(int, int) error { return nil }}
	require.NoError(t, pane.Input([]byte("hello;\r\x03\x1b[A")))
	require.NoError(t, pane.Resize(40, 10))
	require.Zero(t, renderer.reads)
}
