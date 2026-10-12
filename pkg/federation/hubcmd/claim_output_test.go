package hubcmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHubClaimOutputModes(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never"} {
		for _, stdout := range []bool{false, true} {
			for _, stderr := range []bool{false, true} {
				var output bytes.Buffer
				writeClaimOutput(&output, "tchac_test-secret", "/private/hub's data/admin-claim.token", mode, stdout, stderr)
				require.Contains(t, output.String(), "sudo cat -- '/private/hub'\\''s data/admin-claim.token'")
				if mode == "always" || mode == "auto" && stdout && stderr {
					require.Contains(t, output.String(), "tchac_test-secret")
				} else {
					require.NotContains(t, output.String(), "tchac_test-secret")
				}
			}
		}
		require.NoError(t, validateClaimOutput(mode))
	}
	require.Error(t, validateClaimOutput("sometimes"))
	for _, args := range [][]string{{"serve"}, {"admin", "reset"}, {"admin", "rotate-claim"}} {
		cmd, _, err := RootCmd().Find(args)
		require.NoError(t, err)
		flag := cmd.Flags().Lookup("print-claim-token")
		require.NotNil(t, flag)
		require.Equal(t, "auto", flag.DefValue)
	}
}

func TestHubDefaultDirectoryCompatibilityAndSandbox(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("TCLAUDE_HUB_DIR", "")
	expected := filepath.Join(home, ".tclaude", "data", "hub", "hub.sqlite")
	require.Equal(t, expected, DefaultDBPath())
	require.NoError(t, os.MkdirAll(filepath.Dir(expected), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(expected), "admin-claim.token"), []byte("fixture"), 0600))
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.Contains(t, protected, filepath.Join(home, ".tclaude", "data"))
	for _, path := range []string{filepath.Dir(expected), filepath.Join(filepath.Dir(expected), "admin-claim.token")} {
		for _, access := range []sandboxpolicy.Access{sandboxpolicy.AccessRead, sandboxpolicy.AccessWrite} {
			_, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{Name: "hub-secret", Filesystem: []sandboxpolicy.FilesystemGrant{{Path: path, Access: access}}})
			require.ErrorContains(t, err, "intersects protected directory")
		}
	}
	legacy := filepath.Join(home, ".tclaude-hub")
	require.NoError(t, os.Mkdir(legacy, 0700))
	require.Equal(t, filepath.Join(legacy, "hub.sqlite"), DefaultDBPath(), "existing state wins even when new directory exists")
	configured := filepath.Join(home, "explicit")
	t.Setenv("TCLAUDE_HUB_DIR", configured)
	require.Equal(t, filepath.Join(configured, "hub.sqlite"), DefaultDBPath())
}

func TestHubClaimResetCommandHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_TEST_CLAIM_RESET") != "1" {
		t.Skip("subprocess helper")
	}
	cmd := RootCmd()
	args := []string{"admin", "reset"}
	if os.Getenv("TCLAUDE_TEST_CLAIM_ACTION") == "rotate-claim" {
		args = []string{"admin", "rotate-claim"}
	}
	if os.Getenv("TCLAUDE_TEST_CLAIM_ACTION") == "serve" {
		args = []string{"serve", "--listen", "127.0.0.1:0"}
	}
	args = append(args, "--db", os.Getenv("TCLAUDE_TEST_CLAIM_DB"), "--print-claim-token", os.Getenv("TCLAUDE_TEST_CLAIM_MODE"))
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute())
}

func TestHubClaimResetRedirectedOutput(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			root := testutil.CanonicalTempDir(t)
			db := filepath.Join(root, "hub.sqlite")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHubClaimResetCommandHelper$")
			cmd.Env = append(os.Environ(), "TCLAUDE_TEST_CLAIM_RESET=1", "TCLAUDE_TEST_CLAIM_DB="+db, "TCLAUDE_TEST_CLAIM_MODE="+mode)
			output, err := cmd.CombinedOutput()
			if mode == "invalid" {
				require.Error(t, err)
				_, err = os.Stat(db)
				require.True(t, os.IsNotExist(err))
				return
			}
			require.NoError(t, err)
			token, err := os.ReadFile(filepath.Join(root, "admin-claim.token"))
			require.NoError(t, err)
			info, err := os.Stat(filepath.Join(root, "admin-claim.token"))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			require.Contains(t, string(output), "sudo cat --")
			if mode == "always" {
				require.Contains(t, string(output), string(bytes.TrimSpace(token)))
			} else {
				require.NotContains(t, string(output), "tchac_")
			}
		})
	}
}

func TestHubClaimServeRedirectedOutput(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never"} {
		t.Run(mode, func(t *testing.T) {
			root := testutil.CanonicalTempDir(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestHubClaimResetCommandHelper$")
			cmd.Env = append(os.Environ(), "TCLAUDE_TEST_CLAIM_RESET=1", "TCLAUDE_TEST_CLAIM_ACTION=serve", "TCLAUDE_TEST_CLAIM_DB="+filepath.Join(root, "hub.sqlite"), "TCLAUDE_TEST_CLAIM_MODE="+mode)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			require.NoError(t, cmd.Start())
			waited := false
			finished := make(chan error, 1)
			go func() { finished <- cmd.Wait() }()
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					<-finished
				}
			})
			claim := filepath.Join(root, "admin-claim.token")
			require.Eventually(t, func() bool { _, err := os.Stat(claim); return err == nil }, 3*time.Second, 10*time.Millisecond)
			require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
			select {
			case err := <-finished:
				waited = true
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("serve did not stop")
			}
			require.Contains(t, output.String(), "sudo cat --")
			if mode == "always" {
				require.Contains(t, output.String(), "tchac_")
			} else {
				require.NotContains(t, output.String(), "tchac_")
			}
		})
	}
}

func TestHubClaimRotateRedirectedOutput(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			root := testutil.CanonicalTempDir(t)
			path := filepath.Join(root, "hub.sqlite")
			st, err := hub.OpenStore(path)
			require.NoError(t, err)
			old, err := st.PrepareAdminClaim(false, time.Now())
			require.NoError(t, err)
			require.NoError(t, st.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestHubClaimResetCommandHelper$")
			cmd.Env = append(os.Environ(), "TCLAUDE_TEST_CLAIM_RESET=1", "TCLAUDE_TEST_CLAIM_ACTION=rotate-claim", "TCLAUDE_TEST_CLAIM_DB="+path, "TCLAUDE_TEST_CLAIM_MODE="+mode)
			output, err := cmd.CombinedOutput()
			token, readErr := os.ReadFile(filepath.Join(root, "admin-claim.token"))
			require.NoError(t, readErr)
			require.NotContains(t, string(output), old)
			if mode == "invalid" {
				require.Error(t, err)
				require.Equal(t, old, string(bytes.TrimSpace(token)))
				return
			}
			require.NoError(t, err)
			require.NotEqual(t, old, string(bytes.TrimSpace(token)))
			require.Contains(t, string(output), "sudo cat --")
			if mode == "always" {
				require.Contains(t, string(output), string(bytes.TrimSpace(token)))
			} else {
				require.NotContains(t, string(output), "tchac_")
			}
			info, err := os.Stat(filepath.Join(root, "admin-claim.token"))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}
