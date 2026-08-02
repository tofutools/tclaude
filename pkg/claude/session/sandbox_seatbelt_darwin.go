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
) (string, error) {
	switch posture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
	default:
		return "", fmt.Errorf("darwin tclaude-layer has invalid network posture %d", posture)
	}

	info, err := statDarwinSeatbelt(darwinSeatbeltExecutable)
	if err != nil {
		return "", fmt.Errorf(
			"darwin tclaude-layer requires %s for Seatbelt filesystem confinement: %w",
			darwinSeatbeltExecutable,
			err,
		)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf(
			"darwin tclaude-layer requires executable %s for Seatbelt filesystem confinement",
			darwinSeatbeltExecutable,
		)
	}
	if err := probeDarwinSeatbelt(darwinSeatbeltExecutable); err != nil {
		return "", fmt.Errorf(
			"darwin tclaude-layer cannot enforce the required Seatbelt deny-write capability: %w",
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
			"darwin tclaude-layer requires %s for Seatbelt filesystem confinement: %w",
			darwinSeatbeltExecutable, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf(
			"darwin tclaude-layer requires executable %s for Seatbelt filesystem confinement",
			darwinSeatbeltExecutable)
	}
	return nil
}

func resolveBwrapServerBinary(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (string, error) {
	return resolveBwrapBinary(posture, root)
}

func tclaudeLayerStackedCommand(
	string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	[]string,
	sandboxpolicy.MountPlan,
	string,
	string,
	string,
	bool,
	string,
) (string, error) {
	return "", fmt.Errorf(
		"stacked tclaude-layer is refused on macOS: nested Seatbelt is unsupported")
}

func tclaudeLayerCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	return tclaudeLayerDarwinCommand(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, harnessCommand, 0, 0)
}

func tclaudeLayerDarwinCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
	preserveFDs int,
	loopbackBindPort int,
) (string, error) {
	if tclaudeLayerPlanDeploysProxy(plan) {
		return darwinProxyLauncherCommand(darwinProxyLaunchSpec{
			Binary:           binary,
			Phase0WriteDirs:  phase0WriteDirs,
			PrivateWriteDirs: privateWriteDirs,
			FinalHideDirs:    finalHideDirs,
			ReadOnlyBinds:    readOnlyBinds,
			SocketPaths:      socketPaths,
			Plan:             plan,
			HarnessCommand:   harnessCommand,
			PreserveFDs:      preserveFDs,
			LoopbackBindPort: loopbackBindPort,
		})
	}
	if loopbackBindPort != 0 {
		return "", fmt.Errorf("Darwin loopback bind exception requires the filtering proxy floor")
	}
	return renderDarwinSeatbeltCommand(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, harnessCommand, netip.AddrPort{}, 0)
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
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
		if !tclaudeLayerPlanDeploysProxy(plan) &&
			!sandboxpolicy.FilteredNetworkRulesAreLoopbackOnly(plan.FilteredNetwork) {
			return "", fmt.Errorf(
				"darwin tclaude-layer filtered networking supports only a non-empty loopback-only list",
			)
		}
	default:
		return "", fmt.Errorf(
			"darwin tclaude-layer has invalid network posture %d",
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
		if i >= len(sandboxpolicy.AgentdSocketFloor()) {
			info, statErr := os.Lstat(socketPaths[i])
			if statErr != nil || info.Mode()&os.ModeSocket == 0 {
				return "", fmt.Errorf(
					"materialized unix socket %q changed before the tclaude-layer adapter rendered it",
					socketPaths[i])
			}
		}
		socketPaths[i] = canonicalSeatbeltOwnedPath(socketPaths[i])
	}
	readOnlyPaths, err := darwinSeatbeltReadOnlyPaths(readOnlyBinds)
	if err != nil {
		return "", err
	}
	profile, params, err := renderSeatbeltProfileWithLoopbackBind(
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
		canonicalPrivateWriteDirs...,
	)
	if err != nil {
		return "", err
	}

	command := clcommon.ShellQuoteArg(binary) + " -p " + clcommon.ShellQuoteArg(profile)
	for _, param := range params {
		command += " " + clcommon.ShellQuoteArg("-D"+param.name+"="+param.path)
	}
	command += " -- /bin/sh -c " + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}

func tclaudeLayerServerCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	serverCommand string,
) (string, error) {
	return tclaudeLayerServerCommandWithLoopbackBind(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, 0, serverCommand)
}

func tclaudeLayerServerCommandWithLoopbackBind(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	loopbackBindPort int,
	serverCommand string,
) (string, error) {
	return tclaudeLayerDarwinCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		socketPaths,
		plan,
		serverCommand,
		2,
		loopbackBindPort,
	)
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
			"darwin tclaude-layer requires TMPDIR for the Seatbelt runtime write carveout",
		)
	}
	path := filepath.Clean(raw)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf(
			"darwin tclaude-layer requires absolute TMPDIR under /private/var/folders, got %q",
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
			"darwin tclaude-layer refuses TMPDIR %q: the filesystem slice only "+
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
