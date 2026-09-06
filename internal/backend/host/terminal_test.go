//go:build linux || darwin

package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrivateTerminalExecutesLiteralInputAndRecovers(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tclaude-host-terminal-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	output := filepath.Join(root, "input.txt")
	prepared, err := (TerminalHost{PrivateRoot: root}).Prepare("exec_test")
	require.NoError(t, err)
	terminal, err := prepared.Release(ProcessSpec{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestTerminalHelper", "--", output},
		Env:        []string{"TCLAUDE_TERMINAL_HELPER=1"},
		Directory:  root,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = terminal.Stop(ctx, true)
	})
	require.Eventually(t, func() bool { return terminal.Observe().Running }, time.Second, 10*time.Millisecond,
		"observation: %#v identity: %#v", terminal.Observe(), terminal.Identity())

	recovered, err := RecoverTerminal(TerminalHost{}, terminal.Identity())
	require.NoError(t, err)
	require.NoError(t, recovered.SendLiteral(context.Background(), "literal $(touch nope); `false`"))
	require.Eventually(t, func() bool {
		value, err := os.ReadFile(output)
		return err == nil && string(value) == "literal $(touch nope); `false`\n"
	}, 2*time.Second, 10*time.Millisecond)
	require.NoFileExists(t, filepath.Join(root, "nope"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	acknowledged, _, err := recovered.Stop(ctx, true)
	require.NoError(t, err)
	require.True(t, acknowledged)
}

func TestPreparedTerminalAbortRemovesPrivateResource(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	prepared, err := (TerminalHost{PrivateRoot: t.TempDir()}).Prepare("exec_abort")
	require.NoError(t, err)
	directory := prepared.directory
	require.NoError(t, prepared.Abort())
	_, err = os.Stat(directory)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTerminalHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_TERMINAL_HELPER") != "1" {
		return
	}
	var output string
	for index, arg := range os.Args {
		if arg == "--" && index+1 < len(os.Args) {
			output = os.Args[index+1]
		}
	}
	if output == "" {
		os.Exit(2)
	}
	buffer := make([]byte, 4096)
	n, err := os.Stdin.Read(buffer)
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(output, buffer[:n], 0o600); err != nil {
		os.Exit(4)
	}
	select {}
}
