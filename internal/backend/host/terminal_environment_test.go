//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrivateTerminalHonorsExactEnvironment(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TCLAUDE_TEST_AMBIENT_SECRET", "synthetic-not-a-real-secret")
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "exact"}[exact], func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tenv-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
			output := filepath.Join(root, "environment.json")
			prepared, err := (TerminalHost{PrivateRoot: root}).Prepare("environment")
			require.NoError(t, err)
			terminal, err := prepared.Release(ProcessSpec{Executable: os.Args[0], Args: []string{"-test.run=^TestTerminalEnvironmentHelper$", "--", output}, Directory: root, Env: []string{"TCLAUDE_TERMINAL_ENV_HELPER=1", "DECLARED_VALUE=literal $(not-executed); `false`"}, ExactEnvironment: exact})
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _, _ = terminal.Stop(ctx, true)
			})
			var values map[string]string
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(output)
				return err == nil && json.Unmarshal(data, &values) == nil
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, "literal $(not-executed); `false`", values["declared"])
			if exact {
				require.Empty(t, values["ambient"])
			} else {
				require.Equal(t, "synthetic-not-a-real-secret", values["ambient"])
			}
			recovered, err := RecoverTerminal(TerminalHost{}, terminal.Identity())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			acknowledged, _, err := recovered.Stop(ctx, true)
			require.NoError(t, err)
			require.True(t, acknowledged)
			require.Eventually(t, func() bool { return recovered.Observe().Exited }, 5*time.Second, 10*time.Millisecond)
		})
	}
}
func TestTerminalEnvironmentHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_TERMINAL_ENV_HELPER") != "1" {
		return
	}
	output := os.Args[len(os.Args)-1]
	data, err := json.Marshal(map[string]string{"ambient": os.Getenv("TCLAUDE_TEST_AMBIENT_SECRET"), "declared": os.Getenv("DECLARED_VALUE")})
	if err != nil {
		os.Exit(2)
	}
	if err = os.WriteFile(output, data, 0600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}
