//go:build linux

package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// findPowerShell returns the path to powershell.exe using the shared WSL utilities.
func findPowerShell() string {
	path := wsl.FindPowerShell()
	if path != "" {
		slog.Debug(fmt.Sprintf("Found PowerShell at: %s", path), "module", "focus")
	} else {
		slog.Debug("PowerShell not found", "module", "focus")
	}
	return path
}

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
func TryFocusAttachedSession(tmuxSession string) {
	slog.Debug(fmt.Sprintf("TryFocusAttachedSession called for: %s", tmuxSession), "module", "focus")

	if isWSL() {
		slog.Debug("WSL detected, using Windows focus", "module", "focus")
		focusWSLWindow()
		return
	}

	// Native Linux - use xdotool
	slog.Debug("Native Linux, using xdotool", "module", "focus")
	focusLinuxTmuxSession(tmuxSession)
}

// FocusOwnWindow attempts to focus the current process's terminal window.
func FocusOwnWindow() bool {
	slog.Debug("FocusOwnWindow called", "module", "focus")

	if isWSL() {
		slog.Debug("Detected WSL environment", "module", "focus")
		return focusWSLWindow()
	}

	slog.Debug("Native Linux, using xdotool", "module", "focus")
	return focusLinuxCurrentWindow()
}

// GetOwnWindowTitle returns the title of the current terminal window.
func GetOwnWindowTitle() string {
	return ""
}

// isWSL detects if we're running in Windows Subsystem for Linux.
func isWSL() bool {
	return wsl.IsWSL()
}

// focusWSLWindow attempts to focus the terminal window hosting this WSL session.
// It walks up the process tree to find the Windows terminal process and focuses it.
func focusWSLWindow() bool {
	// Get our PID and walk up to find the terminal
	pid := os.Getpid()
	slog.Debug(fmt.Sprintf("Current PID: %d", pid), "module", "focus")

	// Walk up the process tree looking for the init process (PID 1's parent on WSL is the Windows side)
	terminalPID := findTerminalPID(pid)
	slog.Debug(fmt.Sprintf("Found terminal PID: %d", terminalPID), "module", "focus")

	if terminalPID == 0 {
		slog.Debug("No terminal PID found, trying fallback to focus any terminal", "module", "focus")
		// Fallback: just try to focus any Windows Terminal or terminal window
		return focusAnyTerminal()
	}

	return focusWindowByPID(terminalPID)
}

// findTerminalPID walks up the process tree to find the terminal's Windows PID.
// Returns 0 if not found.
func findTerminalPID(pid int) int {
	slog.Debug(fmt.Sprintf("Walking process tree from PID %d", pid), "module", "focus")

	// In WSL, we can try to get the Windows PID from the init process
	// The WSL init process (PID 1) is spawned by the Windows side

	// Walk up to PID 1 (init), then use PowerShell to find its Windows parent
	current := pid
	for current > 1 {
		ppid := getParentPID(current)
		slog.Debug(fmt.Sprintf("  PID %d -> parent %d", current, ppid), "module", "focus")
		if ppid <= 0 {
			break
		}
		current = ppid
	}

	slog.Debug(fmt.Sprintf("Reached PID %d, now querying Windows side", current), "module", "focus")

	// Now use PowerShell to find the Windows process hosting WSL
	// We look for the wsl.exe or WindowsTerminal.exe process
	return getWSLHostPID()
}

// getParentPID returns the parent PID of a process.
func getParentPID(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ppid, _ := strconv.Atoi(parts[1])
				return ppid
			}
		}
	}
	return 0
}

// getWSLHostPID uses PowerShell to find the Windows process hosting this WSL instance.
// It walks up the process tree from wsl.exe until it finds a process with a window handle.
func getWSLHostPID() int {
	slog.Debug("Querying PowerShell for WSL host process...", "module", "focus")

	// Walk up from wsl.exe until we find a process with a main window
	script := `
$wslProcesses = Get-Process -Name wsl -ErrorAction SilentlyContinue
if (-not $wslProcesses) {
    Write-Output "0|No wsl.exe found"
    exit
}

$wsl = $wslProcesses | Select-Object -First 1
$currentPid = $wsl.Id
$visited = @{}

# Walk up the process tree looking for a window
for ($i = 0; $i -lt 20; $i++) {
    if ($visited[$currentPid]) { break }
    $visited[$currentPid] = $true

    $proc = Get-Process -Id $currentPid -ErrorAction SilentlyContinue
    if ($proc -and $proc.MainWindowHandle -ne [IntPtr]::Zero) {
        Write-Output "$currentPid|Found window: $($proc.ProcessName)"
        exit
    }

    # Get parent
    try {
        $parentPid = (Get-CimInstance Win32_Process -Filter "ProcessId=$currentPid" -ErrorAction SilentlyContinue).ParentProcessId
        if (-not $parentPid -or $parentPid -eq 0) { break }
        Write-Host "  $($proc.ProcessName) ($currentPid) -> parent $parentPid"
        $currentPid = $parentPid
    } catch {
        break
    }
}
Write-Output "0|No window found in process tree"
`
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return 0
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))

	// Parse output - format is "PID|message"
	lines := strings.Split(outStr, "\n")
	for _, line := range lines {
		if strings.Contains(line, "|") {
			parts := strings.SplitN(line, "|", 2)
			pid, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			if len(parts) > 1 {
				slog.Debug(fmt.Sprintf("PowerShell: %s", parts[1]), "module", "focus")
			}
			if pid > 0 {
				slog.Debug(fmt.Sprintf("PowerShell returned host PID: %d", pid), "module", "focus")
				return pid
			}
		} else if line != "" {
			slog.Debug(fmt.Sprintf("PowerShell trace: %s", line), "module", "focus")
		}
	}

	if err != nil {
		slog.Debug(fmt.Sprintf("PowerShell error: %v", err), "module", "focus")
	}
	return 0
}

// focusWTTab tries to focus a Windows Terminal tab by title.
// Returns true if successfully focused a tab with our session ID.
func focusWTTab(sessionID string) bool {
	slog.Debug(fmt.Sprintf("Trying to focus Windows Terminal tab for session: %s", sessionID), "module", "focus")
	// This is now handled by focusWindowByTitlePattern and focusWTTabByCycling
	return false
}

// focusWTTabByCycling opens a new Windows Terminal window and attaches to the session.
// This is more reliable than trying to find/focus existing tabs.
func focusWTTabByCycling(sessionID string) bool {
	slog.Debug(fmt.Sprintf("Fallback: opening new Windows Terminal window to attach to session %s", sessionID), "module", "focus")

	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Open a new Windows Terminal window with wsl running attach command
	// Use -f (force) to detach from any existing attachment
	// Syntax: wt.exe -w -1 wsl -- bash -c "command" (-w -1 = new window)
	script := fmt.Sprintf(`
wt.exe -w -1 wsl -- bash -lc "tclaude session attach -f %s"
Write-Output "True"
`, sessionID)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Open new WT window result: %s", outStr), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to open new WT window: %v", err), "module", "focus")
		return false
	}

	return outStr == "True"
}

// focusWindowByPID focuses a window by its Windows process ID.
// This is now a fallback - we prefer searching ALL windows by title pattern.
func focusWindowByPID(pid int) bool {
	slog.Debug(fmt.Sprintf("Attempting to focus window for PID %d", pid), "module", "focus")

	// First try to find window by our known title pattern (set by setTerminalTitle)
	sessionID := os.Getenv("TCLAUDE_SESSION_ID")
	if sessionID != "" {
		slog.Debug(fmt.Sprintf("Searching ALL windows for title containing 'tclaude:%s'", sessionID), "module", "focus")
		if focusWindowByTitlePattern(sessionID) {
			return true
		}
	}

	slog.Debug("Title pattern search failed, trying tab cycling", "module", "focus")
	if focusWTTabByCycling(sessionID) {
		return true
	}

	slog.Debug("Tab cycling failed, trying PID-based enumeration", "module", "focus")
	return focusWindowByPIDEnumeration(pid, sessionID)
}

// focusWindowByTitlePattern searches ALL visible windows for one matching our title pattern.
// This is more reliable than PID-based search because Windows Terminal tabs all share one process.
func focusWindowByTitlePattern(sessionID string) bool {
	// First try Windows Terminal's native tab focusing via wt.exe
	if focusWTTab(sessionID) {
		return true
	}

	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Search ALL visible windows for our title pattern "tclaude:<sessionID>"
	script := fmt.Sprintf(`
$sessionId = '%s'
$pattern = "tclaude:$sessionId"

Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public class AllWindowEnumerator {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hWnd);

    public static List<string> GetAllVisibleWindowTitles() {
        var titles = new List<string>();
        EnumWindows((hWnd, lParam) => {
            if (IsWindowVisible(hWnd)) {
                StringBuilder sb = new StringBuilder(512);
                GetWindowText(hWnd, sb, 512);
                string title = sb.ToString();
                if (!string.IsNullOrEmpty(title)) {
                    titles.Add(title);
                }
            }
            return true;
        }, IntPtr.Zero);
        return titles;
    }
}
"@

$titles = [AllWindowEnumerator]::GetAllVisibleWindowTitles()
Write-Host "Searching $($titles.Count) visible windows for pattern: $pattern"

foreach ($title in $titles) {
    Write-Host "  Checking: $title"
    if ($title -like "*$pattern*") {
        Write-Host "FOUND MATCH!"
        Write-Output "MATCH|$title"
        exit 0
    }
}

Write-Host "No matching window found"
Write-Output "NOMATCH"
`, sessionID)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Title pattern search output:\n%s", outStr), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Title pattern search error: %v", err), "module", "focus")
	}

	// Parse output
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MATCH|") {
			matchTitle := strings.TrimPrefix(line, "MATCH|")
			slog.Debug(fmt.Sprintf("Found matching window: %s", matchTitle), "module", "focus")
			return focusByTitle(matchTitle)
		}
	}

	return false
}

// focusWindowByPIDEnumeration is the fallback that enumerates windows for a specific PID.
func focusWindowByPIDEnumeration(pid int, sessionID string) bool {
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Enumerate all windows for this process and find one with our session ID in title
	script := fmt.Sprintf(`
$targetPid = %d
$sessionId = '%s'

Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public class WindowEnumerator {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint lpdwProcessId);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hWnd);

    public static List<Tuple<IntPtr, string>> GetWindowsForProcess(uint targetPid) {
        var windows = new List<Tuple<IntPtr, string>>();
        EnumWindows((hWnd, lParam) => {
            uint pid;
            GetWindowThreadProcessId(hWnd, out pid);
            if (pid == targetPid && IsWindowVisible(hWnd)) {
                StringBuilder sb = new StringBuilder(512);
                GetWindowText(hWnd, sb, 512);
                string title = sb.ToString();
                if (!string.IsNullOrEmpty(title)) {
                    windows.Add(Tuple.Create(hWnd, title));
                }
            }
            return true;
        }, IntPtr.Zero);
        return windows;
    }
}
"@

$windows = [WindowEnumerator]::GetWindowsForProcess(%d)
Write-Host "Found $($windows.Count) windows for PID %d"

foreach ($w in $windows) {
    $handle = $w.Item1
    $title = $w.Item2
    Write-Host "  Window: $title (handle: $handle)"

    # Check if this window's title contains our session ID
    if ($title -match $sessionId -or $title -match 'TCLAUDE_SESSION_ID' -or $title -like "*tclaude:*") {
        Write-Host "Found matching window!"
        Write-Output "MATCH|$title"
        exit 0
    }
}

# If no exact match, output all titles for debugging
foreach ($w in $windows) {
    Write-Output "WINDOW|$($w.Item2)"
}
`, pid, sessionID, pid, pid)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Window enumeration output:\n%s", outStr), "module", "focus")

	// Parse output to find matching window title
	var matchTitle string
	var allTitles []string
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MATCH|") {
			matchTitle = strings.TrimPrefix(line, "MATCH|")
			break
		} else if strings.HasPrefix(line, "WINDOW|") {
			allTitles = append(allTitles, strings.TrimPrefix(line, "WINDOW|"))
		}
	}

	if matchTitle != "" {
		slog.Debug(fmt.Sprintf("Focusing matched window: %s", matchTitle), "module", "focus")
		return focusByTitle(matchTitle)
	}

	// No exact match - log all titles found
	slog.Debug(fmt.Sprintf("No matching window found. Available windows: %v", allTitles), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Enumeration error: %v", err), "module", "focus")
	}

	return false
}

// focusByTitle focuses a window using AppActivate by title
func focusByTitle(title string) bool {
	slog.Debug(fmt.Sprintf("Focusing by title: %s", title), "module", "focus")

	// Escape quotes in title
	escapedTitle := strings.ReplaceAll(title, "'", "''")

	script := fmt.Sprintf(`
$title = '%s'
$wshell = New-Object -ComObject wscript.shell
$wshell.AppActivate($title)
`, escapedTitle)

	psPath := findPowerShell()
	if psPath == "" {
		return false
	}

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		slog.Debug(fmt.Sprintf("AppActivate output: %s", strings.TrimSpace(string(output))), "module", "focus")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("AppActivate failed: %v", err), "module", "focus")
		return false
	}
	slog.Debug("AppActivate succeeded", "module", "focus")
	return true
}

// focusAnyTerminal tries to focus Windows Terminal or other common terminals.
func focusAnyTerminal() bool {
	slog.Debug("Trying to focus any terminal window...", "module", "focus")

	// Try common terminal process names
	terminals := []string{"WindowsTerminal", "cmd", "powershell", "pwsh", "ConEmu64", "ConEmu"}
	slog.Debug(fmt.Sprintf("Looking for: %v", terminals), "module", "focus")

	script := fmt.Sprintf(`
$ProcessNames = "%s"
Add-Type @"
using System;
using System.Runtime.InteropServices;
public class WindowHelper {
    [DllImport("user32.dll")]
    public static extern bool SetForegroundWindow(IntPtr hWnd);
    [DllImport("user32.dll")]
    public static extern bool ShowWindow(IntPtr hWnd, int nCmdShow);
    [DllImport("user32.dll")]
    public static extern bool IsIconic(IntPtr hWnd);

    public const int SW_RESTORE = 9;

    public static bool FocusWindow(IntPtr hWnd) {
        if (IsIconic(hWnd)) {
            ShowWindow(hWnd, SW_RESTORE);
        }
        return SetForegroundWindow(hWnd);
    }
}
"@

$names = $ProcessNames -split ','
foreach ($name in $names) {
    $procs = Get-Process -Name $name -ErrorAction SilentlyContinue
    Write-Host "Checking $name : found $($procs.Count) processes"
    foreach ($proc in $procs) {
        if ($proc.MainWindowHandle -ne [IntPtr]::Zero) {
            Write-Host "Focusing $($proc.ProcessName) PID $($proc.Id) handle $($proc.MainWindowHandle)"
            $result = [WindowHelper]::FocusWindow($proc.MainWindowHandle)
            Write-Host "SetForegroundWindow returned: $result"
            exit 0
        }
    }
}
Write-Host "No terminal with window handle found"
exit 1
`, strings.Join(terminals, ","))
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		slog.Debug(fmt.Sprintf("PowerShell output: %s", strings.TrimSpace(string(output))), "module", "focus")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Focus any terminal failed: %v", err), "module", "focus")
		return false
	}
	slog.Debug("Focus any terminal succeeded", "module", "focus")
	return true
}

// =============================================================================
// Native Linux Focus Support (using xdotool)
// =============================================================================

// focusLinuxTmuxSession focuses the terminal window running a specific tmux session.
func focusLinuxTmuxSession(tmuxSession string) bool {
	// Check if xdotool is available
	if _, err := exec.LookPath("xdotool"); err != nil {
		slog.Debug("xdotool not found, cannot focus window", "module", "focus")
		return false
	}

	// Get the client TTY for this tmux session
	cmd := clcommon.TmuxCommand("list-clients", "-t", tmuxSession, "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to get tmux client tty: %v", err), "module", "focus")
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		slog.Debug("No clients attached to tmux session", "module", "focus")
		return false
	}
	tty := lines[0]
	slog.Debug(fmt.Sprintf("Found client TTY: %s", tty), "module", "focus")

	// Find the terminal window by walking the process tree from TTY
	return focusLinuxWindowByTTY(tty)
}

// focusLinuxCurrentWindow focuses the current terminal window.
func focusLinuxCurrentWindow() bool {
	// Check if xdotool is available
	if _, err := exec.LookPath("xdotool"); err != nil {
		slog.Debug("xdotool not found, cannot focus window", "module", "focus")
		return false
	}

	// Try to get the active window from the current TTY
	tty, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to get current TTY: %v", err), "module", "focus")
		return false
	}
	slog.Debug(fmt.Sprintf("Current TTY: %s", tty), "module", "focus")

	return focusLinuxWindowByTTY(tty)
}

// focusLinuxWindowByTTY finds and focuses the terminal window owning a TTY.
func focusLinuxWindowByTTY(tty string) bool {
	// Find processes on this TTY
	cmd := exec.Command("lsof", "-t", tty)
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("lsof failed for TTY %s: %v", tty, err), "module", "focus")
		// Fallback: try to focus by window name pattern
		return focusLinuxWindowByPattern("tclaude:")
	}

	pids := strings.Fields(string(output))
	slog.Debug(fmt.Sprintf("Found PIDs on TTY: %v", pids), "module", "focus")

	// Walk up process tree to find terminal
	for _, pidStr := range pids {
		if windowID := findLinuxWindowForPID(pidStr); windowID != "" {
			return focusLinuxWindowByID(windowID)
		}
	}

	// Fallback: try by window name
	return focusLinuxWindowByPattern("tclaude:")
}

// findLinuxWindowForPID walks up the process tree to find a window.
func findLinuxWindowForPID(pidStr string) string {
	visited := make(map[string]bool)
	current := pidStr

	for current != "" && current != "0" && current != "1" && !visited[current] {
		visited[current] = true

		// Try to find X window for this PID
		cmd := exec.Command("xdotool", "search", "--pid", current)
		output, err := cmd.Output()
		if err == nil {
			windows := strings.Fields(string(output))
			if len(windows) > 0 {
				slog.Debug(fmt.Sprintf("Found window %s for PID %s", windows[0], current), "module", "focus")
				return windows[0]
			}
		}

		// Get parent PID
		ppidData, err := os.ReadFile("/proc/" + current + "/status")
		if err != nil {
			break
		}
		for _, line := range strings.Split(string(ppidData), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					current = parts[1]
					break
				}
			}
		}
	}

	return ""
}

// focusLinuxWindowByID focuses a window by its X window ID.
func focusLinuxWindowByID(windowID string) bool {
	slog.Debug(fmt.Sprintf("Focusing window ID: %s", windowID), "module", "focus")

	// Activate the window
	cmd := exec.Command("xdotool", "windowactivate", "--sync", windowID)
	if err := cmd.Run(); err != nil {
		slog.Debug(fmt.Sprintf("xdotool windowactivate failed: %v", err), "module", "focus")
		return false
	}

	slog.Debug("Successfully focused window", "module", "focus")
	return true
}

// focusLinuxWindowByPattern searches for and focuses a window by name pattern.
func focusLinuxWindowByPattern(pattern string) bool {
	slog.Debug(fmt.Sprintf("Searching for window with pattern: %s", pattern), "module", "focus")

	// Search for windows matching the pattern
	cmd := exec.Command("xdotool", "search", "--name", pattern)
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("xdotool search failed: %v", err), "module", "focus")
		return false
	}

	windows := strings.Fields(string(output))
	if len(windows) == 0 {
		slog.Debug("No windows found matching pattern", "module", "focus")
		return false
	}

	slog.Debug(fmt.Sprintf("Found %d windows, focusing first: %s", len(windows), windows[0]), "module", "focus")
	return focusLinuxWindowByID(windows[0])
}

// IsXdotoolInstalled checks if xdotool is available.
func IsXdotoolInstalled() bool {
	_, err := exec.LookPath("xdotool")
	return err == nil
}
