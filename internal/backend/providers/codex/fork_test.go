package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeTurnForkerUsesExactLastTurn(t *testing.T) {
	root := t.TempDir()
	argv := filepath.Join(root, "argv")
	executable := filepath.Join(root, "codex-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argv + "\"\nIFS= read -r init\nprintf '%s\\n' '{\"id\":1,\"result\":{\"userAgent\":\"codex/1\"}}'\nIFS= read -r initialized\nIFS= read -r fork\nprintf '%s\\n' \"$fork\" >> \"" + argv + "\"\nprintf '%s\\n' '{\"id\":2,\"result\":{\"thread\":{\"id\":\"forked-thread\"}}}'\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	id, err := (nativeTurnForker{executable: executable}).Fork(context.Background(), TurnForkRequest{StateRoot: root, WorkingDirectory: root, ThreadID: "source-thread", LastTurnID: "turn-7"})
	require.NoError(t, err)
	require.Equal(t, "forked-thread", id)
	raw, err := os.ReadFile(argv)
	require.NoError(t, err)
	require.Contains(t, string(raw), "app-server")
	require.Contains(t, string(raw), `"lastTurnId":"turn-7"`)
}
