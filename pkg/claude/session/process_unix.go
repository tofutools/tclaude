//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// procExeLink reads the /proc/<pid>/exe link. Indirected through a package
// var so a test can force the unreadable branch — the case that decides
// whether this function may answer with weaker evidence than it promises,
// and one no test can stage against a real process portably.
var procExeLink = func(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}

// GetProcessExeName returns the basename of the executable a process is
// running, or "" when it cannot be read.
//
// This is deliberately NOT GetProcessName: on Linux that reads
// /proc/<pid>/comm, which is the name of the process's MAIN THREAD — a
// value the program itself can overwrite (and which the kernel truncates to
// 15 bytes). GitHub Copilot CLI does exactly that: its Node SEA renames its
// main thread, so a running `copilot` binary reports comm "MainThread" and
// the npm loader that spawned it reports "node-MainThread" (measured on
// 1.0.78, TCL-1049). /proc/<pid>/exe is a kernel-maintained link to the
// binary actually being executed; a process cannot choose it, so matching on
// it is strictly stronger evidence of what a process IS than comm or argv[0].
//
// On platforms without /proc (macOS) this falls back to `ps -o comm=`, which
// there prints the executable path — the same identity, which is why the
// harness walks below have always worked on macOS and only ever missed
// Copilot on Linux.
//
// On Linux there is NO fallback: an unreadable /proc/<pid>/exe yields "".
// `ps` there prints comm, the very value this function exists to be stronger
// than — and a process can force that path deliberately, since
// prctl(PR_SET_DUMPABLE, 0) makes its own exe link unreadable. Answering with
// comm would let a caller choose what this function returns, quietly voiding
// the contract above for anyone who relies on it. No evidence is the honest
// answer; callers degrade to the name check they would have used anyway.
func GetProcessExeName(pid int) string {
	if target, err := procExeLink(pid); err == nil {
		// A replaced binary reads back as "/path/to/exe (deleted)".
		target = strings.TrimSuffix(strings.TrimSpace(target), " (deleted)")
		if target == "" {
			return ""
		}
		return filepath.Base(target)
	}
	if runtime.GOOS == "linux" {
		return ""
	}
	cmd := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(output))
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}

// IsHarnessProcessAt reports whether the process at pid is a coding-harness
// runtime, given the name a caller already read for it (pass "" to skip the
// name check). It is the pid-aware companion to IsHarnessProcessName: the
// name is tried first because callers already have it, and the executable is
// consulted only when the name misses.
//
// Every process-tree walk that looks for a harness ancestor must use this
// rather than the bare name predicate — a harness that renames its main
// thread is otherwise invisible to the walk, which is what cut Copilot panes
// off from agentd's caller identity entirely (TCL-1049).
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
// PID instead of 0 (JOH-160); a non-tmux row at PID 0 is otherwise reaped
// as a false-positive.
func FindClaudePID() int {
	pid := os.Getppid()
	for pid > 1 {
		if IsHarnessProcessAt(pid, GetProcessName(pid)) {
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
