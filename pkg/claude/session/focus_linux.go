//go:build linux

package session

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// findPowerShell returns the path to powershell.exe using the shared WSL utilities.
func findPowerShell() string {
	path := wsl.FindPowerShell()
	if path != "" {
		debugLog("Found PowerShell at: %s", path)
	} else {
		debugLog("PowerShell not found")
	}
	return path
}

// debugLog writes to the debug log file.
// Always logs focus operations since they're hard to debug otherwise.
func debugLog(format string, args ...any) {
	f, err := os.OpenFile(DebugLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[focus] "+format+"\n", args...)
}

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
func TryFocusAttachedSession(tmuxSession string) {
	debugLog("TryFocusAttachedSession called for: %s", tmuxSession)

	if isWSL() {
		debugLog("WSL detected, using Windows focus")
		focusWSLWindow()
		return
	}

	// Native Linux - use xdotool
	debugLog("Native Linux, using xdotool")
	focusLinuxTmuxSession(tmuxSession)
}

// FocusOwnWindow attempts to focus the current process's terminal window.
func FocusOwnWindow() bool {
	debugLog("FocusOwnWindow called")

	if isWSL() {
		debugLog("Detected WSL environment")
		return focusWSLWindow()
	}

	debugLog("Native Linux, using xdotool")
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
	// Get our PID and walk up to find terminal
	pid := os.Getpid()
	debugLog("Current PID: %d", pid)

	// Walk up the process tree looking for the init process (PID 1's parent on WSL is the Windows side)
	terminalPID := findTerminalPID(pid)
	debugLog("Found terminal PID: %d", terminalPID)

	if terminalPID == 0 {
		debugLog("No terminal PID found, trying fallback to focus any terminal")
		// Fallback: just try to focus any Windows Terminal or terminal window
		return focusAnyTerminal()
	}

	return focusWindowByPID(terminalPID)
}

// findTerminalPID walks up the process tree to find the terminal's Windows PID.
// Returns 0 if not found.
func findTerminalPID(pid int) int {
	debugLog("Walking process tree from PID %d", pid)

	// In WSL, we can try to get the Windows PID from the init process
	// The WSL init process (PID 1) is spawned by the Windows side

	// Walk up to PID 1 (init), then use PowerShell to find its Windows parent
	current := pid
	for current > 1 {
		ppid := getParentPID(current)
		debugLog("  PID %d -> parent %d", current, ppid)
		if ppid <= 0 {
			break
		}
		current = ppid
	}

	debugLog("Reached PID %d, now querying Windows side", current)

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
	debugLog("Querying PowerShell for WSL host process...")

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
		debugLog("PowerShell not found")
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
				debugLog("PowerShell: %s", parts[1])
			}
			if pid > 0 {
				debugLog("PowerShell returned host PID: %d", pid)
				return pid
			}
		} else if line != "" {
			debugLog("PowerShell trace: %s", line)
		}
	}

	if err != nil {
		debugLog("PowerShell error: %v", err)
	}
	return 0
}

// focusWTTab tries to focus a Windows Terminal tab by title.
// Returns true if successfully focused a tab with our session ID.
func focusWTTab(sessionID string) bool {
	debugLog("Trying to focus Windows Terminal tab for session: %s", sessionID)
	// This is now handled by focusWindowByTitlePattern and focusWTTabByCycling
	return false
}

// focusWTTabByCycling opens a new Windows Terminal window and attaches to the session.
// This is more reliable than trying to find/focus existing tabs.
func focusWTTabByCycling(sessionID string) bool {
	debugLog("Fallback: opening new Windows Terminal window to attach to session %s", sessionID)

	psPath := findPowerShell()
	if psPath == "" {
		debugLog("PowerShell not found")
		return false
	}

	// Open new Windows Terminal window with wsl running attach command
	// Use -f (force) to detach from any existing attachment
	// Syntax: wt.exe -w -1 wsl -- bash -c "command" (-w -1 = new window)
	script := fmt.Sprintf(`
wt.exe -w -1 wsl -- bash -lc "tclaude session attach -f %s"
Write-Output "True"
`, sessionID)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	debugLog("Open new WT window result: %s", outStr)

	if err != nil {
		debugLog("Failed to open new WT window: %v", err)
		return false
	}

	return outStr == "True"
}

// focusWindowByPID focuses a window by its Windows process ID.
// This is now a fallback - we prefer searching ALL windows by title pattern.
func focusWindowByPID(pid int) bool {
	debugLog("Attempting to focus window for PID %d", pid)

	// First try to find window by our known title pattern (set by setTerminalTitle)
	sessionID := os.Getenv("TOFU_SESSION_ID")
	if sessionID != "" {
		debugLog("Searching ALL windows for title containing 'tofu:%s'", sessionID)
		if focusWindowByTitlePattern(sessionID) {
			return true
		}
	}

	debugLog("Title pattern search failed, trying tab cycling")
	if focusWTTabByCycling(sessionID) {
		return true
	}

	debugLog("Tab cycling failed, trying PID-based enumeration")
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
		debugLog("PowerShell not found")
		return false
	}

	// Search ALL visible windows for our title pattern "tofu:<sessionID>"
	script := fmt.Sprintf(`
$sessionId = '%s'
$pattern = "tofu:$sessionId"

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
	debugLog("Title pattern search output:\n%s", outStr)

	if err != nil {
		debugLog("Title pattern search error: %v", err)
	}

	// Parse output
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MATCH|") {
			matchTitle := strings.TrimPrefix(line, "MATCH|")
			debugLog("Found matching window: %s", matchTitle)
			return focusByTitle(matchTitle)
		}
	}

	return false
}

// focusWindowByPIDEnumeration is the fallback that enumerates windows for a specific PID.
func focusWindowByPIDEnumeration(pid int, sessionID string) bool {
	psPath := findPowerShell()
	if psPath == "" {
		debugLog("PowerShell not found")
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
    if ($title -match $sessionId -or $title -match 'TOFU_SESSION_ID' -or $title -like "*tofu:*") {
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
	debugLog("Window enumeration output:\n%s", outStr)

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
		debugLog("Focusing matched window: %s", matchTitle)
		return focusByTitle(matchTitle)
	}

	// No exact match - log all titles found
	debugLog("No matching window found. Available windows: %v", allTitles)

	if err != nil {
		debugLog("Enumeration error: %v", err)
	}

	return false
}

// focusByTitle focuses a window using AppActivate by title
func focusByTitle(title string) bool {
	debugLog("Focusing by title: %s", title)

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
		debugLog("AppActivate output: %s", strings.TrimSpace(string(output)))
	}
	if err != nil {
		debugLog("AppActivate failed: %v", err)
		return false
	}
	debugLog("AppActivate succeeded")
	return true
}

// focusAnyTerminal tries to focus Windows Terminal or other common terminals.
func focusAnyTerminal() bool {
	debugLog("Trying to focus any terminal window...")

	// Try common terminal process names
	terminals := []string{"WindowsTerminal", "cmd", "powershell", "pwsh", "ConEmu64", "ConEmu"}
	debugLog("Looking for: %v", terminals)

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
		debugLog("PowerShell not found")
		return false
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		debugLog("PowerShell output: %s", strings.TrimSpace(string(output)))
	}
	if err != nil {
		debugLog("Focus any terminal failed: %v", err)
		return false
	}
	debugLog("Focus any terminal succeeded")
	return true
}

// =============================================================================
// Native Linux Focus Support (using xdotool)
// =============================================================================

// focusLinuxTmuxSession focuses the terminal window running a specific tmux session.
func focusLinuxTmuxSession(tmuxSession string) bool {
	// Check if xdotool is available
	if _, err := exec.LookPath("xdotool"); err != nil {
		debugLog("xdotool not found, cannot focus window")
		return false
	}

	// Get the client TTY for this tmux session
	cmd := exec.Command("tmux", "list-clients", "-t", tmuxSession, "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		debugLog("Failed to get tmux client tty: %v", err)
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		debugLog("No clients attached to tmux session")
		return false
	}
	tty := lines[0]
	debugLog("Found client TTY: %s", tty)

	// Find the terminal window by walking the process tree from TTY
	return focusLinuxWindowByTTY(tty)
}

// focusLinuxCurrentWindow focuses the current terminal window.
func focusLinuxCurrentWindow() bool {
	// Check if xdotool is available
	if _, err := exec.LookPath("xdotool"); err != nil {
		debugLog("xdotool not found, cannot focus window")
		return false
	}

	// Try to get the active window from the current TTY
	tty, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		debugLog("Failed to get current TTY: %v", err)
		return false
	}
	debugLog("Current TTY: %s", tty)

	return focusLinuxWindowByTTY(tty)
}

// focusLinuxWindowByTTY finds and focuses the terminal window owning a TTY.
func focusLinuxWindowByTTY(tty string) bool {
	// Find processes on this TTY
	cmd := exec.Command("lsof", "-t", tty)
	output, err := cmd.Output()
	if err != nil {
		debugLog("lsof failed for TTY %s: %v", tty, err)
		// Fallback: try to focus by window name pattern
		return focusLinuxWindowByPattern("tofu:")
	}

	pids := strings.Fields(string(output))
	debugLog("Found PIDs on TTY: %v", pids)

	// Walk up process tree to find terminal
	for _, pidStr := range pids {
		if windowID := findLinuxWindowForPID(pidStr); windowID != "" {
			return focusLinuxWindowByID(windowID)
		}
	}

	// Fallback: try by window name
	return focusLinuxWindowByPattern("tofu:")
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
				debugLog("Found window %s for PID %s", windows[0], current)
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
	debugLog("Focusing window ID: %s", windowID)

	// Activate the window
	cmd := exec.Command("xdotool", "windowactivate", "--sync", windowID)
	if err := cmd.Run(); err != nil {
		debugLog("xdotool windowactivate failed: %v", err)
		return false
	}

	debugLog("Successfully focused window")
	return true
}

// focusLinuxWindowByPattern searches for and focuses a window by name pattern.
func focusLinuxWindowByPattern(pattern string) bool {
	debugLog("Searching for window with pattern: %s", pattern)

	// Search for windows matching the pattern
	cmd := exec.Command("xdotool", "search", "--name", pattern)
	output, err := cmd.Output()
	if err != nil {
		debugLog("xdotool search failed: %v", err)
		return false
	}

	windows := strings.Fields(string(output))
	if len(windows) == 0 {
		debugLog("No windows found matching pattern")
		return false
	}

	debugLog("Found %d windows, focusing first: %s", len(windows), windows[0])
	return focusLinuxWindowByID(windows[0])
}

// IsXdotoolInstalled checks if xdotool is available.
func IsXdotoolInstalled() bool {
	_, err := exec.LookPath("xdotool")
	return err == nil
}
