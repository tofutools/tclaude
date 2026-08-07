//go:build linux

package portowner

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ProcessOwnsLoopbackPort verifies the listener through the kernel's socket
// inode tables: it reads the inodes of every 127.0.0.1:port listening socket,
// then looks for one of them among the open descriptors of the subtree rooted
// at rootPID.
//
// Matching on the socket INODE rather than on a pid reported by a port lookup
// is what makes this a proof. The inode names one kernel object, so a process
// in the subtree holding that descriptor is holding the very socket a dial to
// 127.0.0.1:port will reach.
func ProcessOwnsLoopbackPort(rootPID, port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	inodes := listeningSocketInodes(hexPort(port))
	if len(inodes) == 0 {
		return false
	}
	for _, pid := range ProcessSubtree(rootPID) {
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

// HasLoopbackListener reports whether ANY process is listening on
// 127.0.0.1:port.
//
// It exists to tell two failures apart without touching the socket: a harness
// that never came up, and one whose port was won by something else. That
// distinction is the difference between "wait longer / read the harness log"
// and "you lost the bind race", and a caller with only a yes/no ownership
// answer cannot make it.
//
// Says nothing about WHOSE listener it is. Only ProcessOwnsLoopbackPort
// answers that, and nothing may be sent to the port until it does.
func HasLoopbackListener(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	return len(listeningSocketInodes(hexPort(port))) > 0
}

// ProcessInSubtree reports whether candidatePID is rootPID or one of its
// descendants.
func ProcessInSubtree(rootPID, candidatePID int) bool {
	return slices.Contains(ProcessSubtree(rootPID), candidatePID)
}

// ProcessSubtree returns rootPID followed by its descendants, breadth-first.
// The walk is bounded so a pathological or adversarial process tree cannot turn
// an ownership check into an unbounded scan.
func ProcessSubtree(rootPID int) []int {
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
		for rawChild := range strings.FieldsSeq(string(data)) {
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

// hexPort renders a port the way /proc/net/tcp spells it: uppercase hex.
func hexPort(port int) string {
	return strings.ToUpper(strconv.FormatUint(uint64(port), 16))
}

// listeningSocketInodes collects the inodes of every socket LISTENing (state
// 0A) on 127.0.0.1 (address 0100007F, little-endian) at this port.
func listeningSocketInodes(port string) map[string]struct{} {
	result := map[string]struct{}{}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return result
	}
	for line := range strings.SplitSeq(string(data), "\n") {
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
