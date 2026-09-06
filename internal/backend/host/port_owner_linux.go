//go:build linux

package host

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processTreeOwnsLoopbackPort(rootPID, port int) (bool, error) {
	inodes, err := loopbackListenerInodes(port)
	if err != nil {
		return false, err
	}
	if len(inodes) == 0 {
		return false, nil
	}
	for _, pid := range processTreePIDs(rootPID) {
		entries, readErr := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return false, readErr
		}
		for _, entry := range entries {
			target, readErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
			if readErr != nil {
				continue
			}
			if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") &&
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
				return true, nil
			}
		}
	}
	return false, nil
}

func loopbackListenerInodes(port int) (map[string]bool, error) {
	result := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			address, rawPort, ok := strings.Cut(fields[1], ":")
			if !ok || !isLoopbackHex(address) {
				continue
			}
			parsed, parseErr := strconv.ParseUint(rawPort, 16, 16)
			if parseErr == nil && int(parsed) == port {
				result[fields[9]] = true
			}
		}
		scanErr := scanner.Err()
		_ = file.Close()
		if scanErr != nil {
			return nil, scanErr
		}
	}
	return result, nil
}

func isLoopbackHex(address string) bool {
	// /proc/net/tcp is little-endian (127.0.0.1 => 0100007F). tcp6 may
	// expose ::1 or an IPv4-mapped address; OpenCode is launched on IPv4.
	return address == "0100007F" || address == "00000000000000000000000001000000"
}

func processTreePIDs(rootPID int) []int {
	result := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for index := 0; index < len(result); index++ {
		path := filepath.Join("/proc", strconv.Itoa(result[index]), "task", strconv.Itoa(result[index]), "children")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, raw := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(raw)
			if err == nil && pid > 1 && !seen[pid] {
				seen[pid] = true
				result = append(result, pid)
			}
		}
	}
	return result
}
