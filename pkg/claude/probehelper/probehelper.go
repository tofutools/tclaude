package probehelper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

const (
	internalPrefix = "__tclaude_stacked_probe_"

	StubMode              = internalPrefix + "stub"
	AFUnixMode            = internalPrefix + "af_unix"
	OpenCodeUnixRelayMode = opencodeapi.InheritedUnixRelayMode

	BoundPath               = "/tmp/.tclaude-stacked-harness/bin/probe-helper"
	EndpointFileName        = "endpoint"
	InnerPolicyFileName     = "inner-policy-failure"
	InnerPolicyFailureValue = "stacked_claude_inner_policy\n"
	BindingFailureMarker    = "TCLAUDE_STACKED_PROBE_HELPER_FAILURE"
	AFUnixDeniedExit        = 77
	AFUnixUntestableExit    = 78
	invalidInvocationExit   = 64
	stubFailureExit         = 74
	maxMessagesRequestBytes = 1 << 20
)

var (
	socketAFUnix = func() (int, error) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.CloseOnExec(fd)
		}
		return fd, err
	}
	closeFD = unix.Close
)

// Dispatch handles the two private stacked-probe modes before normal command
// construction can touch config, logging, or database state. The exact tokens
// deliberately are not Cobra commands and therefore never enter help or shell
// completion.
func Dispatch(args []string) (bool, int) {
	if len(args) < 2 || !strings.HasPrefix(args[1], internalPrefix) {
		return false, 0
	}
	switch args[1] {
	case AFUnixMode:
		if len(args) != 2 {
			return true, invalidInvocationExit
		}
		return true, afUnixExit()
	case StubMode:
		if len(args) != 6 {
			return true, invalidInvocationExit
		}
		if err := ServeStub(
			context.Background(),
			args[2],
			args[3],
			args[4],
			args[5],
		); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude stacked probe stub: %v\n", err)
			return true, stubFailureExit
		}
		return true, 0
	case OpenCodeUnixRelayMode:
		if len(args) < 6 || args[4] != "--" {
			return true, invalidInvocationExit
		}
		fd, err := opencodeapi.ParseInheritedRelayFD(args[2])
		if err != nil {
			return true, invalidInvocationExit
		}
		_ = unix.Close(4)
		if err := opencodeapi.ServeInheritedUnixRelay(
			context.Background(), fd, args[3], args[5:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode Unix relay: %v\n", err)
			return true, stubFailureExit
		}
		return true, 0
	case opencodeapi.UnixLaunchMode:
		if len(args) < 5 || args[3] != "--" {
			return true, invalidInvocationExit
		}
		if err := opencodeapi.ExecUnixRelayLaunch(args[2], args[4:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode Unix launcher: %v\n", err)
			return true, stubFailureExit
		}
		return true, 0
	case opencodeapi.UnixAttachShimMode:
		if len(args) < 9 || args[7] != "--" {
			return true, invalidInvocationExit
		}
		pid, pidErr := strconv.Atoi(args[2])
		device, deviceErr := strconv.ParseInt(args[4], 10, 64)
		inode, inodeErr := strconv.ParseInt(args[5], 10, 64)
		if pidErr != nil || deviceErr != nil || inodeErr != nil ||
			pid <= 1 || device <= 0 || inode <= 0 {
			return true, invalidInvocationExit
		}
		runtime := db.OpenCodeRuntime{
			PID: pid, ServerURL: args[6],
			Transport:           db.OpenCodeTransportUnixRelay,
			ControlSocketPath:   args[3],
			ControlSocketDevice: device,
			ControlSocketInode:  inode,
		}
		if err := opencodeapi.RunUnixAttachShim(
			context.Background(), runtime, args[8:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode attach shim: %v\n", err)
			return true, opencodeapi.ProcessExitCode(err, stubFailureExit)
		}
		return true, 0
	default:
		return true, invalidInvocationExit
	}
}

func afUnixExit() int {
	fd, err := socketAFUnix()
	if err == nil {
		_ = closeFD(fd)
		return 0
	}
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return AFUnixDeniedExit
	}
	return AFUnixUntestableExit
}

// ServeStub runs the launch-owned, credential-free Messages API stub. root is
// the only writable authority the hidden mode accepts; endpoint and failure
// evidence are published as fixed direct children through an O_NOFOLLOW
// directory descriptor.
func ServeStub(
	ctx context.Context,
	root, secret, command, marker string,
) error {
	rootFD, err := openProbeRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootFD) }()
	if !validSecret(secret) || strings.TrimSpace(command) == "" ||
		strings.TrimSpace(marker) == "" {
		return fmt.Errorf("invalid launch-owned probe arguments")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind launch-owned loopback endpoint: %w", err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) || address.Port <= 0 {
		return fmt.Errorf("loopback listener returned an invalid address")
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", address.Port)
	if err := publishAt(rootFD, EndpointFileName, endpoint, 0o600); err != nil {
		return fmt.Errorf("publish launch-owned endpoint: %w", err)
	}

	handler := &messagesHandler{
		rootFD:  rootFD,
		secret:  secret,
		command: command,
		marker:  marker,
		success: "TCLAUDE_STACKED_STUB_OK_" + secret,
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

func openProbeRoot(path string) (int, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "." || !filepath.IsAbs(clean) {
		return -1, fmt.Errorf("probe root is not absolute")
	}
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open launch-owned probe root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("inspect launch-owned probe root: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("launch-owned probe root has unsafe ownership or mode")
	}
	return fd, nil
}

func publishAt(rootFD int, name, value string, mode uint32) error {
	if filepath.Base(name) != name || name == "." || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("invalid probe evidence name")
	}
	tempName := "." + name + ".tmp"
	fd, err := unix.Openat(
		rootFD,
		tempName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		mode,
	)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(rootFD, tempName, 0) }()
	file := os.NewFile(uintptr(fd), tempName)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("wrap probe evidence descriptor")
	}
	_, writeErr := file.WriteString(value)
	syncErr := file.Sync()
	closeErr := file.Close()
	switch {
	case writeErr != nil:
		return writeErr
	case syncErr != nil:
		return syncErr
	case closeErr != nil:
		return closeErr
	}
	// Linking publishes the completed inode atomically without replacing an
	// existing evidence path. The deferred unlink then removes the temp name.
	return unix.Linkat(rootFD, tempName, rootFD, name, 0)
}

func validSecret(secret string) bool {
	if len(secret) != 48 {
		return false
	}
	for _, r := range secret {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
