//go:build linux

package session

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	tclaudeLayerSmokeHelperEnv = "TCLAUDE_SANDBOX_V2_SMOKE_HELPER"
	smokeAllowedEnv            = "TCLAUDE_SANDBOX_V2_ALLOWED"
	smokeOutsideEnv            = "TCLAUDE_SANDBOX_V2_OUTSIDE"
	smokeSocketEnv             = "TCLAUDE_SANDBOX_V2_SOCKET"
	smokeAliasFileEnv          = "TCLAUDE_SANDBOX_V2_ALIAS_FILE"
	smokeProtectedFileEnv      = "TCLAUDE_SANDBOX_V2_PROTECTED_FILE"
)

func TestTclaudeLayerHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on an unsandboxed Linux host with bubblewrap")
	}
	binary, _, err := ResolveTclaudeLayer()
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-sandbox-v2-smoke-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	realTools := filepath.Join(root, "real-tools")
	aliasTools := filepath.Join(root, "alias-tools")
	smokeHome := filepath.Join(root, "home")
	socket := filepath.Join(smokeHome, ".tclaude", "api", "agentd.sock")
	protectedDir := filepath.Join(smokeHome, ".tclaude", "data")
	protectedFile := filepath.Join(protectedDir, "private")
	for _, dir := range []string{
		allowed, outside, realTools, filepath.Dir(socket), protectedDir,
		filepath.Join(smokeHome, ".claude", "sessions"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	t.Setenv("HOME", smokeHome)
	require.NoError(t, os.WriteFile(protectedFile, []byte("must-stay-hidden"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realTools, "probe"), []byte("alias-ok"), 0o600))
	require.NoError(t, os.Symlink(realTools, aliasTools))
	// `go test` normally places its executable under /tmp, which this layer
	// intentionally replaces with a fresh tmpfs. Copy the helper alongside the
	// fixture so the sandbox can execute it through the read-only base root.
	helperBinary := filepath.Join(root, "smoke-helper")
	copyTestBinary(t, os.Args[0], helperBinary)

	// Spell the profile rule through a symlink. Resolution must bind the real
	// target, while the base read-only root keeps the alias itself usable.
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Explicit: &sandboxpolicy.Profile{
			Name: "tclaude-layer-smoke",
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: allowed, Access: sandboxpolicy.AccessWrite},
				{Path: aliasTools, Access: sandboxpolicy.AccessRead},
			},
		},
	})
	require.NoError(t, err)
	plan, err := renderMountPlanInterim(effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{Path: realTools, Mode: sandboxpolicy.MountRO})
	assert.NotContains(t, plan.Entries, sandboxpolicy.MountEntry{Path: aliasTools, Mode: sandboxpolicy.MountRO})

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			accepted <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var byte [1]byte
		_, acceptErr = conn.Read(byte[:])
		accepted <- acceptErr
	}()

	args, err := bwrapArgs(plan)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary, "-test.run=^TestTclaudeLayerSmokeHelper$")...)
	cmd.Env = append(os.Environ(),
		tclaudeLayerSmokeHelperEnv+"=1",
		smokeAllowedEnv+"="+allowed,
		smokeOutsideEnv+"="+outside,
		smokeSocketEnv+"="+socket,
		smokeAliasFileEnv+"="+filepath.Join(aliasTools, "probe"),
		smokeProtectedFileEnv+"="+protectedFile,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer host smoke timed out")
	}
	require.NoErrorf(t, err, "tclaude-layer host smoke output: %s", output)
	select {
	case err := <-accepted:
		require.NoError(t, err, "sandboxed helper could not exchange data over the agentd-style Unix socket")
	case <-time.After(5 * time.Second):
		t.Fatal("sandboxed helper did not connect to the agentd-style Unix socket")
	}
}

func TestTclaudeLayerSmokeHelper(t *testing.T) {
	if os.Getenv(tclaudeLayerSmokeHelperEnv) != "1" {
		t.Skip("host-smoke helper subprocess")
	}
	allowed := os.Getenv(smokeAllowedEnv)
	outside := os.Getenv(smokeOutsideEnv)
	socket := os.Getenv(smokeSocketEnv)
	aliasFile := os.Getenv(smokeAliasFileEnv)
	protectedFile := os.Getenv(smokeProtectedFileEnv)

	require.NoError(t, os.WriteFile(filepath.Join(allowed, "written"), []byte("ok"), 0o600))
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("write outside the allowed root unexpectedly succeeded")
	}
	got, err := os.ReadFile(aliasFile)
	require.NoError(t, err, "symlink alias must remain usable through the read-only base root")
	assert.Equal(t, "alias-ok", string(got))
	if _, err := os.ReadFile(protectedFile); err == nil {
		t.Fatal("protected tclaude state unexpectedly remained readable")
	}

	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	require.NoErrorf(t, err, "connect to agentd-style socket %s", socket)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte{1})
	require.NoError(t, err)
}

func copyTestBinary(t *testing.T, source, destination string) {
	t.Helper()
	src, err := os.Open(source)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	require.NoError(t, err)
	require.NoError(t, dst.Close())
}
