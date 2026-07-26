//go:build linux

package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	tclaudeLayerSmokeHelperEnv = "TCLAUDE_SANDBOX_V2_SMOKE_HELPER"
	smokeAllowedEnv            = "TCLAUDE_SANDBOX_V2_ALLOWED"
	smokeOutsideEnv            = "TCLAUDE_SANDBOX_V2_OUTSIDE"
	smokeAliasFileEnv          = "TCLAUDE_SANDBOX_V2_ALIAS_FILE"
	smokeProtectedFileEnv      = "TCLAUDE_SANDBOX_V2_PROTECTED_FILE"
	smokeProtectedReadableEnv  = "TCLAUDE_SANDBOX_V2_PROTECTED_READABLE"
	smokeTmuxSocketEnv         = "TCLAUDE_SANDBOX_V2_TMUX_SOCKET"
	smokeTclaudeBinaryEnv      = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
)

const smokeConvID = "75000000-0000-4000-8000-000000000750"

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
	protectedDir := filepath.Join(smokeHome, ".tclaude", "data")
	protectedFile := filepath.Join(protectedDir, "private")
	tmuxBase := filepath.Join(root, "tmux-base")
	for _, dir := range []string{
		allowed, outside, realTools, filepath.Join(smokeHome, ".tclaude", "api"), protectedDir, tmuxBase,
		filepath.Join(smokeHome, ".claude", "sessions"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	t.Setenv("HOME", smokeHome)
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	tclaudeBinary := strings.TrimSpace(os.Getenv(smokeTclaudeBinaryEnv))
	require.NotEmpty(t, tclaudeBinary,
		"set TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY to the current built tclaude binary")
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	agentSocket := filepath.Join(smokeHome, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startTclaudeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    "sandbox-v2-smoke",
		PID:                   os.Getpid(),
		ConvID:                smokeConvID,
		Harness:               harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Cwd:                   allowed,
		Status:                StatusWorking,
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))
	tmuxSocket := filepath.Join(tmuxSocketDir, "tclaude")
	tmuxListener, err := net.Listen("unix", tmuxSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tmuxListener.Close() })
	require.NoError(t, os.WriteFile(protectedFile, []byte("must-stay-hidden"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realTools, "probe"), []byte("alias-ok"), 0o600))
	require.NoError(t, os.Symlink(realTools, aliasTools))
	// `go test` normally places its executable under /tmp, which this layer
	// intentionally replaces with a fresh tmpfs. Copy the helper alongside the
	// fixture so the sandbox can execute it through the read-only base root.
	// Name the helper `claude`: the real agentd identity resolver recognizes
	// that harness ancestor and resolves its PID through the sessions table.
	helperBinary := filepath.Join(root, "claude")
	copyTestBinary(t, os.Args[0], helperBinary)

	// Spell the profile rule through a symlink. Resolution must bind the real
	// target, while the base read-only root keeps the alias itself usable.
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Explicit: &sandboxpolicy.Profile{
			Name: "tclaude-layer-smoke",
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: allowed, Access: sandboxpolicy.AccessWrite},
				{Path: aliasTools, Access: sandboxpolicy.AccessRead},
				// The applier's final host-control phase must override even an
				// ordinary write grant on the tmux socket directory's parent.
				{Path: tmuxBase, Access: sandboxpolicy.AccessWrite},
			},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{Path: realTools, Mode: sandboxpolicy.MountRO})
	assert.NotContains(t, plan.Entries, sandboxpolicy.MountEntry{Path: aliasTools, Mode: sandboxpolicy.MountRO})

	phase0, err := tclaudeLayerPhase0WriteDirs(TclaudeLayerLaunchContract{
		HarnessName: harness.DefaultName,
		WriteDirs:   []string{allowed},
	}, effective)
	require.NoError(t, err)
	runTclaudeLayerSmokeHelper(t, binary, helperBinary, phase0, plan, false, allowed, outside,
		filepath.Join(aliasTools, "probe"), protectedFile, tmuxSocket, tclaudeBinary)

	breakGlassEffective := effective
	breakGlassEffective.BreakGlassFilesystem = []sandboxpolicy.BreakGlassGrant{{
		Path: protectedDir, Access: sandboxpolicy.AccessRead,
	}}
	breakGlassPlan, err := sandboxpolicy.RenderMountPlan(breakGlassEffective)
	require.NoError(t, err)
	runTclaudeLayerSmokeHelper(t, binary, helperBinary, phase0, breakGlassPlan, true, allowed, outside,
		filepath.Join(aliasTools, "probe"), protectedFile, tmuxSocket, tclaudeBinary)
}

func runTclaudeLayerSmokeHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	protectedReadable bool,
	allowed, outside, aliasFile, protectedFile, tmuxSocket, tclaudeBinary string,
) {
	t.Helper()
	args, err := bwrapArgs(phase0WriteDirs, plan)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary, "-test.run=^TestTclaudeLayerSmokeHelper$")...)
	cmd.Env = append(os.Environ(),
		tclaudeLayerSmokeHelperEnv+"=1",
		smokeAllowedEnv+"="+allowed,
		smokeOutsideEnv+"="+outside,
		smokeAliasFileEnv+"="+aliasFile,
		smokeProtectedFileEnv+"="+protectedFile,
		smokeProtectedReadableEnv+"="+boolEnv(protectedReadable),
		smokeTmuxSocketEnv+"="+tmuxSocket,
		smokeTclaudeBinaryEnv+"="+tclaudeBinary,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer host smoke timed out")
	}
	require.NoErrorf(t, err, "tclaude-layer host smoke output: %s", output)
}

func TestTclaudeLayerSmokeHelper(t *testing.T) {
	if os.Getenv(tclaudeLayerSmokeHelperEnv) != "1" {
		t.Skip("host-smoke helper subprocess")
	}
	allowed := os.Getenv(smokeAllowedEnv)
	outside := os.Getenv(smokeOutsideEnv)
	aliasFile := os.Getenv(smokeAliasFileEnv)
	protectedFile := os.Getenv(smokeProtectedFileEnv)
	protectedReadable := os.Getenv(smokeProtectedReadableEnv) == "1"
	tmuxSocket := os.Getenv(smokeTmuxSocketEnv)
	tclaudeBinary := os.Getenv(smokeTclaudeBinaryEnv)

	require.NoError(t, os.WriteFile(filepath.Join(allowed, "written"), []byte("ok"), 0o600))
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("write outside the allowed root unexpectedly succeeded")
	}
	got, err := os.ReadFile(aliasFile)
	require.NoError(t, err, "symlink alias must remain usable through the read-only base root")
	assert.Equal(t, "alias-ok", string(got))
	protected, err := os.ReadFile(protectedFile)
	if protectedReadable {
		require.NoError(t, err, "acknowledged break-glass path must reopen after the protected baseline")
		assert.Equal(t, "must-stay-hidden", string(protected))
	} else if err == nil {
		t.Fatal("protected tclaude state unexpectedly remained readable under an ordinary profile")
	}
	if conn, err := net.DialTimeout("unix", tmuxSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host tmux socket remained reachable despite the final applier hide")
	}

	whoami, err := exec.Command(tclaudeBinary, "agent", "whoami").CombinedOutput()
	require.NoErrorf(t, err, "authenticated tclaude agent whoami inside namespace: %s", whoami)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(string(whoami)), "agt_"),
		"agentd must resolve a stable managed identity through bwrap ancestry; got %q", whoami)
}

func startTclaudeLayerSmokeAgentd(t *testing.T, tclaudeBinary, socket string) func() {
	t.Helper()
	db.ResetForTest()
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, tclaudeBinary, "agentd", "serve",
		"--socket", socket, "--no-tray", "--no-print-human-token")
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	deadline := time.Now().Add(15 * time.Second)
	for !agentipc.SocketReachable(socket) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("smoke agentd exited during startup: %s", output.String())
		}
		if time.Now().After(deadline) {
			cancel()
			_ = cmd.Wait()
			t.Fatalf("smoke agentd did not become reachable: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		cancel()
		_ = cmd.Wait()
		db.ResetForTest()
	}
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
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
