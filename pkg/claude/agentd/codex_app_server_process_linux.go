//go:build linux

package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func readCodexAppServerProcessIdentity(pid int, socketPath string) (string, error) {
	if pid <= 1 {
		return "", errorsNewCodexProcessIdentity("invalid pid")
	}
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return "", err
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if !codexAppServerArgsTargetSocket(args, socketPath) {
		return "", fmt.Errorf("process %d argv does not target the recorded Codex app-server socket", pid)
	}
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return "", err
	}
	// comm is parenthesized and may contain spaces or ')'; the last closing
	// parenthesis is the only safe boundary before the fixed-position fields.
	close := bytes.LastIndex(stat, []byte(") "))
	if close < 0 {
		return "", errorsNewCodexProcessIdentity("malformed /proc stat")
	}
	fields := strings.Fields(string(stat[close+2:]))
	// fields begins at field 3 (state); process starttime is field 22.
	if len(fields) <= 19 {
		return "", errorsNewCodexProcessIdentity("short /proc stat")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes.Join([][]byte{
		[]byte(strconv.Itoa(pid)), bytes.TrimSpace(bootID), []byte(fields[19]), cmdline,
	}, []byte{0}))
	return hex.EncodeToString(digest[:]), nil
}

// Linux PID files written inside a fresh bwrap procfs contain namespace-local
// PIDs. Resolve both relay and server from their live socket inodes instead;
// /proc/<pid>/net/* lets agentd inspect the target network namespace without
// joining it or sending traffic to the listener.
func discoverCodexAppServerRelayPID(ctx context.Context, socketPath string) (int, error) {
	for {
		inode, err := findUnixSocketInodeAcrossNamespaces(socketPath)
		if err == nil {
			if pid := findProcessHoldingSocket(inode); pid > 1 {
				return pid, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("discover Codex app-server relay owner: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func discoverCodexAppServerPID(ctx context.Context, socketPath, _ string) (int, error) {
	endpoint, err := readCodexAppServerEndpoint(socketPath)
	if err != nil {
		return 0, err
	}
	relayPID, err := discoverCodexAppServerRelayPID(ctx, socketPath)
	if err != nil {
		return 0, err
	}
	port, err := codexEndpointPort(endpoint)
	if err != nil {
		return 0, err
	}
	for {
		inode, findErr := findTCPListenerInode(filepath.Join("/proc", strconv.Itoa(relayPID), "net", "tcp"), port)
		if findErr == nil {
			if pid := findProcessHoldingSocket(inode); pid > 1 && codexServerArgsTargetEndpoint(pid, endpoint) {
				return pid, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("discover native Codex app-server listener owner: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func processOwnsCodexAppServerEndpoint(pid int, endpoint string) bool {
	port, err := codexEndpointPort(endpoint)
	if err != nil || !codexServerArgsTargetEndpoint(pid, endpoint) {
		return false
	}
	inode, err := findTCPListenerInode(filepath.Join("/proc", strconv.Itoa(pid), "net", "tcp"), port)
	return err == nil && processHoldsSocket(pid, inode)
}

func processOwnsCodexAppServerRelayEndpoint(pid int, socketPath, endpoint string) bool {
	port, err := codexEndpointPort(endpoint)
	if err != nil || !codexRelayArgsTargetEndpoint(pid, socketPath, endpoint) {
		return false
	}
	inode, err := findTCPListenerInode(filepath.Join("/proc", strconv.Itoa(pid), "net", "tcp"), port)
	return err == nil && processHoldsSocket(pid, inode)
}

func codexEndpointPort(endpoint string) (int, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "ws" || parsed.Hostname() != "127.0.0.1" {
		return 0, fmt.Errorf("invalid Codex app-server loopback endpoint")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid Codex app-server loopback port")
	}
	return port, nil
}

func codexServerArgsTargetEndpoint(pid int, endpoint string) bool {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	for i := 0; i+2 < len(args); i++ {
		if string(args[i]) == "app-server" && string(args[i+1]) == "--listen" && string(args[i+2]) == endpoint {
			return true
		}
	}
	return false
}

func codexRelayArgsTargetEndpoint(pid int, socketPath, endpoint string) bool {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if !codexAppServerArgsTargetSocket(args, socketPath) {
		return false
	}
	for i := 0; i+1 < len(args); i++ {
		if string(args[i]) == "--listen" && string(args[i+1]) == endpoint {
			return true
		}
	}
	return false
}

func findUnixSocketInodeAcrossNamespaces(socketPath string) (string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "net", "unix"))
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 7 && strings.HasSuffix(line, " "+socketPath) {
				return fields[6], nil
			}
		}
	}
	return "", os.ErrNotExist
}

func findTCPListenerInode(tablePath string, port int) (string, error) {
	data, err := os.ReadFile(tablePath)
	if err != nil {
		return "", err
	}
	wantPort := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) <= 9 || fields[3] != "0A" {
			continue
		}
		address, rawPort, found := strings.Cut(fields[1], ":")
		if found && address == "0100007F" &&
			strings.TrimLeft(rawPort, "0") == strings.TrimLeft(wantPort, "0") {
			return fields[9], nil
		}
	}
	return "", os.ErrNotExist
}

func findProcessHoldingSocket(inode string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 1 && processHoldsSocket(pid, inode) {
			return pid
		}
	}
	return 0
}

func processHoldsSocket(pid int, inode string) bool {
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return false
	}
	want := "socket:[" + inode + "]"
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err == nil && target == want {
			return true
		}
	}
	return false
}

func codexAppServerArgsTargetSocket(args [][]byte, socketPath string) bool {
	for i := 0; i+2 < len(args); i++ {
		if string(args[i]) == "codex-app-server-relay" && string(args[i+1]) == "--socket" && string(args[i+2]) == socketPath {
			return true
		}
	}
	return false
}

func errorsNewCodexProcessIdentity(detail string) error {
	return fmt.Errorf("codex app-server process identity: %s", detail)
}
