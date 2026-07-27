package probehelper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	internalPrefix = "__tclaude_stacked_probe_"

	StubMode   = internalPrefix + "stub"
	AFUnixMode = internalPrefix + "af_unix"

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
		return unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
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
	defer unix.Close(rootFD)
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
	fd, err := unix.Openat(
		rootFD,
		name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		mode,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
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
	default:
		return closeErr
	}
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
