//go:build darwin

package session

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	darwinSeatbeltExecutable   = "/usr/bin/sandbox-exec"
	darwinSeatbeltProbeTimeout = 5 * time.Second
)

var (
	statDarwinSeatbelt     = os.Stat
	runDarwinSeatbeltProbe = func(
		ctx context.Context,
		binary, profile, probePath string,
	) ([]byte, error) {
		cmd := exec.CommandContext(
			ctx,
			binary,
			"-p", profile,
			"-DTCLAUDE_PROBE="+probePath,
			"--", "/bin/sh", "-c",
			`test ! -e "$TCLAUDE_PROBE" && ! touch "$TCLAUDE_PROBE"`,
		)
		cmd.Env = append(os.Environ(), "TCLAUDE_PROBE="+probePath)
		return cmd.CombinedOutput()
	}
	probeDarwinSeatbelt = probeDarwinSeatbeltCapability
)

func probeDarwinSeatbeltCapability(binary string) error {
	tmpDir, err := darwinSeatbeltRuntimeTempDir()
	if err != nil {
		return err
	}
	probePath := filepath.Join(tmpDir, fmt.Sprintf(".tclaude-seatbelt-probe-%d", os.Getpid()))
	_ = os.Remove(probePath)
	profile := `(version 1)
(allow default)
(deny file-write* (literal (param "TCLAUDE_PROBE")))
`
	ctx, cancel := context.WithTimeout(context.Background(), darwinSeatbeltProbeTimeout)
	defer cancel()
	output, runErr := runDarwinSeatbeltProbe(ctx, binary, profile, probePath)
	if runErr != nil {
		if ctx.Err() != nil || errors.Is(runErr, context.DeadlineExceeded) {
			return fmt.Errorf(
				"sandbox-exec capability probe timed out after %s: %w",
				darwinSeatbeltProbeTimeout,
				context.DeadlineExceeded,
			)
		}
		return fmt.Errorf("sandbox-exec deny-write probe: %w: %s", runErr, output)
	}
	if _, statErr := os.Lstat(probePath); !os.IsNotExist(statErr) {
		_ = os.Remove(probePath)
		if statErr == nil {
			return fmt.Errorf("sandbox-exec deny-write probe unexpectedly created %s", probePath)
		}
		return fmt.Errorf("inspect sandbox-exec deny-write probe %s: %w", probePath, statErr)
	}
	return nil
}

// The root posture is ignored on Darwin: Seatbelt is a path filter over the
// host mount namespace and has no root to construct. It expresses the same
// socket-confinement intent with native denies instead (TCL-798).
func resolveBwrapBinary(
	posture sandboxpolicy.NetworkPosture,
	_ sandboxpolicy.RootPosture,
	_ ...bool,
) (string, error) {
	switch posture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
	default:
		return "", fmt.Errorf("tclaude’s sandbox on macOS has invalid network posture %d", posture)
	}

	info, err := statDarwinSeatbelt(darwinSeatbeltExecutable)
	if err != nil {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS requires %s for Seatbelt filesystem confinement: %w",
			darwinSeatbeltExecutable,
			err,
		)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS requires executable %s for Seatbelt filesystem confinement",
			darwinSeatbeltExecutable,
		)
	}
	if err := probeDarwinSeatbelt(darwinSeatbeltExecutable); err != nil {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS cannot enforce the required Seatbelt deny-write capability: %w",
			err,
		)
	}
	return darwinSeatbeltExecutable, nil
}

// tclaudeLayerToolingPresence is the fork-free half of resolveBwrapBinary:
// sandbox-exec exists and is executable. It deliberately does NOT run
// probeDarwinSeatbelt — that exec is what makes the availability predicate too
// expensive for a polled disclosure surface. The relay distinction is
// Linux-only (pidfd), so both boundaries answer identically here.
func tclaudeLayerToolingPresence(bool) error {
	info, err := statDarwinSeatbelt(darwinSeatbeltExecutable)
	if err != nil {
		return fmt.Errorf(
			"tclaude’s sandbox on macOS requires %s for Seatbelt filesystem confinement: %w",
			darwinSeatbeltExecutable, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf(
			"tclaude’s sandbox on macOS requires executable %s for Seatbelt filesystem confinement",
			darwinSeatbeltExecutable)
	}
	return nil
}

func resolveBwrapServerBinary(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	_ ...bool,
) (string, error) {
	return resolveBwrapBinary(posture, root)
}

// tclaudeLayerCommand renders a pane launch's Seatbelt boundary. A route-slot
// pool carries its pre-reservation and group-route authority to the renderer;
// a group-route helper without a pool is the Linux helper, which macOS does
// not run. A host-open floor needs no loopback bind exception.
func tclaudeLayerCommand(in tclaudeLayerRenderInput, opts tclaudeLayerLaunchOpts, harnessCommand string) (string, error) {
	if len(opts.RouteSlots) == 0 {
		if opts.RouteHelper != nil {
			return "", fmt.Errorf("linux group-route helper is unavailable")
		}
		// A reservation belongs to a route-slot pool: without one there is
		// nothing of this launch's for the renderer to release.
		opts.RouteReservation = nil
	}
	if tclaudeLayerPlanFloorPosture(in.Plan) == sandboxpolicy.NetworkHostOpen {
		opts.LoopbackBindPort = 0
	}
	return tclaudeLayerDarwinCommand(in, opts, harnessCommand)
}

func tclaudeLayerDarwinCommand(
	in tclaudeLayerRenderInput,
	opts tclaudeLayerLaunchOpts,
	harnessCommand string,
) (string, error) {
	if err := ValidateDarwinRouteSlots(opts.RouteSlots); err != nil {
		return "", err
	}
	if len(opts.RouteSlots) > 0 && !tclaudeLayerPlanDeploysProxy(in.Plan) && opts.RouteReservation == nil {
		reservation, reserveErr := ReserveDarwinRouteSlotsAt(opts.RouteSlots)
		if reserveErr != nil {
			return "", fmt.Errorf("reserve Darwin route slots before Seatbelt rendering: %w", reserveErr)
		}
		defer func() { _ = reservation.Release() }()
	}
	if tclaudeLayerPlanDeploysProxy(in.Plan) {
		if opts.RouteReservation != nil {
			if err := opts.RouteReservation.Release(); err != nil {
				return "", fmt.Errorf("release Darwin route slots for proxy launcher: %w", err)
			}
		}
		return darwinProxyLauncherCommand(darwinProxyLaunchSpec{
			Binary:           in.Binary,
			Phase0WriteDirs:  in.Phase0WriteDirs,
			PrivateWriteDirs: in.PrivateWriteDirs,
			FinalHideDirs:    in.FinalHideDirs,
			ReadOnlyBinds:    in.ReadOnlyBinds,
			SocketPaths:      in.SocketPaths,
			Plan:             in.Plan,
			HarnessCommand:   harnessCommand,
			LoopbackBindPort: opts.LoopbackBindPort,
			RouteSlots:       append([]int(nil), opts.RouteSlots...),
			RouteAuthority:   proxyRouteAuthorityConfigFromHelper(opts.RouteHelper),
		})
	}
	return renderDarwinSeatbeltCommandWithRouteSlots(
		in.Binary, in.Phase0WriteDirs, in.PrivateWriteDirs, in.FinalHideDirs,
		in.ReadOnlyBinds, in.SocketPaths, in.Plan, harnessCommand, netip.AddrPort{},
		opts.LoopbackBindPort, opts.RouteSlots)
}

func renderDarwinSeatbeltCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
	proxyEndpoint netip.AddrPort,
	loopbackBindPort int,
) (string, error) {
	return renderDarwinSeatbeltCommandWithRouteSlots(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, harnessCommand, proxyEndpoint,
		loopbackBindPort, nil)
}

func renderDarwinSeatbeltCommandWithRouteSlots(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
	proxyEndpoint netip.AddrPort,
	loopbackBindPort int,
	routeSlots []int,
) (string, error) {
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
		if !tclaudeLayerPlanDeploysProxy(plan) &&
			!sandboxpolicy.FilteredNetworkRulesAreLoopbackOnly(plan.FilteredNetwork) {
			return "", fmt.Errorf(
				"tclaude’s sandbox on macOS filtered networking supports only a non-empty loopback-only list",
			)
		}
	default:
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS has invalid network posture %d",
			plan.NetworkPosture,
		)
	}
	filteredContract, err := existingSeatbeltPositivePaths(
		"launch-contract write",
		phase0WriteDirs,
	)
	if err != nil {
		return "", err
	}
	filteredPlan, err := existingSeatbeltPlan(plan)
	if err != nil {
		return "", err
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return "", fmt.Errorf("resolve protected sandbox roots: %w", err)
	}
	// The unresolved spellings too: a dotfile-managed ~/.claude is a symlink,
	// and the ancestor unlink deny must name the link itself, which is what
	// host tools walk on their next access. A hide on a link spelling is a
	// redundant deny, never a widening.
	lexicalRoots, err := sandboxpolicy.LexicalProtectedPaths()
	if err != nil {
		return "", err
	}
	protectedRoots = append(protectedRoots, lexicalRoots...)
	protectedRoots = append(protectedRoots, finalHideDirs...)
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return "", fmt.Errorf("resolve tclaude tmux socket directory: %w", err)
	}
	tmuxSocketDir = canonicalSeatbeltOwnedPath(tmuxSocketDir)
	runtimeTempDir, err := darwinSeatbeltRuntimeTempDir()
	if err != nil {
		return "", err
	}
	canonicalPrivateWriteDirs := make(
		[]TclaudeLayerPrivateWriteDir,
		0,
		len(privateWriteDirs),
	)
	for _, privateDir := range privateWriteDirs {
		canonicalPrivateWriteDirs = append(
			canonicalPrivateWriteDirs,
			TclaudeLayerPrivateWriteDir{
				Parent:  canonicalSeatbeltOwnedPath(privateDir.Parent),
				Current: canonicalSeatbeltOwnedPath(privateDir.Current),
			},
		)
	}
	for i := range socketPaths {
		isCanonical := filepath.Clean(socketPaths[i]) == filepath.Clean(agentipc.CanonicalSocketPath())
		if i >= len(sandboxpolicy.AgentdSocketFloor()) {
			info, statErr := os.Lstat(socketPaths[i])
			if statErr != nil || info.Mode()&os.ModeSocket == 0 {
				return "", fmt.Errorf(
					"materialized unix socket %q changed before tclaude’s sandbox adapter rendered it",
					socketPaths[i])
			}
		}
		socketPaths[i] = canonicalSeatbeltOwnedPath(socketPaths[i])
		if isCanonical {
			// The directory is the narrow filesystem floor, while Seatbelt's
			// unix-socket matcher still needs the exact endpoint to reopen a
			// connect beneath an authored ancestor hide.
			socketPaths[i] = canonicalSeatbeltOwnedPath(agentipc.CanonicalSocketDir())
			socketPaths = append(socketPaths, canonicalSeatbeltOwnedPath(agentipc.CanonicalSocketPath()))
		}
	}
	readOnlyPaths, err := darwinSeatbeltReadOnlyPaths(readOnlyBinds)
	if err != nil {
		return "", err
	}
	profile, params, err := renderSeatbeltProfileWithLoopbackBindAndRouteSlots(
		filteredContract,
		socketPaths,
		filteredPlan,
		proxyEndpoint,
		protectedRoots,
		tmuxSocketDir,
		runtimeTempDir,
		darwinSeatbeltLstatIdentity,
		readOnlyPaths,
		loopbackBindPort,
		routeSlots,
		canonicalPrivateWriteDirs...,
	)
	if err != nil {
		return "", err
	}

	command := clcommon.ShellQuoteArg(binary) + " -p " + clcommon.ShellQuoteArg(profile)
	for _, param := range params {
		command += " " + clcommon.ShellQuoteArg("-D"+param.name+"="+param.path)
	}
	if len(routeSlots) > 0 {
		encoded, encodeErr := EncodeDarwinRouteSlots(routeSlots)
		if encodeErr != nil {
			return "", encodeErr
		}
		harnessCommand = "export " + DarwinRouteSlotsEnv + "=" +
			clcommon.ShellQuoteArg(encoded) + "; " + harnessCommand
	}
	command += sandboxExecShellPrefix() + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}

// tclaudeLayerServerCommand renders an agentd-owned server's Seatbelt
// boundary. The Darwin OpenCode server uses an authenticated TCP control
// endpoint, not the inherited Unix listener and relay executable used by the
// v4 Linux server boundary, so nothing above this launcher supplies fd 3 or
// 4; loopbackBindPort is that control endpoint's listener.
func tclaudeLayerServerCommand(in tclaudeLayerRenderInput, loopbackBindPort int, serverCommand string) (string, error) {
	return tclaudeLayerDarwinCommand(in, tclaudeLayerLaunchOpts{LoopbackBindPort: loopbackBindPort}, serverCommand)
}

func tclaudeLayerUnixRelayServerCommandArgs(
	_ TclaudeLayerLaunchSpec,
	bwrapArgv []string,
) ([]string, error) {
	return bwrapArgv, nil
}

func darwinSeatbeltReadOnlyPaths(
	binds []TclaudeLayerReadOnlyBind,
) ([]string, error) {
	out := make([]string, 0, len(binds))
	for _, bind := range binds {
		source := canonicalSeatbeltOwnedPath(bind.Source)
		target := canonicalSeatbeltOwnedPath(bind.Target)
		if source != target {
			return nil, fmt.Errorf(
				"darwin_seatbelt_path_projection: Seatbelt cannot project daemon-final read-only source %q onto target %q",
				source, target)
		}
		out = append(out, target)
	}
	return out, nil
}

func existingSeatbeltPositivePaths(label string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for i, path := range paths {
		path = filepath.Clean(path)
		_, err := os.Stat(path)
		switch {
		case err == nil:
			out = append(out, path)
		case os.IsNotExist(err):
		default:
			return nil, fmt.Errorf("%s %d source %q: %w", label, i, path, err)
		}
	}
	return out, nil
}

func existingSeatbeltPlan(plan sandboxpolicy.MountPlan) (sandboxpolicy.MountPlan, error) {
	filtered := plan
	filtered.Entries = make([]sandboxpolicy.MountEntry, 0, len(plan.Entries))
	for i, entry := range plan.Entries {
		if entry.Mode == sandboxpolicy.MountHide {
			filtered.Entries = append(filtered.Entries, entry)
			continue
		}
		if entry.Mode == sandboxpolicy.MountTmpfs {
			// Named rather than reported as an unrecognized mode number:
			// Seatbelt filters paths in the host namespace and has no mount to
			// create, so there is nothing to stat and nothing to filter.
			// ValidateTmpfsSupport refuses this launch before it reaches here;
			// this is the backstop that keeps the reason attributable.
			return sandboxpolicy.MountPlan{}, fmt.Errorf(
				"seatbelt_tmpfs_mount: Seatbelt cannot mount a temporary filesystem at sandbox path %q (mount plan entry %d); a tmpfs requires a mount namespace, which only tclaude’s sandbox on Linux provides",
				entry.Path, i)
		}
		if entry.Mode != sandboxpolicy.MountRO && entry.Mode != sandboxpolicy.MountRW {
			return sandboxpolicy.MountPlan{}, fmt.Errorf(
				"mount plan entry %d has invalid mode %d",
				i,
				entry.Mode,
			)
		}
		// Stat the HOST source, not the sandbox path. For a remapped entry those
		// differ, and statting the sandbox path would drop the entry as "missing
		// source" — a silent fallback that would skip the explicit refusal
		// seatbeltCommandArgs makes for exactly this shape.
		source := entry.SourcePath()
		_, err := os.Stat(source)
		switch {
		case err == nil:
			filtered.Entries = append(filtered.Entries, entry)
		case os.IsNotExist(err):
		default:
			return sandboxpolicy.MountPlan{}, fmt.Errorf(
				"mount plan entry %d source %q: %w",
				i,
				source,
				err,
			)
		}
	}
	return filtered, nil
}

func darwinSeatbeltRuntimeTempDir() (string, error) {
	raw := strings.TrimSpace(os.Getenv("TMPDIR"))
	if raw == "" {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS requires TMPDIR for the Seatbelt runtime write carveout",
		)
	}
	path := filepath.Clean(raw)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS requires absolute TMPDIR under /private/var/folders, got %q",
			raw,
		)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve darwin TMPDIR %q: %w", raw, err)
	}
	const privateVarFolders = "/private/var/folders"
	if !sandboxpolicy.PathContainsOrEqual(privateVarFolders, path) {
		return "", fmt.Errorf(
			"tclaude’s sandbox on macOS refuses TMPDIR %q: the filesystem slice only "+
				"carves the standard %s runtime tree",
			path,
			privateVarFolders,
		)
	}
	return path, nil
}

func canonicalSeatbeltOwnedPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

func darwinSeatbeltLstatIdentity(path string) (seatbeltFileIdentity, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return seatbeltFileIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return seatbeltFileIdentity{}, false
	}
	return seatbeltFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}, true
}

func tclaudeLayerLaunchOSSandbox(
	posture sandboxpolicy.NetworkPosture,
	_ sandboxpolicy.RootPosture,
) harness.LaunchOSSandbox {
	switch posture {
	// Source names the mechanism and posture that decided. The Darwin fidelity
	// caveats — no PID isolation, no constructed root, no mount namespace,
	// enumerable hidden paths, host reachability — belong to the badge's
	// partial-fidelity sentence and are stated there, once (TCL-790).
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		return harness.LaunchOSSandbox{
			State: "on",
			Source: "tclaude-layer (Seatbelt/sandbox-exec; isolated network; " +
				"host loopback/IDE bridge unavailable; agentd socket allowlisted)",
			Unverified: true,
		}
	case sandboxpolicy.NetworkHostOpen:
		return harness.LaunchOSSandbox{
			State:      "on",
			Source:     "tclaude-layer (Seatbelt/sandbox-exec; host network)",
			Unverified: true,
		}
	case sandboxpolicy.NetworkFiltered:
		return harness.LaunchOSSandbox{
			State: "on",
			Source: "tclaude-layer (Seatbelt/sandbox-exec; local access uses real " +
				"host loopback and reopens host-local services/IDE bridge)",
			FilteredNetwork: true,
			Unverified:      true,
		}
	default:
		return harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}
	}
}

func tclaudeLayerOpenCodeLaunchOSSandbox() harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State: "on",
		Source: "tclaude-layer (Seatbelt/sandbox-exec; OpenCode tool-executing server confined; " +
			"mutable XDG privacy covers data/cache/state only; config-base writes are not redirected)",
		Unverified: true,
	}
}

func validateTclaudeLayerHarness(harnessName string) error {
	return nil
}
