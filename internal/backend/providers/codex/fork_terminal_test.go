package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfinedForkTerminalRecordsNativeResultAndNeverRepeatsHelper(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0700))
	executable := filepath.Join(root, "native")
	script := `#!/bin/sh
printf helper >> "$CODEX_HOME/calls"
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r initialized
IFS= read -r fork
printf '%s\n' "$fork" > "$CODEX_HOME/request"
printf '%s\n' '{"id":2,"result":{"thread":{"id":"new-thread"}}}'
`
	require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
	request := ForkTerminalRequest{Executable: executable, Fork: TurnForkRequest{StateRoot: root, WorkingDirectory: root, ThreadID: "source", LastTurnID: "turn-7"}, Args: []string{"--dangerously-bypass-hook-trust", "-a", "never", "-s", "workspace-write", "resume", "source", "literal source"}, Receipt: filepath.Join(root, "result")}
	replaced := errors.New("test exec boundary")
	err := executeForkTerminal(context.Background(), request, func(exe string, args, env []string) error {
		require.Equal(t, executable, exe)
		require.Equal(t, "new-thread", args[7])
		require.Equal(t, "literal source", args[8], "only the exact resume ID slot changes")
		id, readErr := readForkReceipt(request.Receipt)
		require.NoError(t, readErr)
		require.Equal(t, "new-thread", id, "receipt is published before terminal exec")
		return replaced
	})
	require.ErrorIs(t, err, replaced)
	raw, err := os.ReadFile(filepath.Join(root, "request"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"lastTurnId":"turn-7"`)
	require.ErrorContains(t, executeForkTerminal(context.Background(), request, nil), "claim confined Codex fork")
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	require.NoError(t, err)
	require.Equal(t, "helper", string(calls))
}
