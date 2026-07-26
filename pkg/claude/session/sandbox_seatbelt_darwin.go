//go:build darwin

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const darwinSeatbeltExecutable = "/usr/bin/sandbox-exec"

var (
	statDarwinSeatbelt  = os.Stat
	probeDarwinSeatbelt = func(binary string) error {
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
		cmd := exec.Command(
			binary,
			"-p", profile,
			"-DTCLAUDE_PROBE="+probePath,
			"--", "/bin/sh", "-c",
			`test ! -e "$TCLAUDE_PROBE" && ! touch "$TCLAUDE_PROBE"`,
		)
		cmd.Env = append(os.Environ(), "TCLAUDE_PROBE="+probePath)
		output, runErr := cmd.CombinedOutput()
		if runErr != nil {
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
)

func resolveBwrapBinary(posture sandboxpolicy.NetworkPosture) (string, error) {
	switch posture {
	case sandboxpolicy.NetworkHostOpen:
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		return "", fmt.Errorf(
			"darwin tclaude-layer does not yet support network_access none: " +
				"the filesystem slice cannot provide network/PID isolation, a constructed root, " +
				"or per-socket agentd allowlisting",
		)
	case sandboxpolicy.NetworkFiltered:
		return "", fmt.Errorf(
			"darwin tclaude-layer does not support reserved filtered networking: " +
				"the filesystem slice has no proxy-backed network applier",
		)
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

func tclaudeLayerCommand(
	binary string,
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	if plan.NetworkPosture != sandboxpolicy.NetworkHostOpen {
		return "", fmt.Errorf(
			"darwin tclaude-layer supports only host-open networking; posture %s is unavailable",
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
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return "", fmt.Errorf("resolve tclaude tmux socket directory: %w", err)
	}
	tmuxSocketDir = canonicalSeatbeltOwnedPath(tmuxSocketDir)
	runtimeTempDir, err := darwinSeatbeltRuntimeTempDir()
	if err != nil {
		return "", err
	}
	profile, params, err := renderSeatbeltProfile(
		filteredContract,
		[]string{canonicalSeatbeltOwnedPath(agentipc.CanonicalSocketPath())},
		breakGlassPaths,
		filteredPlan,
		protectedRoots,
		tmuxSocketDir,
		runtimeTempDir,
		darwinSeatbeltLstatIdentity,
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
		_, err := os.Stat(entry.Path)
		switch {
		case err == nil:
			filtered.Entries = append(filtered.Entries, entry)
		case os.IsNotExist(err):
		default:
			return sandboxpolicy.MountPlan{}, fmt.Errorf(
				"mount plan entry %d source %q: %w",
				i,
				entry.Path,
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

func tclaudeLayerLaunchOSSandbox(posture sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	if posture != sandboxpolicy.NetworkHostOpen {
		return harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}
	}
	return harness.LaunchOSSandbox{
		State: "on",
		Source: "tclaude-layer (Seatbelt/sandbox-exec; filesystem policy enforced; " +
			"host network and ambient Unix sockets reachable; no mount namespace; " +
			"hidden paths remain enumerable)",
		Unverified: true,
	}
}
