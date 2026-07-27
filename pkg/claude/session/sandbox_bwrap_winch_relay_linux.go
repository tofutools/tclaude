//go:build linux

package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
	"golang.org/x/sys/unix"
)

const tclaudeLayerWinchRelayCommand = "tclaude-layer-winch-relay"

type bwrapChildStatus struct {
	ChildPID int `json:"child-pid"`
}

type stackedRelayBindingOptions struct {
	ManifestPath   string
	ManifestSHA256 string
	Consume        bool
	ReadyPath      string
}

// tclaudeLayerWinchRelayCmd stays outside bubblewrap and outside the terminal
// I/O path. Bubblewrap inherits stdin/stdout/stderr directly; this process only
// turns the host PTY's SIGWINCH notification into the same fixed signal for the
// disconnected sandbox process group.
func tclaudeLayerWinchRelayCmd() *cobra.Command {
	var binding stackedRelayBindingOptions
	cmd := &cobra.Command{
		Use:    tclaudeLayerWinchRelayCommand + " -- <bwrap> [args...]",
		Short:  "Relay terminal resize notifications into tclaude-layer (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)

			code, err := runTclaudeLayerWinchRelay(args, winch, binding)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tclaude: terminal resize relay: %v\n", err)
				os.Exit(125)
			}
			os.Exit(code)
		},
	}
	cmd.Flags().StringVar(
		&binding.ManifestPath,
		"stacked-binding",
		"",
		"launch-owned stacked binding manifest (internal)",
	)
	cmd.Flags().StringVar(
		&binding.ManifestSHA256,
		"stacked-binding-sha256",
		"",
		"probe-pinned stacked binding manifest digest (internal)",
	)
	cmd.Flags().BoolVar(
		&binding.Consume,
		"stacked-consume",
		false,
		"unlink stacked staging names after opening them (internal)",
	)
	cmd.Flags().StringVar(
		&binding.ReadyPath,
		"stacked-ready",
		"",
		"write after the final binding fds reach bubblewrap (internal)",
	)
	return cmd
}

// runTclaudeLayerWinchRelay launches one bubblewrap argv, learns the
// host-visible identity of its initial sandbox process from bubblewrap itself,
// and forwards SIGWINCH to that process group. Signaling the group rather than
// the reported process is load-bearing because bubblewrap's --new-session
// helper is the group leader while production runs the TUI beneath
// `sh -c <harness command>`.
func runTclaudeLayerWinchRelay(
	argv []string,
	winch <-chan os.Signal,
	binding stackedRelayBindingOptions,
) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 125, fmt.Errorf("missing bubblewrap command")
	}
	bindingArgs, bindingFiles, err := prepareStackedRelayBinding(binding)
	if err != nil {
		return 125, err
	}
	defer func() {
		for _, file := range bindingFiles {
			_ = file.Close()
		}
	}()

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 125, fmt.Errorf("create bubblewrap status pipe: %w", err)
	}
	defer func() { _ = statusR.Close() }()

	childArgs := make([]string, 0, len(argv)+len(bindingArgs)+2)
	childArgs = append(childArgs, "--json-status-fd", "3")
	if len(bindingArgs) == 0 {
		childArgs = append(childArgs, argv[1:]...)
	} else {
		commandIndex := -1
		for index, arg := range argv[1:] {
			if arg == "--" {
				commandIndex = index + 1
				break
			}
		}
		if commandIndex < 0 {
			return 125, fmt.Errorf("stacked binding requires a bubblewrap command separator")
		}
		childArgs = append(childArgs, argv[1:commandIndex]...)
		childArgs = append(childArgs, bindingArgs...)
		childArgs = append(childArgs, argv[commandIndex:]...)
	}
	child := exec.Command(argv[0], childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	// ExtraFiles are installed consecutively from fd 3. Bubblewrap owns fd 3
	// for its status stream; the verified binding descriptors follow it.
	// Every --file/--ro-bind-data operation names its descriptor explicitly,
	// so bubblewrap preserves them during startup even on the minimum
	// supported 0.9.x series.
	child.ExtraFiles = append([]*os.File{statusW}, bindingFiles...)
	if err := child.Start(); err != nil {
		_ = statusW.Close()
		return 125, fmt.Errorf("start bubblewrap: %w", err)
	}
	_ = statusW.Close()

	waited := false
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	defer func() {
		if waited {
			return
		}
		_ = child.Process.Kill()
		<-waitCh
	}()

	var status bwrapChildStatus
	if err := json.NewDecoder(statusR).Decode(&status); err != nil {
		if errors.Is(err, io.EOF) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		_ = child.Process.Kill()
		<-waitCh
		waited = true
		return 125, fmt.Errorf("read bubblewrap child identity: %w", err)
	}
	if status.ChildPID <= 0 {
		return 125, fmt.Errorf("bubblewrap reported invalid child pid %d", status.ChildPID)
	}
	if binding.ReadyPath != "" {
		if err := writeStackedBindingReady(binding.ReadyPath); err != nil {
			return 125, err
		}
	}
	childPidfd, err := unix.PidfdOpen(status.ChildPID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		return 125, fmt.Errorf("pin bubblewrap child pid %d: %w", status.ChildPID, err)
	}
	defer func() { _ = unix.Close(childPidfd) }()

	for {
		select {
		case _, ok := <-winch:
			if !ok {
				winch = nil
				continue
			}
			if err := signalPinnedTclaudeLayerGroup(childPidfd, status.ChildPID); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				return 125, fmt.Errorf("forward SIGWINCH to sandbox process group: %w", err)
			}
		case waitErr := <-waitCh:
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
	}
}

func prepareStackedRelayBinding(
	options stackedRelayBindingOptions,
) ([]string, []*os.File, error) {
	if strings.TrimSpace(options.ManifestPath) == "" {
		if strings.TrimSpace(options.ManifestSHA256) != "" ||
			options.Consume || strings.TrimSpace(options.ReadyPath) != "" {
			return nil, nil, fmt.Errorf("stacked binding flags require a manifest")
		}
		return nil, nil, nil
	}
	expectedDigest := strings.ToLower(strings.TrimSpace(options.ManifestSHA256))
	if decoded, decodeErr := hex.DecodeString(expectedDigest); decodeErr != nil ||
		len(decoded) != sha256.Size {
		return nil, nil, fmt.Errorf("stacked binding manifest digest is invalid")
	}
	manifestPath, err := filepath.Abs(options.ManifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve stacked binding manifest: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read stacked binding manifest: %w", err)
	}
	if stackedBindingDigest(data) != expectedDigest {
		return nil, nil, fmt.Errorf("stacked binding manifest changed after capability probe")
	}
	var manifest stackedSandboxBindingManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, fmt.Errorf("parse stacked binding manifest: %w", err)
	}
	root := filepath.Clean(manifest.StageRoot)
	if manifest.Version != stackedSandboxBindingVersion ||
		!filepath.IsAbs(root) ||
		filepath.Dir(manifestPath) != root {
		return nil, nil, fmt.Errorf("stacked binding manifest has invalid authority")
	}
	if readyPath := filepath.Clean(strings.TrimSpace(options.ReadyPath)); readyPath != "." {
		if !filepath.IsAbs(readyPath) {
			return nil, nil, fmt.Errorf("stacked binding readiness path is not absolute")
		}
		relative, relErr := filepath.Rel(root, readyPath)
		if relErr != nil {
			return nil, nil, fmt.Errorf("resolve stacked binding readiness path: %w", relErr)
		}
		if relative == "." || (relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return nil, nil, fmt.Errorf(
				"stacked binding readiness path must be outside the staging root")
		}
	}

	files := make(
		[]*os.File,
		0,
		2+len(manifest.RuntimeFiles)+len(manifest.ManagedPolicy),
	)
	closeFiles := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	openBinding := func(binding stackedSandboxBindingFile) (*os.File, error) {
		path := filepath.Clean(binding.StagePath)
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("stacked binding path %q escapes its launch root", path)
		}
		source, openErr := os.Open(path)
		if openErr != nil {
			return nil, openErr
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return nil, statErr
		}
		if !info.Mode().IsRegular() || info.Size() != binding.Size {
			_ = source.Close()
			return nil, fmt.Errorf("stacked binding file %q changed shape", path)
		}
		memfd, memfdErr := unix.MemfdCreate(
			"tclaude-stacked-binding",
			unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING,
		)
		if memfdErr != nil {
			_ = source.Close()
			return nil, fmt.Errorf("create immutable stacked binding: %w", memfdErr)
		}
		file := os.NewFile(uintptr(memfd), "tclaude-stacked-binding")
		mode := os.FileMode(binding.Mode)
		if mode != 0o400 && mode != 0o500 {
			_ = source.Close()
			_ = file.Close()
			return nil, fmt.Errorf(
				"stacked binding file %q has invalid mode %04o",
				path,
				binding.Mode,
			)
		}
		if chmodErr := file.Chmod(mode); chmodErr != nil {
			_ = source.Close()
			_ = file.Close()
			return nil, fmt.Errorf("set immutable stacked binding mode: %w", chmodErr)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hash), source)
		closeSourceErr := source.Close()
		if copyErr != nil {
			_ = file.Close()
			return nil, copyErr
		}
		if closeSourceErr != nil {
			_ = file.Close()
			return nil, closeSourceErr
		}
		if written != binding.Size ||
			!strings.EqualFold(
				fmt.Sprintf("%x", hash.Sum(nil)),
				strings.TrimSpace(binding.SHA256),
			) {
			_ = file.Close()
			return nil, fmt.Errorf(
				"stacked binding file %q changed after capability probe",
				path,
			)
		}
		const immutableSeals = unix.F_SEAL_SEAL |
			unix.F_SEAL_SHRINK |
			unix.F_SEAL_GROW |
			unix.F_SEAL_WRITE
		if _, sealErr := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, immutableSeals); sealErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("seal immutable stacked binding %q: %w", path, sealErr)
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			_ = file.Close()
			return nil, seekErr
		}
		files = append(files, file)
		return file, nil
	}

	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		manifest.Engine.Destination !=
			filepath.Join(stackedBoundCodexRuntimeRoot, "bin", "codex") {
		return nil, nil, fmt.Errorf("stacked binding manifest has invalid engine destination")
	}
	if manifest.Engine.Destination == stackedBoundExecutablePath &&
		len(manifest.RuntimeFiles) != 0 {
		return nil, nil, fmt.Errorf("non-Codex stacked binding carries runtime files")
	}
	if manifest.Engine.Destination == stackedBoundExecutablePath &&
		manifest.ProbeHelper != nil &&
		(manifest.ProbeHelper.Destination != probehelper.BoundPath ||
			manifest.ProbeHelper.Mode != 0o500) {
		return nil, nil, fmt.Errorf(
			"%s: Claude stacked binding has an invalid sealed Go probe helper",
			probehelper.BindingFailureMarker,
		)
	}
	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		len(manifest.RuntimeFiles) == 0 {
		return nil, nil, fmt.Errorf("codex stacked binding omits its runtime closure")
	}
	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		manifest.ProbeHelper != nil {
		return nil, nil, fmt.Errorf("codex stacked binding carries a probe helper")
	}
	_, err = openBinding(manifest.Engine)
	if err != nil {
		closeFiles()
		return nil, nil, fmt.Errorf("stacked engine binding: %w", err)
	}
	childFDFor := func() string {
		const firstBindingChildFD = 4 // fd 3 is bubblewrap's JSON status pipe.
		return strconv.Itoa(firstBindingChildFD + len(files) - 1)
	}
	imageRoot := stackedBoundExecutableRoot
	if manifest.Engine.Destination != stackedBoundExecutablePath {
		imageRoot = stackedBoundCodexRuntimeRoot
	}
	// --file copies each sealed descriptor to a linked path in this
	// launch-private filesystem. The final remount makes the complete runtime
	// image immutable before bubblewrap starts the harness.
	args := []string{"--tmpfs", imageRoot}
	destinationDirs := make(map[string]struct{})
	addDestinationDirs := func(destination string) {
		parent := filepath.Dir(destination)
		var parents []string
		for parent != imageRoot && parent != "/" && parent != "/tmp" && parent != "." {
			parents = append(parents, parent)
			parent = filepath.Dir(parent)
		}
		for index := len(parents) - 1; index >= 0; index-- {
			if _, exists := destinationDirs[parents[index]]; exists {
				continue
			}
			destinationDirs[parents[index]] = struct{}{}
			args = append(args, "--dir", parents[index])
		}
	}
	addDestinationDirs(manifest.Engine.Destination)
	args = append(args,
		"--perms", fmt.Sprintf("%04o", manifest.Engine.Mode),
		"--file", childFDFor(), manifest.Engine.Destination,
	)

	for _, binding := range manifest.RuntimeFiles {
		destination := filepath.Clean(binding.Destination)
		relative, relativeErr := filepath.Rel(stackedBoundCodexRuntimeRoot, destination)
		if relativeErr != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			destination == manifest.Engine.Destination {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"stacked Codex runtime destination %q is invalid",
				binding.Destination,
			)
		}
		_, openErr := openBinding(binding)
		if openErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"stacked Codex runtime binding %q: %w",
				destination,
				openErr,
			)
		}
		addDestinationDirs(destination)
		args = append(args,
			"--perms", fmt.Sprintf("%04o", binding.Mode),
			"--file", childFDFor(), destination,
		)
	}
	if manifest.ProbeHelper != nil {
		binding := *manifest.ProbeHelper
		_, openErr := openBinding(binding)
		if openErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"%s: stacked probe helper binding: %w",
				probehelper.BindingFailureMarker,
				openErr,
			)
		}
		addDestinationDirs(binding.Destination)
		args = append(args,
			"--perms", fmt.Sprintf("%04o", binding.Mode),
			"--file", childFDFor(), binding.Destination,
		)
	}
	args = append(args, "--remount-ro", imageRoot)

	if manifest.FreezeClaudeManagedPolicy {
		etcEntries, readEtcErr := os.ReadDir("/etc")
		if readEtcErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf("snapshot host /etc entries: %w", readEtcErr)
		}
		args = append(args, "--tmpfs", "/etc")
		for _, entry := range etcEntries {
			if entry.Name() == "claude-code" {
				continue
			}
			args = append(args,
				"--ro-bind",
				filepath.Join("/etc", entry.Name()),
				filepath.Join("/etc", entry.Name()),
			)
		}
		args = append(args, "--dir", "/etc/claude-code")
		for _, binding := range manifest.ManagedPolicy {
			destination := filepath.Clean(binding.Destination)
			if filepath.IsAbs(destination) || destination == "." ||
				destination == ".." ||
				strings.HasPrefix(destination, ".."+string(filepath.Separator)) ||
				(destination != "managed-settings.json" &&
					!strings.HasPrefix(destination, "managed-settings.d"+string(filepath.Separator))) {
				closeFiles()
				return nil, nil, fmt.Errorf(
					"stacked managed-policy destination %q is invalid",
					binding.Destination,
				)
			}
			_, openErr := openBinding(binding)
			if openErr != nil {
				closeFiles()
				return nil, nil, fmt.Errorf(
					"stacked managed-policy binding %q: %w",
					destination,
					openErr,
				)
			}
			if strings.HasPrefix(destination, "managed-settings.d"+string(filepath.Separator)) {
				args = append(args, "--dir", "/etc/claude-code/managed-settings.d")
			}
			args = append(args,
				"--perms",
				fmt.Sprintf("%04o", binding.Mode),
				"--ro-bind-data",
				childFDFor(),
				filepath.Join("/etc/claude-code", destination),
			)
		}
		args = append(args, "--remount-ro", "/etc")
	}
	if options.Consume {
		if err := os.RemoveAll(root); err != nil {
			closeFiles()
			return nil, nil, fmt.Errorf("consume stacked binding staging root: %w", err)
		}
	}
	return args, files, nil
}

func writeStackedBindingReady(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return fmt.Errorf("stacked binding readiness path is not absolute")
	}
	fd, err := unix.Open(
		path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("write stacked binding readiness: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := file.WriteString("ready\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write stacked binding readiness: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close stacked binding readiness: %w", err)
	}
	return nil
}

// signalPinnedTclaudeLayerGroup resolves the child's current process group,
// pins its leader, then verifies membership again before addressing the group.
// Bubblewrap reports the child before --new-session finishes moving it into
// the final group, so this resolution must happen when each signal arrives.
// No user-selected PID or signal reaches this sink: the child identity came
// from bubblewrap, the pgid comes from the kernel, and SIGWINCH is fixed.
func signalPinnedTclaudeLayerGroup(childPidfd, childPID int) error {
	if err := unix.PidfdSendSignal(childPidfd, 0, nil, 0); err != nil {
		return err
	}
	pgid, err := unix.Getpgid(childPID)
	if err != nil {
		return err
	}
	if pgid <= 0 {
		return fmt.Errorf("bubblewrap child pid %d has invalid process group %d", childPID, pgid)
	}
	groupLeaderPidfd, err := unix.PidfdOpen(pgid, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(groupLeaderPidfd) }()
	if err := unix.PidfdSendSignal(groupLeaderPidfd, 0, nil, 0); err != nil {
		return err
	}
	currentPGID, err := unix.Getpgid(childPID)
	if err != nil {
		return err
	}
	if currentPGID != pgid {
		return fmt.Errorf("bubblewrap child moved from process group %d to %d", pgid, currentPGID)
	}
	return unix.Kill(-pgid, syscall.SIGWINCH)
}

func tclaudeLayerRelayExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 125, fmt.Errorf("wait for bubblewrap: %w", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 125, fmt.Errorf("inspect bubblewrap exit status: %w", err)
	}
	switch {
	case status.Exited():
		return status.ExitStatus(), nil
	case status.Signaled():
		return 128 + int(status.Signal()), nil
	default:
		return 125, fmt.Errorf("bubblewrap exited with unsupported wait status %v", status)
	}
}
