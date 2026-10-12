package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The guardian re-execs the same executable before Cobra or test flag parsing.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--internal-hub-exec-guardian" {
		cli.Main("", func() *cobra.Command { panic("guardian constructed the root") })
	}
	if os.Getenv("TCLAUDE_TEST_SHARED_CLI_MAIN") == "1" {
		cli.Main("", func() *cobra.Command {
			cmd := &cobra.Command{Use: "fixture", Run: func(*cobra.Command, []string) { fmt.Fprintln(os.Stdout, "ordinary root executed") }}
			cmd.SetArgs([]string{})
			return cmd
		})
	}
	os.Exit(m.Run())
}
func TestSharedMainHubGuardianDispatch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("hub scripts refuse root")
	}
	home := testutil.CanonicalTempDir(t)
	path := filepath.Join(home, "script.sh")
	require.NoError(t, os.WriteFile(path, []byte("echo guardian-executed\n"), 0600))
	life, keep, err := os.Pipe()
	require.NoError(t, err)
	defer life.Close()
	defer keep.Close()
	result, send, err := os.Pipe()
	require.NoError(t, err)
	defer result.Close()
	defer send.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "--internal-hub-exec-guardian", "10", path)
	cmd.ExtraFiles = []*os.File{life, send}
	cmd.Env = append(os.Environ(), "HOME="+home)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	require.NoError(t, life.Close())
	require.NoError(t, send.Close())
	require.NoError(t, cmd.Wait(), stderr.String())
	var report struct {
		ExitCode int `json:"exit_code"`
	}
	require.NoError(t, json.NewDecoder(result).Decode(&report))
	require.Zero(t, report.ExitCode)
	require.Equal(t, "guardian-executed\n", out.String())
	require.Empty(t, stderr.String())
	require.NoDirExists(t, filepath.Join(home, ".tclaude"), "private helper must run before config/database work")
}
func TestSharedMainOrdinaryInvocationDoesNotEnterGuardian(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_SHARED_CLI_MAIN=1", "HOME="+home)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "ordinary root executed\n", string(output))
}
