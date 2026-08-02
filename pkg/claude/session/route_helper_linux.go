//go:build linux

package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"golang.org/x/sys/unix"
)

func tclaudeLayerRouteHelperCmd() *cobra.Command {
	var (
		socketPath       string
		agentID          string
		convID           string
		launchGeneration string
		credentialFD     int
		groupIDs         []int64
	)
	cmd := &cobra.Command{
		Use:    tclaudeLayerRouteHelperCommand,
		Short:  "Run the namespace-local group-route helper (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			credential, err := readRouteHelperCredentialFD(credentialFD)
			if err != nil {
				return err
			}
			config := routeadapter.HelperConfig{
				SocketPath: socketPath, AgentID: agentID, ConvID: convID,
				LaunchGeneration: launchGeneration, Credential: credential, GroupIDs: groupIDs,
			}
			if err := routeadapter.ValidateHelper(cmd.Context(), config); err != nil {
				return fmt.Errorf("authenticate route helper: %w", err)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "ready"); err != nil {
				return fmt.Errorf("signal route helper readiness: %w", err)
			}
			return routeadapter.RunHelper(cmd.Context(), config)
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "agentd Unix socket (internal)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "stable agent identity (internal)")
	cmd.Flags().StringVar(&convID, "conv-id", "", "conversation identity (internal)")
	cmd.Flags().StringVar(&launchGeneration, "launch-generation", "", "launch generation (internal)")
	cmd.Flags().IntVar(&credentialFD, "credential-fd", 0, "one-shot inherited helper credential descriptor (internal)")
	cmd.Flags().Int64SliceVar(&groupIDs, "group-id", nil, "explicit route-enabled group id (repeatable)")
	return cmd
}

func readRouteHelperCredentialFD(fd int) (string, error) {
	if fd <= 0 {
		return "", errors.New("route helper credential FD is required")
	}
	f := os.NewFile(uintptr(fd), "route-helper-credential")
	if f == nil {
		return "", errors.New("route helper credential FD is invalid")
	}
	defer f.Close()
	credential, err := io.ReadAll(io.LimitReader(bufio.NewReader(f), 4096))
	if err != nil {
		return "", fmt.Errorf("read route helper credential FD: %w", err)
	}
	value := strings.TrimSpace(string(credential))
	if value == "" {
		return "", errors.New("route helper credential FD was empty")
	}
	return value, nil
}

func tclaudeLayerRouteHelperBootstrapCmd() *cobra.Command {
	var handoffSocket string
	cmd := &cobra.Command{
		Use:    tclaudeLayerRouteHelperBootstrapCommand + " -- <relay> [args...]",
		Short:  "Receive the route helper credential FD and exec the Linux relay (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := strings.TrimSpace(handoffSocket)
			if path == "" {
				return errors.New("route helper handoff socket is required")
			}
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				return fmt.Errorf("connect route helper handoff socket: %w", err)
			}
			fd, recvErr := receiveRouteHelperCredentialFD(conn)
			_ = conn.Close()
			if recvErr != nil {
				return recvErr
			}
			if fd != 3 {
				if err := unix.Dup3(fd, 3, 0); err != nil {
					_ = unix.Close(fd)
					return fmt.Errorf("install route helper credential FD 3: %w", err)
				}
				_ = unix.Close(fd)
			}
			if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
				_ = unix.Close(3)
				return fmt.Errorf("exec route helper relay: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&handoffSocket, "handoff-socket", "", "one-shot route helper FD handoff socket (internal)")
	return cmd
}

func receiveRouteHelperCredentialFD(conn *net.UnixConn) (int, error) {
	if conn == nil {
		return -1, errors.New("route helper handoff connection is nil")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var recvErr error
	if err := raw.Read(func(socketFD uintptr) bool {
		payload := make([]byte, 1)
		oob := make([]byte, unix.CmsgSpace(4))
		_, _, _, _, err := unix.Recvmsg(int(socketFD), payload, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				return false
			}
			recvErr = err
			return true
		}
		messages, err := unix.ParseSocketControlMessage(oob)
		if err != nil {
			recvErr = err
			return true
		}
		for _, message := range messages {
			rights, err := unix.ParseUnixRights(&message)
			if err != nil {
				recvErr = err
				return true
			}
			if len(rights) != 1 || fd != -1 {
				recvErr = errors.New("route helper handoff must contain exactly one FD")
				return true
			}
			fd = rights[0]
		}
		return true
	}); err != nil {
		return -1, err
	}
	if recvErr != nil {
		return -1, fmt.Errorf("receive route helper credential FD: %w", recvErr)
	}
	if fd <= 0 {
		return -1, errors.New("route helper handoff did not contain a credential FD")
	}
	return fd, nil
}
