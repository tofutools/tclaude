//go:build linux

package agentd

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// openCodeProcessOwnsEndpoint verifies the listener through the kernel's
// socket inode tables. Merely observing a healthy HTTP response is not enough:
// another local process could have won the bind-close-exec race and use the
// authenticated health request to capture the server password.
func openCodeProcessOwnsEndpoint(rootPID int, endpoint string) bool {
	port, ok := openCodeEndpointPort(endpoint)
	if !ok {
		return false
	}
	inodes := openCodeListeningSocketInodes(port)
	if len(inodes) == 0 {
		return false
	}
	for _, pid := range openCodeProcessTreePIDs(rootPID) {
		entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(
				"/proc", strconv.Itoa(pid), "fd", entry.Name()))
			if err == nil && strings.HasPrefix(target, "socket:[") &&
				strings.HasSuffix(target, "]") {
				if _, found := inodes[strings.TrimSuffix(
					strings.TrimPrefix(target, "socket:["), "]")]; found {
					return true
				}
			}
		}
	}
	return false
}

func openCodeEndpointPort(endpoint string) (string, bool) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	_, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return "", false
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return "", false
	}
	return strings.ToUpper(strconv.FormatUint(port, 16)), true
}

func openCodeListeningSocketInodes(port string) map[string]struct{} {
	result := map[string]struct{}{}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) <= 9 || fields[3] != "0A" {
			continue
		}
		address, socketPort, found := strings.Cut(fields[1], ":")
		if found && address == "0100007F" &&
			strings.TrimLeft(socketPort, "0") == strings.TrimLeft(port, "0") {
			result[fields[9]] = struct{}{}
		}
	}
	return result
}

func openCodeProcessTreePIDs(rootPID int) []int {
	if rootPID <= 1 {
		return nil
	}
	result := []int{rootPID}
	seen := map[int]struct{}{rootPID: {}}
	for cursor := 0; cursor < len(result) && len(result) < 256; cursor++ {
		pid := result[cursor]
		data, err := os.ReadFile(filepath.Join(
			"/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
		if err != nil {
			continue
		}
		for _, rawChild := range strings.Fields(string(data)) {
			child, err := strconv.Atoi(rawChild)
			if err != nil || child <= 1 {
				continue
			}
			if _, exists := seen[child]; exists {
				continue
			}
			seen[child] = struct{}{}
			result = append(result, child)
		}
	}
	return result
}
