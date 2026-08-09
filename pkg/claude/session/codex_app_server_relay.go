package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// codexAppServerRelayCmd is a deliberately dumb byte relay. Authorization is
// performed by Codex's authenticated loopback WebSocket listener after the
// HTTP upgrade crosses this Unix boundary.
func codexAppServerRelayCmd() *cobra.Command {
	var socketPath, upstream string
	cmd := &cobra.Command{
		Use:    "codex-app-server-relay",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunCodexAppServerRelay(context.Background(), socketPath, upstream)
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix listener path")
	cmd.Flags().StringVar(&upstream, "upstream", "", "loopback TCP upstream")
	_ = cmd.MarkFlagRequired("socket")
	_ = cmd.MarkFlagRequired("upstream")
	return cmd
}

func codexAppServerTokenConsumeCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:    "codex-app-server-token-consume",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := consumeCodexAppServerToken(path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), token)
			return err
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "one-shot capability file")
	_ = cmd.MarkFlagRequired("path")
	return cmd
}

func consumeCodexAppServerToken(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("codex app-server capability handoff must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 1024 {
		return "", errors.New("codex app-server capability handoff is not an owned private file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", errors.New("codex app-server capability handoff changed while opening")
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(data) > 1024 {
		return "", errors.New("codex app-server capability handoff is too large")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("codex app-server capability handoff is empty")
	}
	return token, nil
}

// RunCodexAppServerRelay exposes the relay for the real-Codex compatibility
// test. Production calls it only through the hidden subprocess command.
func RunCodexAppServerRelay(ctx context.Context, socketPath, upstream string) error {
	if !filepath.IsAbs(socketPath) {
		return errors.New("codex app-server relay socket must be absolute")
	}
	host, _, err := net.SplitHostPort(upstream)
	if err != nil {
		return fmt.Errorf("parse Codex app-server relay upstream: %w", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return errors.New("codex app-server relay upstream must be numeric loopback")
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale Codex app-server relay socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on Codex app-server relay socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("secure Codex app-server relay socket: %w", err)
	}
	for {
		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return acceptErr
		}
		go relayCodexAppServerConnection(client, upstream)
	}
}

func relayCodexAppServerConnection(client net.Conn, upstream string) {
	defer client.Close()
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}
