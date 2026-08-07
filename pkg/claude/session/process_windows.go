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

// GetProcessExeName returns the basename of a process's executable. wmic's
// Name is already the image name, so on Windows this is GetProcessName.
// Native Windows is not a supported target; this exists so the shared
// harness-ancestor predicate compiles.
func GetProcessExeName(pid int) string { return GetProcessName(pid) }

// IsHarnessProcessAt is the Windows twin of the Unix predicate: it reports
// whether the process at pid is a coding-harness runtime. See the Unix
// implementation for why the executable, not the thread name, is the
// authority there.
func IsHarnessProcessAt(pid int, name string) bool {
	if IsHarnessProcessName(name) {
		return true
	}
	exe := GetProcessExeName(pid)
	return exe != "" && exe != name && IsHarnessProcessName(exe)
}

// FindClaudePID walks up the process tree from the current process to find
// the coding-harness ancestor — a parent named "claude"/"node" (Claude
// Code runs as node) or any other registered harness binary (e.g.
// "codex"). Returns its PID, or 0 if none is found. The harness-aware match
// (IsHarnessProcessAt) is what lets a Codex hook callback record a real
// PID instead of 0 (JOH-160).
func FindClaudePID() int {
	pid := os.Getppid()
	// Windows uses PID 0 for System Idle Process
	for pid > 0 {
		if IsHarnessProcessAt(pid, GetProcessName(pid)) {
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
