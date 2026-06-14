//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// IsProcessAlive checks if a process with the given PID is still running
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds, so we need to send signal 0
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// GetParentPID returns the parent PID of a process
// Returns 0 if unable to determine
func GetParentPID(pid int) int {
	// Try /proc first (Linux)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		// Format: pid (comm) state ppid ...
		// Find the closing paren to skip the comm field (which can contain spaces/parens)
		s := string(data)
		closeParenIdx := strings.LastIndex(s, ")")
		if closeParenIdx != -1 && closeParenIdx+2 < len(s) {
			fields := strings.Fields(s[closeParenIdx+2:])
			if len(fields) >= 2 {
				ppid, _ := strconv.Atoi(fields[1])
				return ppid
			}
		}
	}

	// Fallback: use ps command (works on macOS and Linux)
	cmd := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(output)))
	return ppid
}

// GetProcessName returns the name of a process
// Returns empty string if unable to determine
func GetProcessName(pid int) string {
	// Try /proc first (Linux)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err == nil {
		return strings.TrimSpace(string(data))
	}

	// Fallback: use ps command (works on macOS and Linux)
	cmd := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	// On macOS, ps might return the full path, extract just the name
	name := strings.TrimSpace(string(output))
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}
	return name
}

// FindClaudePID walks up the process tree from the current process to find
// the coding-harness ancestor — a parent named "claude"/"node" (Claude
// Code runs as node) or any other registered harness binary (e.g.
// "codex"). Returns its PID, or 0 if none is found. The harness-aware match
// (IsHarnessProcessName) is what lets a Codex hook callback record a real
// PID instead of 0 (JOH-160); a non-tmux row at PID 0 is otherwise reaped
// as a false-positive.
func FindClaudePID() int {
	pid := os.Getppid()
	for pid > 1 {
		name := GetProcessName(pid)
		if IsHarnessProcessName(name) {
			return pid
		}
		pid = GetParentPID(pid)
	}
	return 0
}

// GetCurrentTmuxSession returns the current tmux session name if running inside tmux
// Returns empty string if not in tmux
func GetCurrentTmuxSession() string {
	// Check if we're in tmux
	if os.Getenv("TMUX") == "" {
		return ""
	}
	cmd := clcommon.TmuxCommand("display-message", "-p", "#{session_name}")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
