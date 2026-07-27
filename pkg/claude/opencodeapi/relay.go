package opencodeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const InheritedUnixRelayMode = "__tclaude_stacked_probe_opencode_unix_relay"
const UnixAttachShimMode = "__tclaude_stacked_probe_opencode_unix_attach"
const AttachURLPlaceholder = "__TCLAUDE_OPENCODE_ATTACH_URL__"

// ServeInheritedUnixRelay owns an inherited, already-bound Unix listener and
// forwards each stream to an OpenCode loopback listener in the same network
// namespace. The control socket pathname is neither accepted nor reopened.
func ServeInheritedUnixRelay(
	ctx context.Context,
	listenerFD int,
	target string,
	command []string,
) error {
	if listenerFD < 3 || len(command) == 0 {
		return fmt.Errorf("invalid OpenCode inherited-relay invocation")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("OpenCode inherited relay target must be IPv4 loopback")
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort <= 0 || numericPort > 65535 {
		return fmt.Errorf("OpenCode inherited relay target has invalid port")
	}
	file := os.NewFile(uintptr(listenerFD), "opencode-control-listener")
	if file == nil {
		return fmt.Errorf("wrap inherited OpenCode listener")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return fmt.Errorf("adopt inherited OpenCode listener: %w", err)
	}
	if listener.Addr().Network() != "unix" {
		_ = listener.Close()
		return fmt.Errorf("inherited OpenCode listener is not Unix")
	}

	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Stdout = io.Discard
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		_ = listener.Close()
		return fmt.Errorf("start OpenCode behind inherited relay: %w", err)
	}
	stopForwarding := forwardTerminationSignals(child)
	defer stopForwarding()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go relayOpenCodeStream(ctx, conn, target)
		}
	}()
	waitErr := child.Wait()
	_ = listener.Close()
	<-acceptDone
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("OpenCode behind inherited relay exited: %w", waitErr)
	}
	return nil
}

func relayOpenCodeStream(ctx context.Context, downstream net.Conn, target string) {
	defer downstream.Close()
	upstream, err := (&net.Dialer{}).DialContext(ctx, "tcp4", target)
	if err != nil {
		return
	}
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	copyOneWay := func(destination, source net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(destination, source)
		if writer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
	}
	go copyOneWay(upstream, downstream)
	go copyOneWay(downstream, upstream)
	wg.Wait()
}

func ParseInheritedRelayFD(raw string) (int, error) {
	if strings.TrimSpace(raw) != raw {
		return 0, fmt.Errorf("invalid inherited relay descriptor")
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return 0, fmt.Errorf("invalid inherited relay descriptor")
	}
	return fd, nil
}

// RunUnixAttachShim pre-binds the only TCP endpoint handed to upstream
// `opencode attach`, then forwards it through the same proven Unix transport
// used by agentd. The password stays inherited in the environment.
func RunUnixAttachShim(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	command []string,
) error {
	if err := db.ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return err
	}
	proof, err := dialVerifiedUnix(ctx, runtime)
	if err != nil {
		return fmt.Errorf("prove OpenCode Unix relay before attach: %w", err)
	}
	_ = proof.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("pre-bind OpenCode attach shim: %w", err)
	}
	defer listener.Close()
	localURL := "http://" + listener.Addr().String()
	argv := append([]string(nil), command...)
	replaced := false
	for i := range argv {
		if argv[i] == AttachURLPlaceholder {
			if replaced {
				return fmt.Errorf("OpenCode attach shim URL placeholder is repeated")
			}
			argv[i] = localURL
			replaced = true
		}
	}
	if len(argv) == 0 || !replaced {
		return fmt.Errorf("OpenCode attach shim command is incomplete")
	}
	child := exec.CommandContext(ctx, argv[0], argv[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return fmt.Errorf("start OpenCode attach client: %w", err)
	}
	stopForwarding := forwardTerminationSignals(child)
	defer stopForwarding()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				upstream, dialErr := dialVerifiedUnix(ctx, runtime)
				if dialErr != nil {
					_ = conn.Close()
					return
				}
				relayConnectedStreams(conn, upstream)
			}()
		}
	}()
	waitErr := child.Wait()
	_ = listener.Close()
	<-acceptDone
	return waitErr
}

func forwardTerminationSignals(child *exec.Cmd) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case received := <-signals:
			if child.Process != nil {
				_ = child.Process.Signal(received)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

func ProcessExitCode(err error, fallback int) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	return fallback
}

func relayConnectedStreams(left, right net.Conn) {
	defer left.Close()
	defer right.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	copyOneWay := func(destination, source net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(destination, source)
		if writer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
	}
	go copyOneWay(left, right)
	go copyOneWay(right, left)
	wg.Wait()
}
