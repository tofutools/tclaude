//go:build linux

package session

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"golang.org/x/sys/unix"
)

const (
	tclaudeLayerSmokeHelperEnv = "TCLAUDE_SANDBOX_V2_SMOKE_HELPER"
	smokeAllowedEnv            = "TCLAUDE_SANDBOX_V2_ALLOWED"
	smokeOutsideEnv            = "TCLAUDE_SANDBOX_V2_OUTSIDE"
	smokeAliasFileEnv          = "TCLAUDE_SANDBOX_V2_ALIAS_FILE"
	smokeProtectedFileEnv      = "TCLAUDE_SANDBOX_V2_PROTECTED_FILE"
	smokeTmuxSocketEnv         = "TCLAUDE_SANDBOX_V2_TMUX_SOCKET"
	smokeRuntimeSocketEnv      = "TCLAUDE_SANDBOX_V2_RUNTIME_SOCKET"
	smokeHostPIDEnv            = "TCLAUDE_SANDBOX_V2_HOST_PID"
	smokeLoopbackAddrEnv       = "TCLAUDE_SANDBOX_V2_LOOPBACK_ADDR"
	smokeTclaudeBinaryEnv      = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	smokeResizeReporterEnv     = "TCLAUDE_SANDBOX_V2_RESIZE_REPORTER"
	smokePrivateOwnFileEnv     = "TCLAUDE_SANDBOX_V2_PRIVATE_OWN_FILE"
	smokePrivateSiblingDirEnv  = "TCLAUDE_SANDBOX_V2_PRIVATE_SIBLING_DIR"
	smokeAllowedSocketEnv      = "TCLAUDE_SANDBOX_V2_ALLOWED_SOCKET"
	smokePeerSocketEnv         = "TCLAUDE_SANDBOX_V2_PEER_SOCKET"
	smokePositiveRootSocketEnv = "TCLAUDE_SANDBOX_V2_POSITIVE_ROOT_SOCKET"
	smokeSocketHelperEnv       = "TCLAUDE_SANDBOX_V2_SOCKET_HELPER"
	smokeHostNetworkHelperEnv  = "TCLAUDE_SANDBOX_V2_HOSTNET_HELPER"
	// The resolved host resolver target, empty when /etc/resolv.conf is a plain
	// file. Non-empty means the constructed root had to reopen it, and the
	// helper turns that into a hard assertion instead of a guess.
	smokeResolverTargetEnv = "TCLAUDE_SANDBOX_V2_RESOLVER_TARGET"
	// TCL-866 mount-path evidence. Each pair names the host directory a grant
	// reads from and the sandbox path it must appear at.
	smokeMountROSourceEnv = "TCLAUDE_SANDBOX_V2_MOUNT_RO_SOURCE"
	smokeMountROGuestEnv  = "TCLAUDE_SANDBOX_V2_MOUNT_RO_GUEST"
	smokeMountRWSourceEnv = "TCLAUDE_SANDBOX_V2_MOUNT_RW_SOURCE"
	smokeMountRWGuestEnv  = "TCLAUDE_SANDBOX_V2_MOUNT_RW_GUEST"
)

// tclaudeLayerSmokeMounts carries the TCL-866 projection fixture through the
// helper boundary. Keeping it in one value avoids growing the helper's already
// long positional parameter list by four more strings.
type tclaudeLayerSmokeMounts struct {
	ReadOnlySource  string
	ReadOnlyGuest   string
	ReadWriteSource string
	ReadWriteGuest  string
}

const smokeConvID = "75000000-0000-4000-8000-000000000750"

const (
	tclaudeLayerSmokeSessionID        = "sandbox-v2-smoke"
	tclaudeLayerSmokeLaunchGeneration = "linux-bwrap-smoke-generation"
)

func TestTclaudeLayerHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on an unsandboxed Linux host with bubblewrap")
	}
	binary, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed)
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
	// Production attachment roots live beneath the class-3 daemon-data hide.
	// The smoke therefore proves the daemon-only child carveout works through
	// that protected ancestor, not merely under an otherwise visible parent.
	privateParent := filepath.Join(protectedDir, "spawn-attachments")
	privateOwn := filepath.Join(privateParent, "own-session")
	privateSibling := filepath.Join(privateParent, "sibling-session")
	for _, dir := range []string{
		allowed, outside, realTools, filepath.Join(smokeHome, ".tclaude", "api"), protectedDir, tmuxBase,
		filepath.Join(smokeHome, ".claude", "sessions"), privateOwn, privateSibling,
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	privateOwnFile := filepath.Join(privateOwn, "pasted-image.png")
	require.NoError(t, os.WriteFile(privateOwnFile, []byte("own-session-image"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(privateSibling, "sibling-secret.png"),
		[]byte("sibling-session-image"),
		0o600,
	))
	t.Setenv("HOME", smokeHome)
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	fakeTmuxBin := filepath.Join(root, "host-bin")
	require.NoError(t, os.MkdirAll(fakeTmuxBin, 0o755))
	fakeTmux := filepath.Join(fakeTmuxBin, "tmux")
	fakeTmuxScript := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" display-message "*)
    printf '%%s|%%s|%%s|0|||%%s\n' %s '%%1' %s %s
    ;;
  *" list-sessions "*)
    printf '%%s\n' %s
    ;;
  *) exit 1 ;;
esac
`, clcommon.ShellQuoteArg(tclaudeLayerSmokeSessionID),
		clcommon.ShellQuoteArg(strconv.Itoa(os.Getpid())),
		clcommon.ShellQuoteArg(tclaudeLayerSmokeLaunchGeneration),
		clcommon.ShellQuoteArg(tclaudeLayerSmokeSessionID))
	require.NoError(t, os.WriteFile(fakeTmux, []byte(fakeTmuxScript), 0o755))
	t.Setenv("PATH", fakeTmuxBin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		ID:                    tclaudeLayerSmokeSessionID,
		PID:                   os.Getpid(),
		ConvID:                smokeConvID,
		TmuxSession:           tclaudeLayerSmokeSessionID,
		Harness:               harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration:  tclaudeLayerSmokeLaunchGeneration,
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
	runtimeSocket := filepath.Join(root, "runtime", "ambient.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(runtimeSocket), 0o700))
	runtimeListener, err := net.Listen("unix", runtimeSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtimeListener.Close() })
	hostLoopback, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hostLoopback.Close() })
	require.NoError(t, os.WriteFile(protectedFile, []byte("must-stay-hidden"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realTools, "probe"), []byte("alias-ok"), 0o600))
	require.NoError(t, os.Symlink(realTools, aliasTools))
	// `go test` normally places its executable under /tmp, which this layer
	// intentionally replaces with a fresh tmpfs. Copy the helper alongside the
	// fixture so the sandbox can execute it through the read-only base root.
	// Keep the harness-shaped name so this smoke remains representative even
	// though tclaude-layer identity is proved from the launch pane and generation.
	helperBinary := filepath.Join(allowed, "claude")
	copyTestBinary(t, os.Args[0], helperBinary)

	// TCL-866 fixture. The projection sources live OUTSIDE the smoke root on
	// purpose: no other rule covers them, so the only way their content can be
	// visible inside the sandbox is through the mount_path grants below. That is
	// what makes "appears at the sandbox path and NOT at the host path" a real
	// assertion rather than a restatement of some other grant.
	mountBase, err := os.MkdirTemp(smokeBase, "tclaude-sandbox-v2-mounts-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(mountBase) })
	mountBase, err = filepath.EvalSymlinks(mountBase)
	require.NoError(t, err)
	mounts := tclaudeLayerSmokeMounts{
		ReadOnlySource:  filepath.Join(mountBase, "dataset"),
		ReadOnlyGuest:   "/srv/dataset",
		ReadWriteSource: filepath.Join(mountBase, "scratch"),
		ReadWriteGuest:  "/srv/scratch",
	}
	require.NoError(t, os.MkdirAll(mounts.ReadOnlySource, 0o700))
	require.NoError(t, os.MkdirAll(mounts.ReadWriteSource, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(mounts.ReadOnlySource, "probe"), []byte("mounted-ro"), 0o600))

	// Spell the profile rule through a symlink and persist it through the real
	// registry path. Resolution must bind the canonical target and recover the
	// retained spelling; a raw in-memory profile would not exercise TCL-762.
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{
		Name:          "tclaude-layer-smoke",
		NetworkAccess: sandboxpolicy.NetworkAccessNone,
		Filesystem: []sandboxpolicy.FilesystemGrant{
			// Exercise a legitimate most-specific-wins reopen beneath an
			// ordinary hide. The applier must create this child bind before
			// remounting the hidden parent read-only.
			{Path: root, Access: sandboxpolicy.AccessDeny},
			{Path: allowed, Access: sandboxpolicy.AccessWrite},
			{Path: aliasTools, Access: sandboxpolicy.AccessRead},
			// The applier's final host-control phase must override even an
			// ordinary write grant on the tmux socket directory's parent.
			{Path: tmuxBase, Access: sandboxpolicy.AccessWrite},
			// TCL-866: project two host directories onto sandbox paths that do
			// not exist on the host at all.
			{
				Path: mounts.ReadOnlySource, Access: sandboxpolicy.AccessRead,
				MountPath: mounts.ReadOnlyGuest,
			},
			{
				Path: mounts.ReadWriteSource, Access: sandboxpolicy.AccessWrite,
				MountPath: mounts.ReadWriteGuest,
			},
		},
	})
	require.NoError(t, err)
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, "tclaude-layer-smoke")
	require.NoError(t, err)
	effective := snapshot.Effective
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{Path: realTools, Mode: sandboxpolicy.MountRO})
	assert.NotContains(t, plan.Entries, sandboxpolicy.MountEntry{Path: aliasTools, Mode: sandboxpolicy.MountRO})
	assert.Contains(t, plan.Aliases, sandboxpolicy.MountAlias{Link: aliasTools, Target: realTools})
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{
		Path: mounts.ReadOnlyGuest, Mode: sandboxpolicy.MountRO,
		Source: mounts.ReadOnlySource,
	})
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{
		Path: mounts.ReadWriteGuest, Mode: sandboxpolicy.MountRW,
		Source: mounts.ReadWriteSource,
	})

	phase0, err := tclaudeLayerPhase0WriteDirs(TclaudeLayerLaunchContract{
		HarnessName: harness.DefaultName,
		WriteDirs:   []string{allowed},
	}, effective)
	require.NoError(t, err)
	hostOpenPlan := plan
	hostOpenPlan.NetworkPosture = sandboxpolicy.NetworkHostOpen
	// The host-open posture binds the real host root read-only, so there is no
	// writable place to create a new mount point and the applier refuses a
	// projection outright. The resize arms below exist to exercise terminal
	// plumbing, not mount paths, so drop the projected entries rather than
	// inventing host directories for them.
	hostOpenEntries := make([]sandboxpolicy.MountEntry, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		if entry.IsRemapped() {
			continue
		}
		hostOpenEntries = append(hostOpenEntries, entry)
	}
	hostOpenPlan.Entries = hostOpenEntries
	for _, tc := range []struct {
		name string
		plan sandboxpolicy.MountPlan
	}{
		{name: "host-open", plan: resizePlanClone(hostOpenPlan)},
		{name: "isolated", plan: resizePlanClone(plan)},
	} {
		t.Run("terminal-resize-"+tc.name, func(t *testing.T) {
			assertTclaudeLayerResizeRoundTrip(
				t, binary, tclaudeBinary, helperBinary, phase0, tc.plan,
			)
		})
	}
	// There is only one arm now. This smoke used to run a second time with an
	// acknowledged break-glass grant and assert the protected file became
	// readable; TCL-791 removed the feature, so protected state is unreadable
	// on real bubblewrap unconditionally, and the helper asserts exactly that.
	privateWriteDir := TclaudeLayerPrivateWriteDir{Parent: privateParent, Current: privateOwn}
	runTclaudeLayerSmokeHelper(t, binary, helperBinary, phase0, plan, privateWriteDir, allowed, outside,
		filepath.Join(aliasTools, "probe"), protectedFile, tmuxSocket, runtimeSocket,
		strconv.Itoa(os.Getpid()), hostLoopback.Addr().String(), tclaudeBinary,
		privateOwnFile, privateSibling, mounts)

	// Two launch-equivalent agents receive disjoint socket lists. Each can
	// connect to its own bound endpoint while the sibling endpoint outside all
	// positive roots is absent. An unlisted socket beneath the launch cwd's
	// writable bind remains reachable, proving the categorical remainder
	// disclosed by Linux's Partial capability.
	policySocketDir := filepath.Join(root, "policy-sockets")
	require.NoError(t, os.MkdirAll(policySocketDir, 0o700))
	policySockets := []string{
		filepath.Join(policySocketDir, "agent-a.sock"),
		filepath.Join(policySocketDir, "agent-b.sock"),
	}
	for _, socket := range policySockets {
		listener, listenErr := net.Listen("unix", socket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}
	positiveRootSocket := filepath.Join(allowed, "unlisted-cwd.sock")
	positiveRootListener, err := net.Listen("unix", positiveRootSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = positiveRootListener.Close() })
	for i := range policySockets {
		runTclaudeLayerSocketVisibilityHelper(
			t, binary, helperBinary, phase0, plan,
			policySockets[i], policySockets[1-i], positiveRootSocket,
		)
	}

	// TCL-798. The constructed root has so far only ever served isolated
	// launches; this arm is the real-engine evidence that it also holds up with
	// the HOST network namespace, which is a much broader surface. It is the
	// evidence the Linux SocketClosed/SocketList capability flip under
	// network=open rests on.
	t.Run("host-network-constructed-root", func(t *testing.T) {
		hostNetworkPlan := resizePlanClone(plan)
		hostNetworkPlan.NetworkPosture = sandboxpolicy.NetworkHostOpen
		hostNetworkPlan.RootPosture = sandboxpolicy.RootConstructed
		require.Equal(t, sandboxpolicy.RootConstructed,
			hostNetworkPlan.EffectiveRootPosture())
		runTclaudeLayerHostNetworkConstructedRootHelper(
			t, binary, helperBinary, phase0, hostNetworkPlan,
			policySockets[0], policySockets[1], runtimeSocket, tmuxSocket,
			strconv.Itoa(os.Getpid()), hostLoopback.Addr().String(),
			tclaudeBinary, mounts, resolvedHostResolverTarget(t),
		)
	})
}

func runTclaudeLayerHostNetworkConstructedRootHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	allowedSocket, peerSocket, runtimeSocket, tmuxSocket,
	hostPID, hostLoopbackAddr, tclaudeBinary string,
	mounts tclaudeLayerSmokeMounts,
	resolverTarget string,
) {
	t.Helper()
	socketPaths := append(sandboxpolicy.AgentdSocketFloor(), allowedSocket)
	args, err := bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, nil, nil, nil, socketPaths, "", nil)
	require.NoError(t, err)
	require.NotContains(t, args, "--unshare-net",
		"this arm exists to prove the constructed root without network isolation")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary,
			"-test.run=^TestTclaudeLayerHostNetworkConstructedRootHelper$")...)
	cmd.Env = append(os.Environ(),
		smokeHostNetworkHelperEnv+"=1",
		agentipc.AgentHintEnvVar+"=1",
		agentipc.SessionIDEnvVar+"="+tclaudeLayerSmokeSessionID,
		smokeAllowedSocketEnv+"="+allowedSocket,
		smokePeerSocketEnv+"="+peerSocket,
		smokeRuntimeSocketEnv+"="+runtimeSocket,
		smokeTmuxSocketEnv+"="+tmuxSocket,
		smokeHostPIDEnv+"="+hostPID,
		smokeLoopbackAddrEnv+"="+hostLoopbackAddr,
		smokeTclaudeBinaryEnv+"="+tclaudeBinary,
		smokeMountROSourceEnv+"="+mounts.ReadOnlySource,
		smokeMountROGuestEnv+"="+mounts.ReadOnlyGuest,
		smokeMountRWSourceEnv+"="+mounts.ReadWriteSource,
		smokeMountRWGuestEnv+"="+mounts.ReadWriteGuest,
		smokeResolverTargetEnv+"="+resolverTarget,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer host-network constructed-root smoke timed out")
	}
	require.NoErrorf(t, err,
		"tclaude-layer host-network constructed-root smoke output: %s", output)
}

func TestTclaudeLayerHostNetworkConstructedRootHelper(t *testing.T) {
	if os.Getenv(smokeHostNetworkHelperEnv) != "1" {
		t.Skip("host-smoke host-network constructed-root helper subprocess")
	}
	allowedSocket := os.Getenv(smokeAllowedSocketEnv)
	peerSocket := os.Getenv(smokePeerSocketEnv)
	runtimeSocket := os.Getenv(smokeRuntimeSocketEnv)
	tmuxSocket := os.Getenv(smokeTmuxSocketEnv)
	hostPID := os.Getenv(smokeHostPIDEnv)
	hostLoopbackAddr := os.Getenv(smokeLoopbackAddrEnv)

	// 1. Ambient filesystem sockets are gone, which is the capability being
	//    claimed, and the /proc/<pid>/root route around it is closed too.
	if conn, err := net.DialTimeout("unix", runtimeSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient runtime socket remained reachable inside the host-network constructed root")
	}
	if conn, err := net.DialTimeout("unix", tmuxSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host tmux socket remained reachable despite the final applier hide")
	}
	if conn, err := net.DialTimeout("unix", peerSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("a non-allowlisted socket remained reachable")
	}
	procRootSocket := filepath.Join(
		"/proc", hostPID, "root", strings.TrimPrefix(runtimeSocket, "/"))
	if conn, err := net.DialTimeout("unix", procRootSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient socket remained reachable through a host process's /proc/<pid>/root")
	}

	// 2. The allowlisted socket and agentd still work.
	conn, err := net.DialTimeout("unix", allowedSocket, 250*time.Millisecond)
	require.NoError(t, err, "the allowlisted socket must be reachable")
	require.NoError(t, conn.Close())
	resolvedTclaude, err := exec.LookPath("tclaude")
	require.NoError(t, err, "the constructed root must put the tclaude CLI on PATH")
	assert.Equal(t, tclaudeLayerConstructedRootTclaudePath, resolvedTclaude)
	whoami, err := exec.Command("tclaude", "agent", "whoami").CombinedOutput()
	require.NoErrorf(t, err, "authenticated tclaude agent whoami inside namespace: %s", whoami)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(string(whoami)), "agt_"),
		"agentd must stay reachable through the constructed root; got %q", whoami)

	// 3. Host IP networking is exactly what this posture preserves. A host
	//    loopback listener the isolated posture could not see is reachable here,
	//    and the host resolver survived root construction.
	hostConn, err := net.DialTimeout("tcp4", hostLoopbackAddr, 2*time.Second)
	require.NoError(t, err,
		"the host network namespace must be preserved; host loopback is unreachable")
	require.NoError(t, hostConn.Close())
	resolv, err := os.ReadFile("/etc/resolv.conf")
	require.NoError(t, err,
		"a host-network sandbox that cannot read its resolver cannot resolve names")
	assert.NotEmpty(t, strings.TrimSpace(string(resolv)))
	// On a systemd-resolved-class runner /etc/resolv.conf is a symlink into
	// /run, which the constructed root does not have. The reopen of that one
	// target file is what keeps the read above from landing on nothing, so
	// assert the target itself is present rather than trusting the symlink.
	if resolverTarget := os.Getenv(smokeResolverTargetEnv); resolverTarget != "" {
		targetBytes, targetErr := os.ReadFile(resolverTarget)
		require.NoErrorf(t, targetErr,
			"the host resolver target %q must be reopened inside the constructed root",
			resolverTarget)
		assert.NotEmpty(t, strings.TrimSpace(string(targetBytes)))
	}
	// End-to-end: names actually resolve. This is the evidence that the reopen
	// works rather than merely that a file is readable. Retried because CI DNS
	// is a network dependency, but a persistent failure is a real failure.
	var lookupErr error
	for attempt := 0; attempt < 3; attempt++ {
		lookupCtx, cancelLookup := context.WithTimeout(
			context.Background(), 5*time.Second)
		_, lookupErr = net.DefaultResolver.LookupHost(lookupCtx, "github.com")
		cancelLookup()
		if lookupErr == nil {
			break
		}
	}
	require.NoError(t, lookupErr,
		"DNS must resolve inside a host-network constructed root; the host resolver reopen is what makes this work")

	// 4. TCL-866 projections land in a root the host never had to prepare.
	mountROGuest := os.Getenv(smokeMountROGuestEnv)
	mountRWGuest := os.Getenv(smokeMountRWGuestEnv)
	projected, err := os.ReadFile(filepath.Join(mountROGuest, "probe"))
	require.NoError(t, err,
		"a remapped read grant must appear at its sandbox path under this posture too")
	assert.Equal(t, "mounted-ro", string(projected))
	require.NoError(t,
		os.WriteFile(filepath.Join(mountRWGuest, "written-host-network"), []byte("ok"), 0o600))
	for _, hostSource := range []string{
		os.Getenv(smokeMountROSourceEnv), os.Getenv(smokeMountRWSourceEnv),
	} {
		require.NotEmpty(t, hostSource)
		_, err = os.Stat(hostSource)
		require.Errorf(t, err,
			"the host path %q must not also be exposed inside the sandbox", hostSource)
	}
}

func runTclaudeLayerSmokeHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	privateWriteDir TclaudeLayerPrivateWriteDir,
	allowed, outside, aliasFile, protectedFile, tmuxSocket, runtimeSocket,
	hostPID, hostLoopbackAddr, tclaudeBinary, privateOwnFile, privateSiblingDir string,
	mounts tclaudeLayerSmokeMounts,
) {
	t.Helper()
	args, err := bwrapArgs(phase0WriteDirs, plan, privateWriteDir)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary, "-test.run=^TestTclaudeLayerSmokeHelper$")...)
	cmd.Env = append(os.Environ(),
		tclaudeLayerSmokeHelperEnv+"=1",
		agentipc.AgentHintEnvVar+"=1",
		agentipc.SessionIDEnvVar+"="+tclaudeLayerSmokeSessionID,
		smokeAllowedEnv+"="+allowed,
		smokeOutsideEnv+"="+outside,
		smokeAliasFileEnv+"="+aliasFile,
		smokeProtectedFileEnv+"="+protectedFile,
		smokeTmuxSocketEnv+"="+tmuxSocket,
		smokeRuntimeSocketEnv+"="+runtimeSocket,
		smokeHostPIDEnv+"="+hostPID,
		smokeLoopbackAddrEnv+"="+hostLoopbackAddr,
		smokeTclaudeBinaryEnv+"="+tclaudeBinary,
		smokePrivateOwnFileEnv+"="+privateOwnFile,
		smokePrivateSiblingDirEnv+"="+privateSiblingDir,
		smokeMountROSourceEnv+"="+mounts.ReadOnlySource,
		smokeMountROGuestEnv+"="+mounts.ReadOnlyGuest,
		smokeMountRWSourceEnv+"="+mounts.ReadWriteSource,
		smokeMountRWGuestEnv+"="+mounts.ReadWriteGuest,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer host smoke timed out")
	}
	require.NoErrorf(t, err, "tclaude-layer host smoke output: %s", output)
}

func resizePlanClone(plan sandboxpolicy.MountPlan) sandboxpolicy.MountPlan {
	plan.Entries = append([]sandboxpolicy.MountEntry(nil), plan.Entries...)
	plan.Aliases = append([]sandboxpolicy.MountAlias(nil), plan.Aliases...)
	return plan
}

func assertTclaudeLayerResizeRoundTrip(
	t *testing.T,
	bwrapBinary, tclaudeBinary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
) {
	t.Helper()
	args, err := bwrapArgs(phase0WriteDirs, plan)
	require.NoError(t, err)

	// Keep the reporter beneath a live sh wrapper instead of letting sh exec
	// it as its final command. This is the production topology: bubblewrap's
	// reported child is `sh -c <harness command>`, while the TUI is a
	// descendant that only a process-group signal reliably reaches.
	reporterCommand := clcommon.ShellQuoteArg(helperBinary) +
		" -test.run=^TestTclaudeLayerResizeSmokeReporter$; " +
		"tclaude_resize_status=$?; exit \"$tclaude_resize_status\""
	relayArgs := []string{"session", tclaudeLayerWinchRelayCommand, "--", bwrapBinary}
	relayArgs = append(relayArgs, args...)
	relayArgs = append(relayArgs, "--", "sh", "-c", reporterCommand)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tclaudeBinary, relayArgs...)
	cmd.Env = append(os.Environ(), smokeResizeReporterEnv+"=1")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	require.NoError(t, err)
	defer func() { _ = ptmx.Close() }()

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
		close(lines)
	}()
	observed := make([]string, 0, 8)
	waitLine := func(prefix string) string {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("resize reporter exited before %s; output=%q", prefix, observed)
				}
				observed = append(observed, line)
				if strings.HasPrefix(line, prefix) {
					return line
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for %s; output=%q", prefix, observed)
			}
		}
	}

	ready := waitLine("READY ")
	assert.Contains(t, ready, "rows=24 cols=80")
	assert.Contains(t, ready, "parent=sh")
	assert.Contains(t, ready, "tty_fg=ENOTTY",
		"--new-session must keep the sandbox disconnected from the host controlling tty")

	require.NoError(t, pty.Setsize(ptmx, &pty.Winsize{Rows: 37, Cols: 113}))
	resized := waitLine("RESIZED ")
	assert.Contains(t, resized, "rows=37 cols=113")

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		require.NoErrorf(t, err, "resize relay output: %q", observed)
	case <-ctx.Done():
		t.Fatalf("resize relay did not exit after reporter completed; output=%q", observed)
	}
}

func TestTclaudeLayerResizeSmokeReporter(t *testing.T) {
	if os.Getenv(smokeResizeReporterEnv) != "1" {
		t.Skip("host-smoke terminal resize reporter")
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	fd := int(os.Stdin.Fd())
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	require.NoError(t, err)
	_, err = unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	require.Error(t, err, "sandbox unexpectedly reacquired a controlling tty")
	require.ErrorIs(t, err, syscall.ENOTTY)

	parentComm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(os.Getppid()), "comm"))
	require.NoError(t, err)
	fmt.Printf(
		"READY rows=%d cols=%d parent=%s tty_fg=ENOTTY pid=%d ppid=%d pgrp=%d\n",
		size.Row, size.Col, strings.TrimSpace(string(parentComm)),
		os.Getpid(), os.Getppid(), unix.Getpgrp(),
	)

	select {
	case <-winch:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapped descendant did not receive SIGWINCH")
	}
	size, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	require.NoError(t, err)
	fmt.Printf("RESIZED rows=%d cols=%d\n", size.Row, size.Col)
}

func TestTclaudeLayerSmokeHelper(t *testing.T) {
	if os.Getenv(tclaudeLayerSmokeHelperEnv) != "1" {
		t.Skip("host-smoke helper subprocess")
	}
	allowed := os.Getenv(smokeAllowedEnv)
	outside := os.Getenv(smokeOutsideEnv)
	aliasFile := os.Getenv(smokeAliasFileEnv)
	protectedFile := os.Getenv(smokeProtectedFileEnv)
	tmuxSocket := os.Getenv(smokeTmuxSocketEnv)
	runtimeSocket := os.Getenv(smokeRuntimeSocketEnv)
	hostPID := os.Getenv(smokeHostPIDEnv)
	hostLoopbackAddr := os.Getenv(smokeLoopbackAddrEnv)
	privateOwnFile := os.Getenv(smokePrivateOwnFileEnv)
	privateSiblingDir := os.Getenv(smokePrivateSiblingDirEnv)

	privateImage, err := os.ReadFile(privateOwnFile)
	require.NoError(t, err, "the current session's private attachment must be readable")
	assert.Equal(t, "own-session-image", string(privateImage))
	_, err = os.Stat(privateSiblingDir)
	require.Error(t, err, "a sibling session's private subtree must be absent")
	assert.True(t, errors.Is(err, syscall.ENOENT),
		"sibling private subtree must fail with ENOENT, got %v", err)
	require.NoError(t, os.WriteFile(filepath.Join(allowed, "written"), []byte("ok"), 0o600),
		"a writable child reopen inside an ordinary hide must stay writable")
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("write outside the allowed root unexpectedly succeeded")
	}
	deniedRootWrite := filepath.Join(filepath.Dir(outside), "blocked-at-denied-root")
	err = os.WriteFile(deniedRootWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "the topmost denied-root tmpfs must reject writes")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"denied-root tmpfs write must fail with EROFS, got %v", err)
	// The parent of the smoke fixture is auto-created in the isolated
	// posture's constructed tmpfs root but is not itself an explicit mount.
	// It must be read-only rather than accepting another throwaway write.
	unboundRootWrite := filepath.Join(filepath.Dir(filepath.Dir(outside)), "tclaude-sandbox-v2-phantom")
	err = os.WriteFile(unboundRootWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "the constructed root must reject writes to unbound paths")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"constructed-root write must fail with EROFS, got %v", err)
	got, err := os.ReadFile(aliasFile)
	require.NoError(t, err, "symlink alias must remain usable through the read-only base root")
	assert.Equal(t, "alias-ok", string(got))

	// TCL-866 real-kernel evidence: a host directory mounted at a different
	// sandbox path appears THERE with the authored access, and its host path is
	// absent inside the namespace.
	mountROSource := os.Getenv(smokeMountROSourceEnv)
	mountROGuest := os.Getenv(smokeMountROGuestEnv)
	mountRWSource := os.Getenv(smokeMountRWSourceEnv)
	mountRWGuest := os.Getenv(smokeMountRWGuestEnv)
	require.NotEmpty(t, mountROGuest)
	require.NotEmpty(t, mountRWGuest)
	projected, err := os.ReadFile(filepath.Join(mountROGuest, "probe"))
	require.NoError(t, err, "a read grant with a mount path must be readable at that sandbox path")
	assert.Equal(t, "mounted-ro", string(projected))
	err = os.WriteFile(filepath.Join(mountROGuest, "denied"), []byte("no"), 0o600)
	require.Error(t, err, "a read grant must stay read-only at its mount path")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"projected read-only mount write must fail with EROFS, got %v", err)
	require.NoError(t,
		os.WriteFile(filepath.Join(mountRWGuest, "written"), []byte("ok"), 0o600),
		"a write grant with a mount path must be writable at that sandbox path")
	for _, hostSource := range []string{mountROSource, mountRWSource} {
		require.NotEmpty(t, hostSource)
		_, err = os.Stat(hostSource)
		require.Errorf(t, err,
			"the host path %q must not also be exposed inside the sandbox", hostSource)
		assert.Truef(t, errors.Is(err, syscall.ENOENT),
			"host path %q must be absent with ENOENT, got %v", hostSource, err)
	}
	// Real-kernel proof of the absolute protected-root invariant (TCL-791):
	// nothing the profile can say makes this file readable inside the wall.
	if _, err := os.ReadFile(protectedFile); err == nil {
		t.Fatal("protected tclaude state unexpectedly remained readable inside the sandbox")
	}
	hiddenWrite := filepath.Join(filepath.Dir(protectedFile), "phantom")
	err = os.WriteFile(hiddenWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "a hidden path must reject writes instead of accepting phantom state")
	assert.True(t, errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EROFS),
		"hidden-path write must fail with ENOENT or EROFS, got %v", err)
	forbiddenProtectedSibling := filepath.Join(filepath.Dir(protectedFile), "forbidden-sibling")
	err = os.MkdirAll(forbiddenProtectedSibling, 0o700)
	require.Error(t, err, "a new ancestor-denied protected sibling must not be creatable")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"ancestor-denied protected path creation must fail with EROFS, got %v", err)
	if conn, err := net.DialTimeout("unix", tmuxSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host tmux socket remained reachable despite the final applier hide")
	}
	if conn, err := net.DialTimeout("unix", runtimeSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient runtime socket remained reachable inside the constructed root")
	}
	procRootSocket := filepath.Join("/proc", hostPID, "root", strings.TrimPrefix(runtimeSocket, "/"))
	if conn, err := net.DialTimeout("unix", procRootSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient runtime socket remained reachable through a host process's /proc/<pid>/root")
	}
	if conn, err := net.DialTimeout("tcp4", hostLoopbackAddr, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host-loopback TCP remained reachable across the isolated network namespace")
	}
	if conn, err := net.DialTimeout("tcp4", "1.1.1.1:53", 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("TCP egress unexpectedly succeeded inside the isolated network namespace")
	}
	loopback, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err, "loopback must be up inside the isolated network namespace")
	defer func() { _ = loopback.Close() }()
	conn, err := net.DialTimeout("tcp4", loopback.Addr().String(), 250*time.Millisecond)
	require.NoError(t, err, "namespace-local loopback round trip")
	_ = conn.Close()

	assertTclaudeLayerReapsOrphan(t)

	resolvedTclaude, err := exec.LookPath("tclaude")
	require.NoError(t, err, "the constructed root must put the tclaude CLI on PATH")
	assert.Equal(t, tclaudeLayerConstructedRootTclaudePath, resolvedTclaude)
	whoami, err := exec.Command("tclaude", "agent", "whoami").CombinedOutput()
	require.NoErrorf(t, err, "authenticated tclaude agent whoami inside namespace: %s", whoami)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(string(whoami)), "agt_"),
		"agentd must resolve a stable managed identity through bwrap ancestry; got %q", whoami)
}

func runTclaudeLayerSocketVisibilityHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	allowedSocket, peerSocket, positiveRootSocket string,
) {
	t.Helper()
	socketPaths := append(sandboxpolicy.AgentdSocketFloor(), allowedSocket)
	args, err := bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, nil, nil, nil, socketPaths, "", nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary, "-test.run=^TestTclaudeLayerSocketVisibilityHelper$")...)
	cmd.Env = append(os.Environ(),
		smokeSocketHelperEnv+"=1",
		smokeAllowedSocketEnv+"="+allowedSocket,
		smokePeerSocketEnv+"="+peerSocket,
		smokePositiveRootSocketEnv+"="+positiveRootSocket,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer socket visibility smoke timed out")
	}
	require.NoErrorf(t, err, "tclaude-layer socket visibility smoke output: %s", output)
}

func TestTclaudeLayerSocketVisibilityHelper(t *testing.T) {
	if os.Getenv(smokeSocketHelperEnv) != "1" {
		t.Skip("host-smoke socket-list helper subprocess")
	}
	allowed := os.Getenv(smokeAllowedSocketEnv)
	peer := os.Getenv(smokePeerSocketEnv)
	positiveRoot := os.Getenv(smokePositiveRootSocketEnv)
	conn, err := net.DialTimeout("unix", allowed, 250*time.Millisecond)
	require.NoError(t, err, "the current agent's allowlisted socket must be reachable")
	require.NoError(t, conn.Close())
	if peerConn, peerErr := net.DialTimeout("unix", peer, 250*time.Millisecond); peerErr == nil {
		_ = peerConn.Close()
		t.Fatal("a sibling agent's non-allowlisted socket remained reachable")
	}
	_, err = os.Lstat(peer)
	assert.True(t, errors.Is(err, syscall.ENOENT),
		"the sibling socket must be absent from the constructed root, got %v", err)
	positiveConn, err := net.DialTimeout("unix", positiveRoot, 250*time.Millisecond)
	require.NoError(t, err,
		"an unlisted socket beneath a readable/writable root is the disclosed Linux Partial remainder")
	require.NoError(t, positiveConn.Close())
}

func assertTclaudeLayerReapsOrphan(t *testing.T) {
	t.Helper()
	output, err := exec.Command(
		"/bin/sh",
		"-c",
		"sleep 0.2 </dev/null >/dev/null 2>&1 & echo $!",
	).Output()
	require.NoError(t, err, "launch orphan-reaping probe")
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err, "parse orphan-reaping probe PID")
	procPath := filepath.Join("/proc", strconv.Itoa(pid))
	require.Eventually(t, func() bool {
		_, err := os.Stat(procPath)
		return os.IsNotExist(err)
	}, 3*time.Second, 25*time.Millisecond,
		"bubblewrap PID 1 must reap orphaned harness subprocesses")
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
	for !tclaudeLayerSmokeAgentdReady(socket) {
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

// tclaudeLayerSmokeAgentdReady waits for the HTTP server rather than merely
// observing its bound Unix socket. Agentd performs startup work after binding
// and before Serve starts; advancing on connect alone can let this smoke's
// deliberately inert tmux listener block that work indefinitely.
func tclaudeLayerSmokeAgentdReady(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return false
	}
	if _, err := io.WriteString(conn,
		"GET /v1/whoami HTTP/1.1\r\nHost: _\r\nConnection: close\r\n\r\n"); err != nil {
		return false
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(status, "HTTP/")
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

// resolvedHostResolverTarget reports the real file /etc/resolv.conf points at
// when it is a symlink, and "" when it is an ordinary file. It is the fixture
// half of the reopen assertion: the smoke cannot know in advance whether the
// runner is systemd-resolved-class, so it measures the host and tells the
// helper what to demand.
func resolvedHostResolverTarget(t *testing.T) string {
	t.Helper()
	info, err := os.Lstat("/etc/resolv.conf")
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return ""
	}
	target, err := filepath.EvalSymlinks("/etc/resolv.conf")
	require.NoError(t, err, "the host resolver symlink must resolve")
	return target
}
