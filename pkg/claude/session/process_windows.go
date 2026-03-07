//go:build windows

package session

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// IsProcessAlive checks if a process with the given PID is still running
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// On Windows, we can use tasklist to check if a process exists
	cmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	// tasklist returns "INFO: No tasks are running..." if process doesn't exist
	return !strings.Contains(string(output), "No tasks")
}

// GetParentPID returns the parent PID of a process
// Returns 0 if unable to determine
func GetParentPID(pid int) int {
	// Use wmic to get parent process ID
	cmd := exec.Command("wmic", "process", "where", "ProcessId="+strconv.Itoa(pid), "get", "ParentProcessId", "/value")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	// Output format: ParentProcessId=1234
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ParentProcessId=") {
			ppidStr := strings.TrimPrefix(line, "ParentProcessId=")
			ppid, _ := strconv.Atoi(strings.TrimSpace(ppidStr))
			return ppid
		}
	}
	return 0
}

// GetProcessName returns the name of a process
// Returns empty string if unable to determine
func GetProcessName(pid int) string {
	// Use wmic to get process name
	cmd := exec.Command("wmic", "process", "where", "ProcessId="+strconv.Itoa(pid), "get", "Name", "/value")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Output format: Name=node.exe
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name=") {
			name := strings.TrimPrefix(line, "Name=")
			name = strings.TrimSpace(name)
			// Remove .exe extension for consistency with Unix
			name = strings.TrimSuffix(name, ".exe")
			name = strings.TrimSuffix(name, ".EXE")
			return name
		}
	}
	return ""
}

// FindClaudePID walks up the process tree from the current process
// to find a parent process named "claude" or "node" (Claude Code runs as node)
// Returns the PID of the Claude process, or 0 if not found
func FindClaudePID() int {
	pid := os.Getppid()
	// Windows uses PID 0 for System Idle Process
	for pid > 0 {
		name := GetProcessName(pid)
		// Claude Code runs as a node process, but check for "claude" too
		// in case the binary is renamed or wrapped
		if name == "claude" || name == "node" {
			return pid
		}
		newPid := GetParentPID(pid)
		if newPid == pid || newPid == 0 {
			// Prevent infinite loop
			break
		}
		pid = newPid
	}
	return 0
}

// GetCurrentTmuxSession returns the current tmux session name if running inside tmux
// Returns empty string on Windows (tmux is not typically used on Windows)
func GetCurrentTmuxSession() string {
	// tmux is not typically available on Windows
	// Users on Windows with WSL might use tmux, but that would be in the Linux environment
	return ""
}
