//go:build darwin

package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func proxyRouteAuthorityConfigFromHelper(helper *TclaudeLayerRouteHelper) *proxyRouteAuthorityConfig {
	if helper == nil {
		return nil
	}
	return &proxyRouteAuthorityConfig{
		SocketPath: helper.SocketPath, AgentID: helper.AgentID,
		ConvID: helper.ConvID, LaunchGeneration: helper.LaunchGeneration,
		GroupIDs:          append([]int64(nil), helper.GroupIDs...),
		HandoffSocketPath: helper.HandoffSocketPath,
	}
}

// receiveProxyRouteCredential consumes the established one-shot descriptor
// handoff. The returned bearer exists only in process memory and is never
// encoded in a command, environment, hostname, or readable file.
func receiveProxyRouteCredential(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("route capability handoff path is empty")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return "", fmt.Errorf("connect route capability handoff: %w", err)
	}
	defer conn.Close()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return "", errors.New("route capability handoff is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return "", err
	}
	fd := -1
	var recvErr error
	if err := raw.Read(func(socketFD uintptr) bool {
		payload := make([]byte, 1)
		oob := make([]byte, unix.CmsgSpace(4))
		_, _, _, _, err := unix.Recvmsg(int(socketFD), payload, oob, 0)
		if err != nil {
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
			if err != nil || len(rights) != 1 || fd != -1 {
				recvErr = errors.New("route capability handoff must contain exactly one descriptor")
				return true
			}
			fd = rights[0]
		}
		return true
	}); err != nil {
		return "", err
	}
	if recvErr != nil || fd <= 0 {
		if fd > 0 {
			_ = unix.Close(fd)
		}
		if recvErr == nil {
			recvErr = errors.New("route capability handoff did not contain a descriptor")
		}
		return "", recvErr
	}
	f := os.NewFile(uintptr(fd), "route-capability")
	if f == nil {
		_ = unix.Close(fd)
		return "", errors.New("route capability descriptor is invalid")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("read route capability descriptor: %w", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		return "", errors.New("route capability descriptor was empty")
	}
	return credential, nil
}
