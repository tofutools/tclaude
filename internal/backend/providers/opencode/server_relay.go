package opencode

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const ServerRelayCommand = "__opencode-server-relay"

// ServerRelayRequest is supplied by the native launch artifact, never by the
// public operator API. The listener is already bound by the owning bootstrap;
// the confined relay must not reopen its authority through a pathname.
type ServerRelayRequest struct {
	ListenerFD int
	Target     string
	Executable string
	Args       []string
}

// ExecuteServerRelay runs inside the selected boundary. Both the native server
// and its control relay inherit that same boundary and exact environment.
func ExecuteServerRelay(ctx context.Context, request ServerRelayRequest) error {
	if request.ListenerFD < 3 || !filepath.IsAbs(request.Executable) {
		return fmt.Errorf("invalid OpenCode server relay invocation")
	}
	address, port, err := net.SplitHostPort(request.Target)
	if err != nil || address != "127.0.0.1" {
		return fmt.Errorf("OpenCode relay target must be exact IPv4 loopback")
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 1 || numericPort > 65535 {
		return fmt.Errorf("OpenCode relay port is invalid")
	}
	file := os.NewFile(uintptr(request.ListenerFD), "opencode-control-listener")
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return fmt.Errorf("adopt OpenCode control listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	if listener.Addr().Network() != "unix" {
		return fmt.Errorf("OpenCode relay requires an inherited Unix listener")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	child := exec.CommandContext(ctx, request.Executable, request.Args...)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return fmt.Errorf("start OpenCode behind control relay: %w", err)
	}
	// Keep the supervisor's existing process group. Host Stop owns that whole
	// group; direct signals to the supervisor must also reach the native server.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for {
			select {
			case received := <-signals:
				_ = child.Process.Signal(received)
			case <-relayCtx.Done():
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				relayServerStream(relayCtx, connection, request.Target)
			}()
		}
	}()
	waitErr := child.Wait()
	cancel()
	_ = listener.Close()
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("OpenCode server exited: %w", waitErr)
	}
	return nil
}

func relayServerStream(ctx context.Context, downstream net.Conn, target string) {
	defer downstream.Close()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", target)
	if err != nil {
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() {
		_ = downstream.Close()
		_ = upstream.Close()
	})
	defer stop()
	var copies sync.WaitGroup
	copies.Add(2)
	copyStream := func(destination, source net.Conn) {
		defer copies.Done()
		_, _ = io.Copy(destination, source)
		if half, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}
	go copyStream(upstream, downstream)
	go copyStream(downstream, upstream)
	copies.Wait()
}
